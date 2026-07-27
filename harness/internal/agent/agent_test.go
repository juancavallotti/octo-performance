package agent

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestProcFSSampleReadsARealStatLine(t *testing.T) {
	p := &ProcFS{Root: "testdata/proc"}

	s, err := p.Sample(1234)
	if err != nil {
		t.Fatal(err)
	}

	// utime 4500 + cutime 12 ticks at 100 Hz.
	if got, want := s.UserSeconds, 45.12; math.Abs(got-want) > 1e-9 {
		t.Errorf("UserSeconds = %v, want %v", got, want)
	}
	// stime 900 + cstime 3.
	if got, want := s.SysSeconds, 9.03; math.Abs(got-want) > 1e-9 {
		t.Errorf("SysSeconds = %v, want %v", got, want)
	}
	if got, want := s.CPUSeconds(), 54.15; math.Abs(got-want) > 1e-9 {
		t.Errorf("CPUSeconds = %v, want %v", got, want)
	}
	// VmRSS from status wins over the page count in stat, and it is kB.
	if got, want := s.RSSBytes, int64(262144*1024); got != want {
		t.Errorf("RSSBytes = %d, want %d", got, want)
	}
	if got, want := s.Threads, 42; got != want {
		t.Errorf("Threads = %d, want %d", got, want)
	}
	if got, want := s.OpenFDs, 7; got != want {
		t.Errorf("OpenFDs = %d, want %d", got, want)
	}
	if got, want := s.VoluntaryCtxSwitches, int64(918273); got != want {
		t.Errorf("VoluntaryCtxSwitches = %d, want %d", got, want)
	}
	if got, want := s.InvoluntaryCtxSwitches, int64(4455); got != want {
		t.Errorf("InvoluntaryCtxSwitches = %d, want %d", got, want)
	}
	if got, want := s.Load1, 2.71; got != want {
		t.Errorf("Load1 = %v, want %v", got, want)
	}
}

func TestPIDStatSurvivesAnExecutableNameWithSpacesAndParens(t *testing.T) {
	// The comm field is untrusted text between parentheses, and a naive whitespace
	// split shifts every subsequent field. The fixture's process is literally named
	// "octo (dev)", which is the shape a dev build would have.
	p := &ProcFS{Root: "testdata/proc"}
	s, err := p.Sample(1234)
	if err != nil {
		t.Fatal(err)
	}
	if s.Threads != 42 {
		t.Fatalf("Threads = %d: the comm field shifted the positional fields", s.Threads)
	}

	// And directly, so the failure names the cause.
	ps, err := parsePIDStat([]byte("7 (a b) (c) R 1 7 7 0 -1 0 0 0 0 0 100 200 0 0 20 0 9 0 5 0 128 0"))
	if err != nil {
		t.Fatal(err)
	}
	if ps.comm != "a b) (c" {
		t.Errorf("comm = %q", ps.comm)
	}
	if ps.state != "R" {
		t.Errorf("state = %q, want R", ps.state)
	}
	if ps.threads != 9 {
		t.Errorf("threads = %d, want 9", ps.threads)
	}
	if ps.rssPages != 128 {
		t.Errorf("rssPages = %d, want 128", ps.rssPages)
	}
}

func TestPIDStatRejectsATruncatedLine(t *testing.T) {
	// A short read of /proc must not become a plausible sample of zero.
	if _, err := parsePIDStat([]byte("7 (octo) R 1 7 7")); err == nil {
		t.Fatal("a truncated stat line must be an error, not a zero-valued sample")
	}
}

func TestHostCPUExcludesGuestFromTheBusyTotal(t *testing.T) {
	// The kernel counts guest inside user and guest_nice inside nice, so summing
	// every column inflates the total by however much virtualisation is going on.
	busy, idle, err := parseHostCPU([]byte("cpu  1000000 5000 300000 8000000 20000 0 15000 2000 900000 400\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := busy, 13220.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("busy = %v, want %v; guest columns leaked into the total", got, want)
	}
	if got, want := idle, 80200.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("idle = %v, want %v", got, want)
	}
}

