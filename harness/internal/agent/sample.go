package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/series"
)

// Fidelity is how much a Source can actually see. It travels with the samples so a
// reader never has to guess whether a missing field means zero or means unavailable.
type Fidelity string

const (
	// Full is /proc: per-process CPU split into user and system, RSS, threads, open
	// descriptors, context switches, and whole-machine CPU.
	Full Fidelity = "full"
	// Coarse is what a portable fallback can offer: cumulative CPU and RSS, at
	// whatever resolution the platform's ps reports.
	Coarse Fidelity = "coarse"
)

// Sample is one instant's view of a process and its machine.
//
// Every CPU field is a cumulative total since the process or machine started, never a
// percentage. Percentages are derived over the measured window, after the window is
// known.
type Sample struct {
	T time.Time `json:"t"`

	// UserSeconds and SysSeconds are the process and its reaped children.
	UserSeconds float64 `json:"userSeconds"`
	SysSeconds  float64 `json:"sysSeconds"`

	RSSBytes int64 `json:"rssBytes"`
	Threads  int   `json:"threads,omitempty"`
	OpenFDs  int   `json:"openFds,omitempty"`

	VoluntaryCtxSwitches   int64 `json:"voluntaryCtxSwitches,omitempty"`
	InvoluntaryCtxSwitches int64 `json:"involuntaryCtxSwitches,omitempty"`

	// HostBusySeconds is cumulative non-idle CPU across every core. It is what turns
	// "the generator had headroom" from an assumption into evidence, so it is
	// collected on the runner as well as on the subject.
	HostBusySeconds float64 `json:"hostBusySeconds,omitempty"`
	HostIdleSeconds float64 `json:"hostIdleSeconds,omitempty"`
	Load1           float64 `json:"load1,omitempty"`
}

// CPUSeconds is the process's total processor time.
func (s Sample) CPUSeconds() float64 { return s.UserSeconds + s.SysSeconds }

// Static describes the machine, and does not change while a campaign runs.
type Static struct {
	Hostname  string    `json:"hostname"`
	Cores     int       `json:"cores"`
	PageSize  int       `json:"pageSize,omitempty"`
	ClockTick int       `json:"clockTick,omitempty"`
	BootTime  time.Time `json:"bootTime,omitzero"`
	CPUModel  string    `json:"cpuModel,omitempty"`
	Kernel    string    `json:"kernel,omitempty"`
}

// Source reads samples for one process on one machine.
type Source interface {
	// Name identifies the mechanism, e.g. "procfs".
	Name() string
	// Fidelity is what this source can see.
	Fidelity() Fidelity
	// Static reads the machine facts once.
	Static() (Static, error)
	// Sample reads pid at the current instant.
	Sample(pid int) (Sample, error)
}

// Collected is a finished sampling run.
type Collected struct {
	Source   string   `json:"source"`
	Fidelity Fidelity `json:"fidelity"`
	Static   Static   `json:"static"`
	Interval string   `json:"interval"`
	PID      int      `json:"pid"`
	Samples  []Sample `json:"samples"`

	// Errors are ticks that failed. A subject that exits mid-window produces these
	// and the count is a finding, not something to swallow: a sampler that quietly
	// stopped collecting looks exactly like a process that quietly stopped working.
	Errors []string `json:"errors,omitempty"`
	// Missed is the number of ticks the sampler was too slow to serve. At 1 Hz this
	// should be zero; anything else means the sampler itself was starved, which
	// makes every rate derived from it suspect.
	Missed int `json:"missed,omitempty"`
}

// Present reports whether anything was actually collected.
func (c Collected) Present() bool { return len(c.Samples) > 0 }

// CPU returns cumulative process CPU seconds. It is a counter: call Rate on it to get
// utilisation over a window, which is exact in a way that reading an instantaneous
// %cpu is not.
func (c Collected) CPU() series.Series {
	return c.seriesOf("subject.cpu.seconds", "seconds", func(s Sample) float64 { return s.CPUSeconds() })
}

