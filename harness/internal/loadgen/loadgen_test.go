package loadgen

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/spec"
)

// The summary fixtures are real k6 output, produced against the fake runtime at two
// operating points. summary-clean.json is a generator keeping up. summary-collapsed
// .json is the failure this whole rebuild exists for, reproduced small: 3,000
// requests per second offered, 127 achieved, 11,292 iterations dropped, the pool
// pinned at its ceiling, and a p95 of 1.58 seconds. The proportions match the real
// 2026-07-26 incident closely enough that a gate which passes this passes that.

func summaryFixture(t *testing.T, name string) Summary {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "summary-"+name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := ParseSummary(b)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestParseSummaryReadsAHealthyRun(t *testing.T) {
	s := summaryFixture(t, "clean")

	if got, want := s.Requests.Count, 3201.0; got != want {
		t.Errorf("Requests.Count = %v, want %v", got, want)
	}
	if got := s.AchievedRPS(); math.Abs(got-799.91) > 0.01 {
		t.Errorf("AchievedRPS = %v, want 799.91", got)
	}
	if got, want := s.VUsMax.Max, 64.0; got != want {
		t.Errorf("VUsMax.Max = %v, want %v", got, want)
	}
	if got := s.TestRunDuration; math.Abs(got.Seconds()-4.0017) > 0.001 {
		t.Errorf("TestRunDuration = %v, want ~4.0017s", got)
	}
	if !s.Duration.Present || s.Duration.P95 <= 0 {
		t.Errorf("Duration = %+v", s.Duration)
	}
	if s.Failed.Rate != 0 {
		t.Errorf("Failed.Rate = %v on a clean run", s.Failed.Rate)
	}
}

func TestAbsentIsNotZero(t *testing.T) {
	// k6 omits dropped_iterations entirely from a run that dropped nothing. A gate
	// that cannot tell "absent" from "zero" reads a missing metric as a clean run,
	// which is the optimistic direction and therefore the dangerous one.
	clean := summaryFixture(t, "clean")
	if clean.Dropped.Present {
		t.Error("the clean fixture reports dropped_iterations as present; k6 does not emit it")
	}
	if clean.Dropped.Count != 0 {
		t.Errorf("Dropped.Count = %v with the metric absent", clean.Dropped.Count)
	}

	collapsed := summaryFixture(t, "collapsed")
	if !collapsed.Dropped.Present {
		t.Fatal("the collapsed fixture must report dropped_iterations as present")
	}
	if got, want := collapsed.Dropped.Count, 11292.0; got != want {
		t.Errorf("Dropped.Count = %v, want %v", got, want)
	}
}

func TestParseSummaryReadsACollapsedRun(t *testing.T) {
	s := summaryFixture(t, "collapsed")

	// Everything a gate needs to reject this cell, from one file.
	if got := s.AchievedRPS(); math.Abs(got-127.28) > 0.01 {
		t.Errorf("AchievedRPS = %v, want 127.28", got)
	}
	if got, want := s.VUsMax.Max, 200.0; got != want {
		t.Errorf("VUsMax.Max = %v, want %v — the pool ran into its ceiling", got, want)
	}
	if s.Duration.P95 < 1000 {
		t.Errorf("Duration.P95 = %v ms, want over a second", s.Duration.P95)
	}
	// The run was asked for four seconds and took five and a half, because the
	// executor could not finish on time. That gap is itself evidence.
	if s.TestRunDuration < 5*time.Second {
		t.Errorf("TestRunDuration = %v, want the overrun to be visible", s.TestRunDuration)
	}
}

func TestParseSummaryKeepsEveryMetricKForReported(t *testing.T) {
	// Roughly sixty fields were captured per repetition in the old lab and four
	// reached the published table. The typed fields above are what gates read; this
	// map is so the report can show something nobody thought to name here without
	// needing another campaign to collect it.
	s := summaryFixture(t, "clean")
	for _, want := range []string{"http_req_connecting", "http_req_sending", "data_received"} {
		if _, ok := s.Metrics[want]; !ok {
			t.Errorf("metric %q was dropped on the way in", want)
		}
	}
}

func TestParseSummaryRejectsSomethingThatIsNotASummary(t *testing.T) {
	if _, err := ParseSummary([]byte(`{"state":{}}`)); err == nil {
		t.Fatal("a summary with no metrics was accepted")
	}
	if _, err := ParseSummary([]byte("not json")); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
}

func TestAggregatorFoldsRowsIntoOneSecondBuckets(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "series.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	a := NewAggregator()
	if err := a.Consume(f); err != nil {
		t.Fatal(err)
	}

	rows, unparsed := a.Rows()
	if rows != 25 {
		t.Errorf("rows = %d, want 25 (a header and 24 observations)", rows)
	}
	if unparsed != 1 {
		t.Errorf("unparsed = %d, want exactly 1 (the header); more means the format moved", unparsed)
	}

	frame := a.Frame()
	rps, ok := frame.Get("k6.rps")
	if !ok {
		t.Fatal("no rps series")
	}
	if rps.Len() != 3 {
		t.Fatalf("rps has %d points, want 3 seconds with data", rps.Len())
	}
	if got := rps.Points[0].V; got != 2 {
		t.Errorf("second 0 rps = %v, want 2", got)
	}
	if got := rps.Points[1].V; got != 1 {
		t.Errorf("second 1 rps = %v, want 1", got)
	}

	// The third second of the run produced nothing at all and is absent, rather than
	// present as a zero. A second the generator skipped and a second in which it
	// delivered nothing are different claims.
	if got := rps.Points[2].At.Unix(); got != 1785200003 {
		t.Errorf("third point is at %d; an empty second was zero-filled", got)
	}

	lat, _ := frame.Get("k6.latency.mean")
	if got := lat.Points[0].V; got != 3 {
		t.Errorf("mean latency in second 0 = %v, want 3 (2 and 4)", got)
	}
	latMax, _ := frame.Get("k6.latency.max")
	if got := latMax.Points[0].V; got != 4 {
		t.Errorf("max latency in second 0 = %v, want 4", got)
	}

	failed, _ := frame.Get("k6.failed")
	if got := failed.Points[1].V; got != 1 {
		t.Errorf("failures in second 1 = %v, want 1", got)
	}
	dropped, _ := frame.Get("k6.dropped")
	if got := dropped.Points[1].V; got != 2 {
		t.Errorf("dropped in second 1 = %v, want 2", got)
	}
}

func TestAggregatorTracksThePoolHighWaterMark(t *testing.T) {
	// This is the number that separates a healthy run from one where the generator
	// was competing with the subject for cores, and in the old lab it existed only
	// inside a k6 script and reached no result file.
	f, err := os.Open(filepath.Join("testdata", "series.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	a := NewAggregator()
	if err := a.Consume(f); err != nil {
		t.Fatal(err)
	}
	if got := a.ObservedMaxVUs(); got != 40 {
		t.Errorf("ObservedMaxVUs = %d, want 40", got)
	}

	// Per second, so growth confined to a warm-up is distinguishable from growth
	// that ran through the measured window.
	vus, _ := a.Frame().Get("k6.vus")
	if vus.Len() != 3 {
		t.Fatalf("vus has %d points", vus.Len())
	}
	if vus.Points[0].V != 10 || vus.Points[1].V != 40 {
		t.Errorf("vus per second = %v %v, want 10 then 40", vus.Points[0].V, vus.Points[1].V)
	}
}

func TestAggregatorHandlesAnEmptyStream(t *testing.T) {
	a := NewAggregator()
	if err := a.Consume(strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if got := a.Frame().Names(); len(got) != 0 {
		t.Errorf("an empty stream produced series %v", got)
	}
	if got := a.ObservedMaxVUs(); got != 0 {
		t.Errorf("ObservedMaxVUs = %d on an empty stream", got)
	}
}

func TestSizePoolFollowsLittlesLaw(t *testing.T) {
	// concurrency = rate x latency, times a headroom multiple for the tail. The
	// recorded evidence is 16,000 requests per second at 25ms expected latency
	// giving 1,600 pre-allocated users, which is what the healthy runs show.
	p := SizePool(16000, 25*time.Millisecond, 0)
	if p.PreAllocatedVUs != 1600 {
		t.Errorf("PreAllocatedVUs = %d, want 1600", p.PreAllocatedVUs)
	}
	if p.MaxVUs != 16000 {
		t.Errorf("MaxVUs = %d, want ten times the allocation", p.MaxVUs)
	}
}

func TestSizePoolKeepsAFloorForSlowLoads(t *testing.T) {
	p := SizePool(5, time.Millisecond, 0)
	if p.PreAllocatedVUs != 20 {
		t.Errorf("PreAllocatedVUs = %d, want the floor of 20; a pool that small absorbs no jitter", p.PreAllocatedVUs)
	}
}

func TestSizePoolRespectsAHardCap(t *testing.T) {
	p := SizePool(16000, 25*time.Millisecond, 500)
	if p.PreAllocatedVUs != 500 || p.MaxVUs != 500 {
		t.Errorf("pool = %+v, want both clamped to the cap", p)
	}
}

func TestPoolGrowthIsTheDiscriminator(t *testing.T) {
	// 1,600 of 1,600 on the healthy run; 7,113 of 1,600 on the collapsed one. The
	// second number is the extra goroutines taking cores from the subject.
	healthy := Pool{PreAllocatedVUs: 1600, ObservedMaxVUs: 1600}
	if got := healthy.Growth(); got != 1.0 {
		t.Errorf("healthy growth = %v, want 1.0", got)
	}
	collapsed := Pool{PreAllocatedVUs: 1600, ObservedMaxVUs: 7113}
	if got := collapsed.Growth(); math.Abs(got-4.446) > 0.001 {
		t.Errorf("collapsed growth = %v, want ~4.45", got)
	}
	if (Pool{}).Growth() != 0 {
		t.Error("growth of an unallocated pool must be zero, not a division by zero")
	}
}

func TestEnvironmentRequiresWhatEachModelNeeds(t *testing.T) {
	base := Request{URL: "http://s/page", Duration: 30 * time.Second, Pool: SizePool(100, time.Millisecond, 0)}

	if _, err := environment(Request{URL: base.URL, Duration: base.Duration, Model: spec.Open}, "/s.json"); err == nil {
		t.Error("an open-model pass with no rate was accepted")
	}
	if _, err := environment(Request{URL: base.URL, Duration: base.Duration, Model: spec.Closed}, "/s.json"); err == nil {
		t.Error("a closed-model pass with no vus was accepted")
	}

	open := base
	open.Model, open.Rate = spec.Open, 800
	env, err := environment(open, "/s.json")
	if err != nil {
		t.Fatal(err)
	}
	if env["PERF_RATE"] != "800" || env["PERF_MODEL"] != "open" {
		t.Errorf("env = %v", env)
	}
	if env["PERF_PRE_VUS"] == "" || env["PERF_MAX_VUS"] == "" {
		t.Error("the pool must reach the script from Go, never be computed inside it")
	}

	closed := base
	closed.Model, closed.VUs = spec.Closed, 50
	env, err = environment(closed, "/s.json")
	if err != nil {
		t.Fatal(err)
	}
	if env["PERF_VUS"] != "50" || env["PERF_MODEL"] != "closed" {
		t.Errorf("env = %v", env)
	}
	if _, ok := env["PERF_RATE"]; ok {
		t.Error("a closed-model pass carried a rate")
	}
}

func TestStagesDisplaceTheSteadyRate(t *testing.T) {
	req := Request{
		URL: "http://s/page", Duration: time.Minute, Model: spec.Open,
		StartRate: 100,
		Stages:    []Stage{{Target: 1000, Duration: 30 * time.Second}, {Target: 4000, Duration: 30 * time.Second}},
		Pool:      SizePool(4000, 5*time.Millisecond, 0),
	}
	env, err := environment(req, "/s.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := env["PERF_RATE"]; ok {
		t.Error("a ramp carried a steady rate as well")
	}
	if !strings.Contains(env["PERF_STAGES"], `"target":1000`) {
		t.Errorf("PERF_STAGES = %q", env["PERF_STAGES"])
	}
	if !strings.Contains(env["PERF_STAGES"], `"duration":"30000ms"`) {
		t.Errorf("PERF_STAGES = %q; k6 needs a duration it can parse", env["PERF_STAGES"])
	}
	if env["PERF_START_RATE"] != "100" {
		t.Errorf("PERF_START_RATE = %q", env["PERF_START_RATE"])
	}
}

func TestDurationTextIsSomethingK6CanParse(t *testing.T) {
	// Go renders 90s as "1m30s" and 1500ms as "1.5s". k6 accepts the first and
	// rejects the second, so nothing here goes through Duration.String().
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Second, "30000ms"},
		{90 * time.Second, "90000ms"},
		{1500 * time.Millisecond, "1500ms"},
		{0, "30s"},
	} {
		if got := durationText(tc.in); got != tc.want {
			t.Errorf("durationText(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestScriptIsArchivedWithItsDigest(t *testing.T) {
	if len(ScriptSHA256) != 64 {
		t.Errorf("ScriptSHA256 = %q", ScriptSHA256)
	}
	// What was offered is as much a part of a result as what came back, so the
	// script has to say the things a reader will ask of it.
	for _, want := range []string{"constant-arrival-rate", "constant-vus", "ramping-arrival-rate", "handleSummary"} {
		if !strings.Contains(Script, want) {
			t.Errorf("the script no longer contains %q", want)
		}
	}
	if strings.Contains(Script, "abortOnFail") {
		t.Error("a threshold that aborts cuts the run short exactly when it is worth measuring")
	}
}
