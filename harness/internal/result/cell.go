// Package result is the on-disk shape of what a campaign produced.
//
// One type per artifact, and every number carries what it needs to be read: its
// verdict, the window it describes, and whether it was measured or is missing. The old
// lab captured roughly sixty fields per repetition, funnelled them into six, and
// published four — as unqualified point estimates, with no way to tell a saturated run
// from a clean one. Nothing here drops a field on the way to the report; the report
// chooses what to show.
package result

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/agent"
	"github.com/juancavallotti/octo-performance/harness/internal/collect"
	"github.com/juancavallotti/octo-performance/harness/internal/gate"
	"github.com/juancavallotti/octo-performance/harness/internal/loadgen"
	"github.com/juancavallotti/octo-performance/harness/internal/payload"
	"github.com/juancavallotti/octo-performance/harness/internal/promx"
	"github.com/juancavallotti/octo-performance/harness/internal/render"
	"github.com/juancavallotti/octo-performance/harness/internal/stats"
	"github.com/juancavallotti/octo-performance/harness/internal/subject"
)

// Schema is the version of this layout. It is written into every cell so a reader
// years later can tell whether the file means what it appears to mean.
const Schema = 1

// Cell is one execution, complete.
type Cell struct {
	Schema int `json:"schema"`

	Campaign      string `json:"campaign"`
	Scenario      string `json:"scenario"`
	Arm           string `json:"arm"`
	Rep           int    `json:"rep"`
	Ordinal       int    `json:"ordinal"`
	PositionInRep int    `json:"positionInRep"`

	StartedAt time.Time     `json:"startedAt"`
	EndedAt   time.Time     `json:"endedAt"`
	Elapsed   time.Duration `json:"elapsed"`

	// Harness identifies the binary that produced this, because a result that
	// cannot be attributed to a version is not a result — and that applies to the
	// measuring instrument as much as to what it measured.
	Harness Harness `json:"harness"`

	Binary   subject.Caps     `json:"binary"`
	Identity subject.Identity `json:"identity"`
	Argv     []string         `json:"argv"`
	// Withheld are flags the harness wanted and the artifact could not accept. An
	// arm that could not serve metrics is not comparable to one that did.
	Withheld []string       `json:"withheld,omitempty"`
	Ready    subject.Ready  `json:"ready"`
	Totals   subject.Totals `json:"totals"`
	Config   render.Result  `json:"config"`
	Load     LoadSpec       `json:"load"`

	// Request is what was offered: the method, and the digest of the exact bytes.
	// Two campaigns claiming to compare the same workload can then be checked rather
	// than assumed to agree.
	Request RequestSpec `json:"request"`

	Warmup   *loadgen.Run `json:"warmup,omitempty"`
	Measured loadgen.Run  `json:"measured"`

	// Window is the interval the numbers describe, and Naive is the fixed-offset
	// window the old lab would have used. Both are kept so detection is auditable
	// across a campaign rather than trusted.
	Window      stats.Window `json:"window"`
	WindowOK    bool         `json:"windowOk"`
	WindowNote  string       `json:"windowNote,omitempty"`
	NaiveWindow stats.Window `json:"naiveWindow"`

	Subject agent.Collected `json:"subject"`
	Runner  agent.Collected `json:"runner"`
	Server  Server          `json:"server"`

	// Clock is the subject's offset from the runner, measured at cell start and again
	// at cell end. Everything here is bucketed into one-second bins, so half a second
	// of skew moves a sample into the neighbouring bucket — enough to invert "the CPU
	// spike preceded the throughput drop".
	Clock gate.Clock `json:"clock"`

	Headline Headline     `json:"headline"`
	Verdict  gate.Verdict `json:"verdict"`
}

// Harness is what produced the result.
type Harness struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	Dev     bool   `json:"dev,omitempty"`
}

// RequestSpec is what the generator sent.
type RequestSpec struct {
	Method      string `json:"method"`
	Route       string `json:"route"`
	ContentType string `json:"contentType,omitempty"`
	// Payload is present when the body was generated. The bytes themselves are
	// archived in the cell directory as body.dat; this carries their identity.
	Payload *payload.Body `json:"payload,omitempty"`
}

// LoadSpec is the resolved load, recorded as executed.
type LoadSpec struct {
	Test            string        `json:"test,omitempty"`
	Model           string        `json:"model"`
	Rate            int           `json:"rate,omitempty"`
	VUs             int           `json:"vus,omitempty"`
	Duration        time.Duration `json:"duration"`
	Warmup          time.Duration `json:"warmup,omitempty"`
	ExpectedLatency time.Duration `json:"expectedLatency,omitempty"`
	Calibrated      bool          `json:"calibrated,omitempty"`
	// RateSource says where the rate came from: "scenario", "campaign" or
	// "calibrated". A rate nobody can account for is how STEADY_RATE=16000 outlived
	// the machine it was measured on.
	RateSource string `json:"rateSource,omitempty"`
}

