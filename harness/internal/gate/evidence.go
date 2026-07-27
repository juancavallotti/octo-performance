package gate

import (
	"github.com/juancavallotti/octo-performance/harness/internal/series"
	"github.com/juancavallotti/octo-performance/harness/internal/stats"
)

// Evidence is everything one cell produced.
//
// The types here are plain values rather than references into the packages that
// collected them, which keeps this package a leaf: gates can be replayed against
// frozen fixtures without starting a process, opening a socket, or importing the
// orchestrator.
//
// Several gates cross-reference two sources — a client that saw no errors against a
// server that shed work, a pool that looks fine until you compare it with its
// siblings — which is why a gate receives the whole struct rather than one metric.
type Evidence struct {
	CellID   string
	Scenario string
	Arm      string
	Rep      int
	Ordinal  int

	Load    Load
	Runner  Runner
	Subject Subject
	Server  Server
	Ready   Ready
	Caps    Caps
	Clock   Clock

	// Window is the measured interval, and WindowOK is false when detection found
	// none. A cell with no defensible window has no defensible numbers.
	Window   stats.Window
	WindowOK bool

	// FingerprintHash covers the host attributes that must match for two cells to be
	// comparable.
	FingerprintHash string

	// Peers are the sibling cells this one will be compared against. The check that
	// catches a generator collapsing on one cell and not another is necessarily
	// cross-cell.
	Peers []Peer
}

// Load is what the generator offered and observed.
type Load struct {
	Model             string
	OfferedRate       float64
	AchievedRPS       float64
	Requests          int64
	DroppedIterations float64
	FailedRate        float64
	ExitCode          int

	// PreAllocatedVUs is the Little's-law allocation, computed by the harness and
	// recorded. Its absence from the old results is why the collapse could not be
	// diagnosed from a result file.
	PreAllocatedVUs int
	MaxVUs          int
	ObservedMaxVUs  int
	VUCap           int

	// RPS is the per-second series over the measured window.
	RPS series.Series
}

// Growth is how far the generator scaled past its allocation.
func (l Load) Growth() float64 {
	if l.PreAllocatedVUs <= 0 {
		return 0
	}
	return float64(l.ObservedMaxVUs) / float64(l.PreAllocatedVUs)
}

// AchievedRatio is achieved over offered. Below one means the generator could not
// deliver the load, which changes what every latency percentile means.
func (l Load) AchievedRatio() float64 {
	if l.OfferedRate <= 0 {
		return 0
	}
	return l.AchievedRPS / l.OfferedRate
}

// Runner is the load generator's own machine. Sampling it is what turns "the generator
// probably had headroom" into evidence.
type Runner struct {
	Present bool
	Cores   int
	// CPUPctMean and CPUPctPeak are percentages where 100 is one saturated core, so
	// an 8-core runner saturates at 800.
	CPUPctMean float64
	CPUPctPeak float64
	CPU        series.Series
}

// UtilisationPct is CPU as a share of the whole machine.
func (r Runner) UtilisationPct() float64 {
	if r.Cores <= 0 {
		return 0
	}
	return r.CPUPctMean / float64(r.Cores)
}

// Subject is the machine under test.
type Subject struct {
	Present    bool
	Cores      int
	CPUPctMean float64
	CPUPctPeak float64
	CPU        series.Series
	RSSPeak    float64
	RSSDrift   float64
}

// Server is what the runtime said about itself over the window.
type Server struct {
	Present bool

	MessagesCompleted float64
	MessagesFailed    float64
	MessagesDropped   float64

	// FlowMeanSeconds is exact — sum over count, both differenced counters — unlike
	// any percentile the histogram could offer.
	FlowMeanSeconds float64
	HasFlowMean     bool

	// IdentityVersion is what octo_build_info reported from the running process,
	// against IntendedVersion, which is what the harness meant to start.
	IdentityVersion string
	IntendedVersion string

	ReadyThroughout bool
}

// Ready is how readiness was detected and how long it took.
type Ready struct {
	// Method is "readyz" or "route". A build with no admin port reaches "ready" at a
	// later moment than one that answers /readyz, so two arms measured different ways
	// have not measured the same thing.
	Method    string
	ColdStart float64 // milliseconds
}

// Caps is what the artifact said it could do, and whether that held up.
type Caps struct {
	AdminPortDetected bool
	// AdminPortAnswered is the positive confirmation. Detection is a grep of someone
	// else's help text; being wrong in the optimistic direction is the dangerous
	// direction.
	AdminPortAnswered bool
	MetricsRequested  bool
}

// Clock is the measured offset between runner and subject.
type Clock struct {
	Measured      bool
	OffsetMs      float64
	UncertaintyMs float64
	// DriftMs is how much the offset moved between the probe at cell start and the
	// one at cell end.
	DriftMs float64
}

// Peer is a sibling cell, carrying only what cross-cell checks need.
type Peer struct {
	CellID string
	// Scenario is what makes a peer comparable. Two cells of the same campaign on
	// different workloads legitimately need different generator capacity — 001 offers
	// a bare GET and 005 holds every request for 70 ms — so comparing their pools
	// answers a question nobody asked.
	Scenario        string
	Arm             string
	ObservedMaxVUs  int
	PreAllocatedVUs int
	FingerprintHash string
	ReadyMethod     string
}