func TestHostCPUCountsStealAsBusy(t *testing.T) {
	// Time the hypervisor took is time this campaign did not get. A runner that
	// cannot keep up because of steal affects the numbers exactly as one that is
	// genuinely saturated does, so it must not be filed under idle.
	withSteal, _, err := parseHostCPU([]byte("cpu  100 0 0 100 0 0 0 500 0 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := withSteal, 6.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("busy = %v, want %v", got, want)
	}
}

func TestStaticPrefersTheMachinesCoreCountOverThisProcessesView(t *testing.T) {
	// A container reporting four online CPUs against a ten-core host is a real
	// artifact from the old lab, and it was visible nowhere. cpuinfo describes the
	// machine; runtime.NumCPU describes what we were given.
	p := &ProcFS{Root: "testdata/proc"}
	st, err := p.Static()
	if err != nil {
		t.Fatal(err)
	}
	if st.Cores != 4 {
		t.Errorf("Cores = %d, want the 4 processors in cpuinfo", st.Cores)
	}
	if !strings.Contains(st.CPUModel, "Xeon") {
		t.Errorf("CPUModel = %q", st.CPUModel)
	}
	if st.Kernel != "6.8.0-1021-gcp" {
		t.Errorf("Kernel = %q", st.Kernel)
	}
	if st.ClockTick != 100 {
		t.Errorf("ClockTick = %d; whatever is assumed must be recorded", st.ClockTick)
	}
	if st.BootTime.IsZero() {
		t.Error("BootTime not derived from uptime")
	}
}

func TestProcFSAvailabilityIsAFactAboutTheHostNotAnError(t *testing.T) {
	if (&ProcFS{Root: "testdata/proc"}).Available() != true {
		t.Error("fixture root must read as available")
	}
	if (&ProcFS{Root: "testdata/nonexistent"}).Available() != false {
		t.Error("a machine with no /proc must report unavailable rather than failing later")
	}
}

func TestParsePSTime(t *testing.T) {
	// The old harness branched on BSD versus GNU here and tested neither branch.
	for _, tc := range []struct {
		in   string
		want float64
	}{
		{"0:01.23", 1.23},      // Darwin, short-lived
		{"12:34.56", 754.56},   // Darwin, minutes
		{"01:02:03", 3723},     // Linux, hours
		{"2-03:04:05", 183845}, // Linux, days
		{"0:00.00", 0},         //
		{"100:00:00", 360000},  // hours past a day without the day prefix
		{"1-00:00:00", 86400},  //
		{"0:59.99", 59.99},     //
		{"3:00", 180},          // bare minutes and seconds
		{"45", 45},             // bare seconds
		{"10-23:59:59.99", 950399.99},
	} {
		got, err := parsePSTime(tc.in)
		if err != nil {
			t.Errorf("parsePSTime(%q): %v", tc.in, err)
			continue
		}
		if math.Abs(got-tc.want) > 1e-6 {
			t.Errorf("parsePSTime(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}

	for _, bad := range []string{"", "not-a-time", "1:2:3:4"} {
		if _, err := parsePSTime(bad); err == nil {
			t.Errorf("parsePSTime(%q) must fail rather than return zero", bad)
		}
	}
}

func TestParsePSLine(t *testing.T) {
	cpu, rss, err := parsePSLine("  0:04.21  219456\n")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(cpu-4.21) > 1e-9 {
		t.Errorf("cpu = %v, want 4.21", cpu)
	}
	if rss != 219456 {
		t.Errorf("rss = %d", rss)
	}
	if _, _, err := parsePSLine("\n"); err == nil {
		t.Error("no row means the process is gone; that must be an error, not a zero sample")
	}
}

// fixedSource yields a deterministic ramp so the sampler can be tested without a real
// process.
type fixedSource struct {
	n    int
	fail bool
}

func (*fixedSource) Name() string       { return "fixed" }
func (*fixedSource) Fidelity() Fidelity { return Full }
func (*fixedSource) Static() (Static, error) {
	return Static{Hostname: "fixture", Cores: 8, ClockTick: 100}, nil
}

func (f *fixedSource) Sample(int) (Sample, error) {
	if f.fail {
		return Sample{}, errNotRunning
	}
	f.n++
	return Sample{
		T:               time.Now(),
		UserSeconds:     float64(f.n) * 0.5,
		SysSeconds:      float64(f.n) * 0.1,
		RSSBytes:        int64(f.n) * 1024,
		HostBusySeconds: float64(f.n) * 2,
	}, nil
}

func TestSamplerTakesASampleImmediatelySoAShortWindowIsNeverEmpty(t *testing.T) {
	s := NewSampler(&fixedSource{}, 1, 10*time.Millisecond)
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	c := s.Stop()
	if !c.Present() {
		t.Fatal("a sampler that started and stopped immediately still produced no sample")
	}
	if c.Static.Hostname != "fixture" {
		t.Errorf("Static not captured: %+v", c.Static)
	}
	if c.Fidelity != Full {
		t.Errorf("Fidelity = %q", c.Fidelity)
	}
}

func TestSamplerProducesCumulativeSeriesReadyForDifferencing(t *testing.T) {
	s := NewSampler(&fixedSource{}, 1, 5*time.Millisecond)
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	c := s.Stop()

	if len(c.Samples) < 3 {
		t.Fatalf("collected %d samples in 60ms at 5ms; the loop is not ticking", len(c.Samples))
	}

	cpu := c.CPU()
	// Every CPU value must be a total, never a rate: monotonically non-decreasing,
	// so series.Rate can differentiate it over whichever window is chosen later.
	for i := 1; i < len(cpu.Points); i++ {
		if cpu.Points[i].V < cpu.Points[i-1].V {
			t.Fatalf("CPU series went backwards at %d (%v then %v); it is not cumulative",
				i, cpu.Points[i-1].V, cpu.Points[i].V)
		}
	}
	if _, resets := cpu.Rate(); resets != 0 {
		t.Errorf("Rate saw %d counter resets in a monotonic series", resets)
	}
	if cpu.Unit != "seconds" {
		t.Errorf("Unit = %q; the report must never have to guess", cpu.Unit)
	}
	if got := c.HostBusy().Len(); got != len(c.Samples) {
		t.Errorf("HostBusy has %d points for %d samples", got, len(c.Samples))
	}
}

func TestSamplerRecordsFailuresRatherThanSwallowingThem(t *testing.T) {
	// A subject that dies mid-window stops producing samples. A sampler that quietly
	// collected nothing looks exactly like a process that quietly did nothing.
	s := NewSampler(&fixedSource{fail: true}, 1, 5*time.Millisecond)
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	c := s.Stop()

	if c.Present() {
		t.Fatal("samples were produced by a source that only fails")
	}
	if len(c.Errors) == 0 {
		t.Fatal("failures must be recorded on the collection")
	}
	if len(c.Errors) > 16 {
		t.Errorf("kept %d errors; the collection is not a log file", len(c.Errors))
	}
}

func TestSamplerRefusesToStartTwice(t *testing.T) {
	s := NewSampler(&fixedSource{}, 1, time.Second)
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	if err := s.Start(t.Context()); err == nil {
		t.Error("starting a running sampler must fail; two loops would double every count")
	}
}

func TestStopIsSafeWithoutStart(t *testing.T) {
	c := NewSampler(&fixedSource{}, 42, time.Second).Stop()
	if c.Present() {
		t.Error("a sampler that never started reported samples")
	}
	if c.PID != 42 || c.Source != "fixed" {
		t.Errorf("identity lost on an unstarted sampler: %+v", c)
	}
}

// errNotRunning stands in for the error a source returns once its process is gone.
var errNotRunning = errors.New("agent: process is not running")