// Server is what the runtime said about itself over the measured window.
type Server struct {
	Present bool `json:"present"`
	// Bracket is the pair of scrapes the counters were differenced across, and how
	// much wider that is than the window.
	Bracket collect.Bracket `json:"bracket"`

	// Flows are the flow names the config declared, which is what the runtime labels
	// its metrics with. Recorded because deriving them from the scenario id looks
	// right and is wrong.
	Flows []string `json:"flows,omitempty"`

	MessagesCompleted float64 `json:"messagesCompleted"`
	MessagesFailed    float64 `json:"messagesFailed"`
	MessagesDropped   float64 `json:"messagesDropped"`

	// FlowMeanSeconds is exact: a differenced sum over a differenced count. Unlike
	// any percentile the histogram could offer, it involves no interpolation.
	FlowMeanSeconds float64 `json:"flowMeanSeconds,omitempty"`
	HasFlowMean     bool    `json:"hasFlowMean"`

	// FlowP95 and FlowP99 are bounded quantiles. Seconds is meaningless unless Ok:
	// the runtime's histogram has a 5 ms lowest edge and a healthy flow finishes in
	// hundreds of microseconds, so interpolating inside the first bucket would
	// invent a number and print it as if it were measured.
	FlowP95 promx.Quantile `json:"flowP95"`
	FlowP99 promx.Quantile `json:"flowP99"`

	// ThroughputRPS is completions over the bracket's own span, never over the
	// window it is reported beside.
	ThroughputRPS float64 `json:"throughputRps"`
}

// Headline is the small set of numbers a reader actually compares.
//
// Every one of them is derived here, once, so the report formats and derives nothing.
// That is what let the old report.py grow to a thousand lines and become a library by
// accident.
type Headline struct {
	OfferedRate   float64 `json:"offeredRate"`
	AchievedRPS   float64 `json:"achievedRps"`
	AchievedRatio float64 `json:"achievedRatio"`

	// Client latencies, in milliseconds, as the generator saw them.
	ClientMeanMs float64 `json:"clientMeanMs"`
	ClientP50Ms  float64 `json:"clientP50Ms"`
	ClientP95Ms  float64 `json:"clientP95Ms"`
	ClientP99Ms  float64 `json:"clientP99Ms"`
	// ClientWaitingP95Ms is time to first byte, which excludes the generator's own
	// send and receive costs. A wide gap between it and ClientP95Ms is the generator
	// struggling rather than the server.
	ClientWaitingP95Ms float64 `json:"clientWaitingP95Ms"`

	// ServerMeanMs is the runtime's own view of the same work. The old lab published
	// a client p95 of 933 ms against a server mean of 0.41 ms and called the first
	// one the runtime's latency; showing both is what makes that impossible.
	ServerMeanMs    float64 `json:"serverMeanMs,omitempty"`
	HasServerMean   bool    `json:"hasServerMean"`
	ClientServerGap float64 `json:"clientServerGap,omitempty"`

	Requests float64 `json:"requests"`
	Failed   float64 `json:"failed"`
	Dropped  float64 `json:"dropped"`

	PreAllocatedVUs int     `json:"preAllocatedVUs"`
	ObservedMaxVUs  int     `json:"observedMaxVUs"`
	PoolGrowth      float64 `json:"poolGrowth"`

	// SubjectCPUPct is percent of one core, so an eight-core subject saturates at
	// 800. It is a rate derived from a cumulative counter over the window, not an
	// instantaneous reading.
	SubjectCPUPct      float64 `json:"subjectCpuPct,omitempty"`
	SubjectCPUmsPerReq float64 `json:"subjectCpuMsPerRequest,omitempty"`
	SubjectRSSPeakMB   float64 `json:"subjectRssPeakMb,omitempty"`
	SubjectRSSDriftMB  float64 `json:"subjectRssDriftMb,omitempty"`

	// RunnerCPUPct is the generator's own cost. Its absence is why the old numbers
	// cannot be defended.
	RunnerCPUPct    float64 `json:"runnerCpuPct,omitempty"`
	HasRunnerCPU    bool    `json:"hasRunnerCpu"`
	ColdStartMs     float64 `json:"coldStartMs"`
	LifetimeCPUSecs float64 `json:"lifetimeCpuSeconds,omitempty"`
}

// Slug is the cell's directory name.
func (c *Cell) Slug() string {
	return fmt.Sprintf("%s__%s__rep%d", c.Scenario, sanitise(c.Arm), c.Rep)
}

func sanitise(s string) string {
	out := []rune(s)
	for i, r := range out {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
		default:
			out[i] = '-'
		}
	}
	return string(out)
}

// Write persists the cell atomically.
//
// Atomically because an interrupted campaign must not leave a half-written cell.json
// that a later reader cannot distinguish from a complete one — which is exactly the
// state three directories in the old results tree were left in.
func (c *Cell) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("result: %w", err)
	}
	if err := writeJSON(filepath.Join(dir, "cell.json"), c); err != nil {
		return err
	}
	// The verdict again on its own, because "which cells failed and why" is a
	// question worth being able to answer with grep.
	return writeJSON(filepath.Join(dir, "verdict.json"), c.Verdict)
}

// ReadCell loads a cell from its directory.
func ReadCell(dir string) (*Cell, error) {
	b, err := os.ReadFile(filepath.Join(dir, "cell.json"))
	if err != nil {
		return nil, fmt.Errorf("result: %w", err)
	}
	var c Cell
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("result: parsing %s: %w", dir, err)
	}
	if c.Schema != Schema {
		return nil, fmt.Errorf("result: %s is schema %d, this harness writes %d", dir, c.Schema, Schema)
	}
	return &c, nil
}

// WriteJSON writes v atomically. Exported because a campaign writes its plan the same
// way it writes its cells: an interrupted run must never leave a truncated artifact
// that a later reader cannot distinguish from a complete one.
func WriteJSON(path string, v any) error { return writeJSON(path, v) }

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("result: encoding %s: %w", filepath.Base(path), err)
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return fmt.Errorf("result: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("result: writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("result: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("result: %w", err)
	}
	return os.Rename(tmp.Name(), path)
}
