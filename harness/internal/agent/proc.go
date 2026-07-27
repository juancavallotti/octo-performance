package agent

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// userHZ is the unit of the CPU counters in /proc/[pid]/stat.
//
// It has been 100 on every Linux the kernel ships for x86-64 and arm64, and the GCP
// images this lab runs on are stock. Reading it properly needs sysconf, which needs
// cgo, which would make the agent harder to cross-compile than the fact is worth. It
// is recorded in Static.ClockTick so a reader can tell what was assumed.
const userHZ = 100

// ProcFS samples through /proc. This is the full-fidelity source and the only one that
// can answer a question about context switches or open descriptors.
type ProcFS struct {
	// Root is the mount point, "/proc" in production and a fixture directory in
	// tests. Every path in this file is built from it, so nothing here needs a real
	// kernel to be exercised.
	Root string
}

// NewProcFS returns a source reading the live /proc.
func NewProcFS() *ProcFS { return &ProcFS{Root: "/proc"} }

// Available reports whether this machine has a usable /proc. A macOS laptop does not,
// which is a fact about the local topology rather than an error.
func (p *ProcFS) Available() bool {
	root := p.Root
	if root == "" {
		root = "/proc"
	}
	_, err := os.Stat(filepath.Join(root, "stat"))
	return err == nil
}

// Name implements Source.
func (*ProcFS) Name() string { return "procfs" }

// Fidelity implements Source.
func (*ProcFS) Fidelity() Fidelity { return Full }

