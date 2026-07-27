package loadgen

import (
	"encoding/json"
	"fmt"
	"time"
)

// Summary is the end-of-run report, parsed from k6's own summary object.
//
// Presence is tracked separately from value throughout. k6 omits a metric entirely
// when it never fired — dropped_iterations does not appear in a run that dropped
// nothing — and "absent" and "zero" are not the same claim. A gate that cannot tell
// them apart would read a missing metric as a clean run.
type Summary struct {
	// TestRunDuration is what k6 measured, not what was asked for. They differ when
	// a run is cut short, and the difference is the first sign that it was.
	TestRunDuration time.Duration `json:"testRunDuration"`

	Requests   Counted `json:"requests"`
	Iterations Counted `json:"iterations"`
	// Dropped is iterations the generator could not start because no virtual user
	// was free. Any value above zero means the offered rate was not offered, so
	// every latency percentile from that run describes a different experiment.
	Dropped Counted `json:"droppedIterations"`

	// Failed is k6's http_req_failed rate metric. A fast failure looks exactly like
	// a fast success in a latency percentile, which is why this is read before any
	// of them.
	Failed Rated `json:"failed"`

	// VUsMax is the high-water mark of the virtual-user pool. Against the
	// pre-allocation the harness computed, it is the single number that separates a
	// healthy run from one where the generator was competing with the subject.
	VUsMax Gauged `json:"vusMax"`
	VUs    Gauged `json:"vus"`

	// Duration is the full client-side request time, and Waiting is time to first
	// byte. Waiting excludes the generator's own send and receive costs, so a large
	// gap between them is the generator struggling rather than the server.
	Duration Trend `json:"duration"`
	Waiting  Trend `json:"waiting"`
	Blocked  Trend `json:"blocked"`

	// Metrics is everything k6 reported, kept so the report can show a number
	// nobody thought to name here without needing another campaign to collect it.
	Metrics map[string]Metric `json:"metrics"`
}

// Metric is one k6 metric as reported.
type Metric struct {
	Type     string             `json:"type"`
	Contains string             `json:"contains,omitempty"`
	Values   map[string]float64 `json:"values"`
}

// Counted is a counter: a total and the rate it accumulated at.
type Counted struct {
	Present bool    `json:"present"`
	Count   float64 `json:"count"`
	Rate    float64 `json:"rate"`
}

// Rated is a k6 rate metric: a share, with the two tallies behind it.
type Rated struct {
	Present bool    `json:"present"`
	Rate    float64 `json:"rate"`
	Passes  float64 `json:"passes"`
	Fails   float64 `json:"fails"`
}

// Gauged is a gauge with its extremes over the run.
type Gauged struct {
	Present bool    `json:"present"`
	Value   float64 `json:"value"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
}

// Trend is a k6 trend metric. Every field is milliseconds, as k6 reports time.
type Trend struct {
	Present bool    `json:"present"`
	Avg     float64 `json:"avg"`
	Min     float64 `json:"min"`
	Med     float64 `json:"med"`
	P90     float64 `json:"p90"`
	P95     float64 `json:"p95"`
	P99     float64 `json:"p99"`
	Max     float64 `json:"max"`
}

// rawSummary mirrors k6's handleSummary argument.
type rawSummary struct {
	State struct {
		TestRunDurationMs float64 `json:"testRunDurationMs"`
	} `json:"state"`
	Metrics map[string]Metric `json:"metrics"`
}

// ParseSummary reads k6's summary JSON.
func ParseSummary(b []byte) (Summary, error) {
	var raw rawSummary
	if err := json.Unmarshal(b, &raw); err != nil {
		return Summary{}, fmt.Errorf("loadgen: parsing k6 summary: %w", err)
	}
	if raw.Metrics == nil {
		return Summary{}, fmt.Errorf("loadgen: k6 summary carries no metrics")
	}

	s := Summary{
		TestRunDuration: time.Duration(raw.State.TestRunDurationMs * float64(time.Millisecond)),
		Metrics:         raw.Metrics,
	}
	s.Requests = counted(raw.Metrics, "http_reqs")
	s.Iterations = counted(raw.Metrics, "iterations")
	s.Dropped = counted(raw.Metrics, "dropped_iterations")
	s.Failed = rated(raw.Metrics, "http_req_failed")
	s.VUsMax = gauged(raw.Metrics, "vus_max")
	s.VUs = gauged(raw.Metrics, "vus")
	s.Duration = trend(raw.Metrics, "http_req_duration")
	s.Waiting = trend(raw.Metrics, "http_req_waiting")
	s.Blocked = trend(raw.Metrics, "http_req_blocked")
	return s, nil
}

func counted(m map[string]Metric, name string) Counted {
	v, ok := m[name]
	if !ok {
		return Counted{}
	}
	return Counted{Present: true, Count: v.Values["count"], Rate: v.Values["rate"]}
}

func rated(m map[string]Metric, name string) Rated {
	v, ok := m[name]
	if !ok {
		return Rated{}
	}
	return Rated{Present: true, Rate: v.Values["rate"], Passes: v.Values["passes"], Fails: v.Values["fails"]}
}

func gauged(m map[string]Metric, name string) Gauged {
	v, ok := m[name]
	if !ok {
		return Gauged{}
	}
	return Gauged{Present: true, Value: v.Values["value"], Min: v.Values["min"], Max: v.Values["max"]}
}

func trend(m map[string]Metric, name string) Trend {
	v, ok := m[name]
	if !ok {
		return Trend{}
	}
	return Trend{
		Present: true,
		Avg:     v.Values["avg"],
		Min:     v.Values["min"],
		Med:     v.Values["med"],
		P90:     v.Values["p(90)"],
		P95:     v.Values["p(95)"],
		P99:     v.Values["p(99)"],
		Max:     v.Values["max"],
	}
}

// AchievedRPS is what the generator actually delivered.
func (s Summary) AchievedRPS() float64 { return s.Requests.Rate }

// FailedShare is the share of requests that failed, in [0,1].
func (s Summary) FailedShare() float64 { return s.Failed.Rate }