// RSS returns the resident set in bytes.
func (c Collected) RSS() series.Series {
	return c.seriesOf("subject.rss.bytes", "bytes", func(s Sample) float64 { return float64(s.RSSBytes) })
}

// HostBusy returns cumulative machine-wide busy CPU seconds, also a counter.
func (c Collected) HostBusy() series.Series {
	return c.seriesOf("host.cpu.seconds", "seconds", func(s Sample) float64 { return s.HostBusySeconds })
}

// OpenFDs returns the descriptor count.
func (c Collected) OpenFDs() series.Series {
	return c.seriesOf("subject.fds", "count", func(s Sample) float64 { return float64(s.OpenFDs) })
}

// Threads returns the thread count.
func (c Collected) Threads() series.Series {
	return c.seriesOf("subject.threads", "count", func(s Sample) float64 { return float64(s.Threads) })
}

func (c Collected) seriesOf(name, unit string, f func(Sample) float64) series.Series {
	pts := make([]series.Point, 0, len(c.Samples))
	for _, s := range c.Samples {
		pts = append(pts, series.Point{At: s.T, V: f(s)})
	}
	// The samples are already in tick order, so this only attaches the name and unit.
	return series.Series{Name: name, Unit: unit, Points: pts}
}

// Sampler ticks a Source at a fixed interval for the life of one cell.
//
// It is started before the measured window opens and stopped after it closes; the
// window is selected from the samples afterwards. Sampling that begins when the window
// begins cannot answer whether the window was steady, which is how a fixed ten-second
// warm-up came to be trusted without evidence.
type Sampler struct {
	src      Source
	pid      int
	interval time.Duration

	mu      sync.Mutex
	samples []Sample
	errs    []string
	missed  int

	static  Static
	cancel  context.CancelFunc
	stopped chan struct{}
	running bool
}

// NewSampler returns a sampler for pid reading through src. An interval of zero means
// one second.
func NewSampler(src Source, pid int, interval time.Duration) *Sampler {
	if interval <= 0 {
		interval = time.Second
	}
	return &Sampler{src: src, pid: pid, interval: interval}
}

// Name implements the collector contract.
func (s *Sampler) Name() string { return s.src.Name() }

// Start begins sampling. It takes one sample immediately so a short window is never
// empty, then ticks.
func (s *Sampler) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return errors.New("agent: sampler already started")
	}
	s.running = true
	s.mu.Unlock()

	st, err := s.src.Static()
	if err != nil {
		return fmt.Errorf("agent: reading machine facts: %w", err)
	}
	s.static = st

	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.stopped = make(chan struct{})

	s.tick()
	go s.loop(ctx)
	return nil
}

func (s *Sampler) loop(ctx context.Context) {
	defer close(s.stopped)

	t := time.NewTicker(s.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			// A tick that arrives late means this sampler was starved, and every
			// rate derived from it is correspondingly less trustworthy. Count it
			// rather than smoothing over it.
			if late := time.Since(now); late > s.interval/2 {
				s.mu.Lock()
				s.missed++
				s.mu.Unlock()
			}
			s.tick()
		}
	}
}

func (s *Sampler) tick() {
	sm, err := s.src.Sample(s.pid)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if len(s.errs) < 16 { // enough to characterise; not a log file
			s.errs = append(s.errs, err.Error())
		}
		return
	}
	s.samples = append(s.samples, sm)
}

// Stop ends sampling and returns everything collected.
func (s *Sampler) Stop() Collected {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return Collected{Source: s.src.Name(), Fidelity: s.src.Fidelity(), PID: s.pid}
	}
	s.running = false
	s.mu.Unlock()

	s.cancel()
	<-s.stopped

	s.mu.Lock()
	defer s.mu.Unlock()
	out := Collected{
		Source:   s.src.Name(),
		Fidelity: s.src.Fidelity(),
		Static:   s.static,
		Interval: s.interval.String(),
		PID:      s.pid,
		Samples:  append([]Sample(nil), s.samples...),
		Missed:   s.missed,
	}
	if len(s.errs) > 0 {
		out.Errors = append([]string(nil), s.errs...)
	}
	return out
}