func (p *ProcFS) path(parts ...string) string {
	root := p.Root
	if root == "" {
		root = "/proc"
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

// Static implements Source.
func (p *ProcFS) Static() (Static, error) {
	st := Static{
		Cores:     runtimeCores(),
		PageSize:  os.Getpagesize(),
		ClockTick: userHZ,
	}
	st.Hostname, _ = os.Hostname()

	if b, err := os.ReadFile(p.path("cpuinfo")); err == nil {
		st.CPUModel = firstFieldValue(b, "model name")
		if st.CPUModel == "" {
			st.CPUModel = firstFieldValue(b, "Model") // arm64 reports it differently
		}
		if n := countPrefix(b, "processor"); n > 0 {
			st.Cores = n
		}
	}
	if b, err := os.ReadFile(p.path("sys", "kernel", "osrelease")); err == nil {
		st.Kernel = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(p.path("uptime")); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if up, err := strconv.ParseFloat(f[0], 64); err == nil {
				st.BootTime = time.Now().Add(-time.Duration(up * float64(time.Second)))
			}
		}
	}
	return st, nil
}

// procSnapshot is the raw bytes one sample is built from.
//
// It exists so the local source and the remote one share a single parse path. The
// alternative — a second implementation reading the same files over SSH — is two
// parsers that agree until one of them is fixed, and a subject sampled remotely
// would then produce numbers that quietly disagree with one sampled locally.
type procSnapshot struct {
	pidStat   []byte
	pidStatus []byte
	hostStat  []byte
	loadavg   []byte
	// openFDs is a count rather than a listing: the remote reader has no cheap way to
	// enumerate a directory, and only the count is ever used.
	openFDs int
	// pageSize converts the stat file's RSS, which is in pages. The subject's page
	// size, not the harness's — they are not the same machine.
	pageSize int
}

// sampleFrom turns one snapshot into a Sample. Pure: no clock beyond the stamp it is
// given, no filesystem, no network.
func sampleFrom(snap procSnapshot, at time.Time) (Sample, error) {
	s := Sample{T: at}

	ps, err := parsePIDStat(snap.pidStat)
	if err != nil {
		return Sample{}, err
	}
	s.UserSeconds = ps.userSeconds
	s.SysSeconds = ps.sysSeconds
	s.Threads = ps.threads
	s.RSSBytes = ps.rssPages * int64(snap.pageSize)

	// status carries the switch counters, and its RSS is authoritative where both
	// exist. Its absence is not fatal: a process that exits between the two reads
	// should still contribute the sample it did produce.
	if b := snap.pidStatus; len(b) > 0 {
		if v, ok := kbField(b, "VmRSS:"); ok {
			s.RSSBytes = v
		}
		if v, ok := intField(b, "voluntary_ctxt_switches:"); ok {
			s.VoluntaryCtxSwitches = v
		}
		if v, ok := intField(b, "nonvoluntary_ctxt_switches:"); ok {
			s.InvoluntaryCtxSwitches = v
		}
	}
	s.OpenFDs = snap.openFDs

	if b := snap.hostStat; len(b) > 0 {
		if busy, idle, err := parseHostCPU(b); err == nil {
			s.HostBusySeconds, s.HostIdleSeconds = busy, idle
		}
	}
	if f := strings.Fields(string(snap.loadavg)); len(f) > 0 {
		s.Load1, _ = strconv.ParseFloat(f[0], 64)
	}
	return s, nil
}

// Sample implements Source.
func (p *ProcFS) Sample(pid int) (Sample, error) {
	at := time.Now()

	stat, err := os.ReadFile(p.path(strconv.Itoa(pid), "stat"))
	if err != nil {
		return Sample{}, fmt.Errorf("agent: reading pid %d stat: %w", pid, err)
	}
	snap := procSnapshot{pidStat: stat, pageSize: os.Getpagesize()}
	snap.pidStatus, _ = os.ReadFile(p.path(strconv.Itoa(pid), "status"))
	snap.hostStat, _ = os.ReadFile(p.path("stat"))
	snap.loadavg, _ = os.ReadFile(p.path("loadavg"))
	if entries, err := os.ReadDir(p.path(strconv.Itoa(pid), "fd")); err == nil {
		snap.openFDs = len(entries)
	}
	return sampleFrom(snap, at)
}

// pidStat is the subset of /proc/[pid]/stat the harness uses.
type pidStat struct {
	comm        string
	state       string
	userSeconds float64
	sysSeconds  float64
	threads     int
	rssPages    int64
	startTicks  int64
}

// parsePIDStat reads /proc/[pid]/stat.
//
// The second field is the executable name in parentheses and it may contain spaces
// and parentheses of its own, so the line cannot be split on whitespace. Everything
// after the LAST close paren is positional; everything before the first is the pid.
// Splitting naively works until a process is named something ordinary like
// "octo (dev)", and then it silently shifts every field by one.
func parsePIDStat(b []byte) (pidStat, error) {
	line := strings.TrimSpace(string(b))

	open := strings.IndexByte(line, '(')
	shut := strings.LastIndexByte(line, ')')
	if open < 0 || shut < 0 || shut < open {
		return pidStat{}, fmt.Errorf("agent: /proc/[pid]/stat has no comm field")
	}

	out := pidStat{comm: line[open+1 : shut]}

	// Fields after comm, numbered as the proc(5) man page does: rest[0] is field 3.
	rest := strings.Fields(line[shut+1:])
	const (
		fState    = 3
		fUTime    = 14
		fSTime    = 15
		fCUTime   = 16
		fCSTime   = 17
		fThreads  = 20
		fStartT   = 22
		fRSSPages = 24
	)
	at := func(n int) (string, bool) {
		i := n - fState
		if i < 0 || i >= len(rest) {
			return "", false
		}
		return rest[i], true
	}
	num := func(n int) int64 {
		s, ok := at(n)
		if !ok {
			return 0
		}
		v, _ := strconv.ParseInt(s, 10, 64)
		return v
	}

	if s, ok := at(fState); ok {
		out.state = s
	}
	if _, ok := at(fRSSPages); !ok {
		return pidStat{}, fmt.Errorf("agent: /proc/[pid]/stat has %d fields after comm, want at least %d",
			len(rest), fRSSPages-fState+1)
	}

	// Children are included because a runtime that forks a helper still costs the
	// machine what the helper costs. cutime/cstime only count reaped children, which
	// is the same thing wait4 reports and therefore consistent with Rusage.
	ticks := func(v int64) float64 { return float64(v) / userHZ }
	out.userSeconds = ticks(num(fUTime) + num(fCUTime))
	out.sysSeconds = ticks(num(fSTime) + num(fCSTime))
	out.threads = int(num(fThreads))
	out.rssPages = num(fRSSPages)
	out.startTicks = num(fStartT)
	return out, nil
}

// parseHostCPU reads the aggregate line of /proc/stat and returns cumulative busy and
// idle seconds across every core.
//
// idle and iowait are idle; everything else is the machine doing something. Steal
// counts as busy on purpose: on a shared cloud host, time stolen by the hypervisor is
// time this campaign did not get, and a runner that cannot keep up because of it has
// exactly the same effect on the numbers as one that is genuinely saturated.
func parseHostCPU(b []byte) (busy, idle float64, err error) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		// Only the first eight columns: user, nice, system, idle, iowait, irq,
		// softirq, steal. guest and guest_nice follow, and the kernel has already
		// counted them inside user and nice, so summing them again inflates the
		// total by however much virtualisation the host is doing.
		cols := f[1:]
		if len(cols) > 8 {
			cols = cols[:8]
		}
		var total, idleTicks float64
		for i, v := range cols {
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				continue
			}
			total += n
			if i == 3 || i == 4 { // idle, iowait
				idleTicks += n
			}
		}
		return (total - idleTicks) / userHZ, idleTicks / userHZ, nil
	}
	return 0, 0, fmt.Errorf("agent: /proc/stat has no aggregate cpu line")
}

func kbField(b []byte, key string) (int64, bool) {
	v, ok := intField(b, key)
	if !ok {
		return 0, false
	}
	return v * 1024, true // /proc/[pid]/status reports kB
}

func intField(b []byte, key string) (int64, bool) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, key) {
			continue
		}
		f := strings.Fields(strings.TrimPrefix(line, key))
		if len(f) == 0 {
			return 0, false
		}
		v, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

func firstFieldValue(b []byte, key string) string {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(name) == key {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func countPrefix(b []byte, prefix string) int {
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), prefix) {
			n++
		}
	}
	return n
}
