package agent

import (
	"fmt"
	"os"
	osexec "os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// runtimeCores is what this process can see, which on a container is not necessarily
// what the machine has. ProcFS.Static overrides it from /proc/cpuinfo where it can —
// the old lab published a container reporting 4 online CPUs against a 10-core host and
// the discrepancy appeared nowhere.
func runtimeCores() int { return runtime.NumCPU() }

// PS samples through ps(1). It is the fallback for a machine with no /proc — in
// practice a macOS laptop running a local campaign.
//
// It reports cumulative CPU and RSS and nothing else. That is a real limitation, not a
// gap to paper over: no thread count, no descriptor count, no context switches, and no
// machine-wide CPU total, so a cell sampled this way cannot support a claim about
// generator headroom. Fidelity says Coarse and the gates read it.
type PS struct{}

// NewPS returns the portable fallback source.
func NewPS() *PS { return &PS{} }

// Name implements Source.
func (*PS) Name() string { return "ps" }

// Fidelity implements Source.
func (*PS) Fidelity() Fidelity { return Coarse }

// Static implements Source.
func (*PS) Static() (Static, error) {
	st := Static{Cores: runtimeCores(), PageSize: os.Getpagesize()}
	st.Hostname, _ = os.Hostname()
	if out, err := osexec.Command("uname", "-r").Output(); err == nil {
		st.Kernel = strings.TrimSpace(string(out))
	}
	if out, err := osexec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
		st.CPUModel = strings.TrimSpace(string(out))
	}
	return st, nil
}

// Sample implements Source.
func (p *PS) Sample(pid int) (Sample, error) {
	out, err := osexec.Command("ps", "-o", "time=,rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return Sample{}, fmt.Errorf("agent: ps for pid %d: %w", pid, err)
	}
	cpu, rssKB, err := parsePSLine(string(out))
	if err != nil {
		return Sample{}, err
	}
	s := Sample{T: time.Now(), RSSBytes: rssKB * 1024}
	// ps reports one total, with no user/system split. Attributing all of it to user
	// would be a lie of a familiar kind, so it goes where it belongs: the sum is
	// correct and the split is absent.
	s.UserSeconds = cpu
	s.Load1 = loadOne()
	return s, nil
}

// loadOne reads the one-minute load average, or zero where it cannot.
func loadOne() float64 {
	out, err := osexec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return 0
	}
	f := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{} "))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

// parsePSLine reads one "time rss" row.
func parsePSLine(s string) (cpuSeconds float64, rssKB int64, err error) {
	f := strings.Fields(strings.TrimSpace(s))
	if len(f) < 2 {
		return 0, 0, fmt.Errorf("agent: ps produced no row for the process")
	}
	cpuSeconds, err = parsePSTime(f[0])
	if err != nil {
		return 0, 0, err
	}
	rssKB, err = strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("agent: ps rss %q: %w", f[1], err)
	}
	return cpuSeconds, rssKB, nil
}

// parsePSTime converts ps's cumulative CPU column to seconds.
//
// The format varies with the platform and with how long the process has been up:
// Darwin prints MM:SS.ss, Linux prints HH:MM:SS and grows a DD- prefix after a day.
// The old lab had a BSD-versus-GNU branch here that nothing tested, and this is the
// replacement for it.
func parsePSTime(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("agent: ps time is empty")
	}

	days := 0.0
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.ParseFloat(d, 64)
		if err != nil {
			return 0, fmt.Errorf("agent: ps time %q: %w", s, err)
		}
		days, s = n, rest
	}

	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0, fmt.Errorf("agent: ps time %q has too many parts", s)
	}

	total := days * 86400
	// Rightmost part is seconds, then minutes, then hours.
	mult := 1.0
	for i := len(parts) - 1; i >= 0; i-- {
		v, err := strconv.ParseFloat(parts[i], 64)
		if err != nil {
			return 0, fmt.Errorf("agent: ps time %q: %w", s, err)
		}
		total += v * mult
		mult *= 60
	}
	return total, nil
}

// LocalSource picks the best source this machine supports.
//
// Choosing rather than requiring is what lets a local campaign run on a laptop at all,
// and Fidelity is what keeps that from being mistaken for a full-fidelity result.
func LocalSource() Source {
	p := NewProcFS()
	if p.Available() {
		return p
	}
	return NewPS()
}
