package promx

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestParseBasicShapes(t *testing.T) {
	e := mustParse(t, `
# HELP octo_ready Whether the runtime is ready.
# TYPE octo_ready gauge
octo_ready 1
octo_connectors 1
octo_flow_messages_total{flow="page",outcome="completed"} 682392
octo_flow_messages_total{flow="page",outcome="failed"} 0
go_info{version="go1.25.0"} 1
`)

	if v, ok := e.First("octo_ready"); !ok || v != 1 {
		t.Fatalf("octo_ready = %v/%v", v, ok)
	}
	if got := e.LabelsOf("go_info")["version"]; got != "go1.25.0" {
		t.Fatalf("go_info version = %q", got)
	}
	if v, ok := e.Total("octo_flow_messages_total", nil); !ok || v != 682392 {
		t.Fatalf("unfiltered total = %v/%v", v, ok)
	}
	if v, ok := e.Total("octo_flow_messages_total", Labels{"outcome": "completed"}); !ok || v != 682392 {
		t.Fatalf("filtered total = %v/%v", v, ok)
	}
}

func TestParseSkipsCommentsAndJunkWithoutDiscardingTheScrape(t *testing.T) {
	e := mustParse(t, `
# a comment
octo_good 1
octo_bad not_a_number
octo_nan NaN
octo_also_good 2
`)
	// One malformed series must not cost the 175 that were fine.
	if _, ok := e.First("octo_good"); !ok {
		t.Fatal("dropped a good series")
	}
	if _, ok := e.First("octo_also_good"); !ok {
		t.Fatal("stopped parsing after a bad line")
	}
	if _, ok := e.First("octo_bad"); ok {
		t.Fatal("kept an unparseable value")
	}
	if _, ok := e.First("octo_nan"); ok {
		t.Fatal("NaN is not data and must be dropped")
	}
}

func TestParseKeepsInfinities(t *testing.T) {
	e := mustParse(t, `x_bucket{le="+Inf"} 5`)
	v, ok := e.Total("x_bucket", Labels{"le": "+Inf"})
	if !ok || v != 5 {
		t.Fatalf("got %v/%v", v, ok)
	}
}

// A block address is a legitimate label value and --metrics-blocks takes a
// comma-separated list of them, so a naive split would mangle exactly the metrics
// that only exist when block telemetry is on.
func TestParseLabelsWithCommasInsideQuotes(t *testing.T) {
	e := mustParse(t, `octo_block_duration_seconds_count{path="flows[0].process[2]",type="rest",flow="a,b"} 7`)
	l := e.LabelsOf("octo_block_duration_seconds_count")
	if l["path"] != "flows[0].process[2]" {
		t.Fatalf("path = %q", l["path"])
	}
	if l["flow"] != "a,b" {
		t.Fatalf("comma inside quotes was split: flow = %q", l["flow"])
	}
	if l["type"] != "rest" {
		t.Fatalf("type = %q", l["type"])
	}
}

func TestParseLabelValueWithSpaces(t *testing.T) {
	// The value is split off the end, not at the first space.
	e := mustParse(t, `octo_build_info{subject="fix the thing",version="0.5.0"} 1`)
	l := e.LabelsOf("octo_build_info")
	if l["subject"] != "fix the thing" {
		t.Fatalf("subject = %q", l["subject"])
	}
	if l["version"] != "0.5.0" {
		t.Fatalf("version = %q", l["version"])
	}
}

func TestParseEscapesInLabelValues(t *testing.T) {
	e := mustParse(t, `m{a="say \"hi\"",b="back\\slash"} 1`)
	l := e.LabelsOf("m")
	if l["a"] != `say "hi"` {
		t.Fatalf("a = %q", l["a"])
	}
	if l["b"] != `back\slash` {
		t.Fatalf("b = %q", l["b"])
	}
}

func TestTotalIsStrictAboutNotMatching(t *testing.T) {
	e := mustParse(t, `octo_flow_messages_total{outcome="completed"} 10`)

	if _, ok := e.Total("nope", nil); ok {
		t.Fatal("absent metric must not report ok")
	}
	// A mistyped label must not read as a measurement of zero.
	if _, ok := e.Total("octo_flow_messages_total", Labels{"outcum": "failed"}); ok {
		t.Fatal("non-matching filter must not report ok")
	}
}

func TestByLabel(t *testing.T) {
	e := mustParse(t, `
m{outcome="completed"} 10
m{outcome="completed"} 5
m{outcome="failed"} 2
m 1
`)
	got := e.ByLabel("m", "outcome")
	if got["completed"] != 15 {
		t.Fatalf("completed = %v, want summed 15", got["completed"])
	}
	if got["failed"] != 2 {
		t.Fatalf("failed = %v", got["failed"])
	}
	if got[""] != 1 {
		t.Fatalf("unlabelled series should collect under \"\": %v", got[""])
	}
}

func TestCounterDelta(t *testing.T) {
	tests := []struct {
		name       string
		start, end string
		want       float64
		wantOK     bool
	}{
		{"normal advance", `c 10`, `c 25`, 15, true},
		{"no advance", `c 10`, `c 10`, 0, true},
		{"missing at start", ``, `c 25`, 0, false},
		{"missing at end", `c 10`, ``, 0, false},
		{"counter went backwards means a restart", `c 100`, `c 5`, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CounterDelta(mustParse(t, tc.start), mustParse(t, tc.end), "c", nil)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Fatalf("delta = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCounterDeltaByLabelDropsRestarts(t *testing.T) {
	start := mustParse(t, "m{f=\"a\"} 10\nm{f=\"b\"} 100")
	end := mustParse(t, "m{f=\"a\"} 30\nm{f=\"b\"} 5\nm{f=\"c\"} 7")

	got := CounterDeltaByLabel(start, end, "m", "f")
	if got["a"] != 20 {
		t.Fatalf("a = %v, want 20", got["a"])
	}
	if _, present := got["b"]; present {
		t.Fatal("b went backwards and must be omitted, not reported negative")
	}
	if got["c"] != 7 {
		t.Fatalf("c appeared mid-window and should count from zero: %v", got["c"])
	}
}

// --- the bounded quantile, which is the reason this package exists ---

// buildHistogram makes exposition text for a histogram with the given cumulative
// bucket counts, in Prometheus's default bucket layout.
func histoText(counts map[string]float64, sum float64) string {
	order := []string{"0.005", "0.01", "0.025", "0.05", "0.1", "0.25", "0.5", "1", "2.5", "5", "10", "+Inf"}
	out := ""
	for _, le := range order {
		if c, ok := counts[le]; ok {
			out += "h_bucket{le=\"" + le + "\"} " + ftoa(c) + "\n"
		}
	}
	out += "h_sum " + ftoa(sum) + "\n"
	out += "h_count " + ftoa(counts["+Inf"]) + "\n"
	return out
}

func TestQuantileBelowTheLowestEdgeIsNeverInterpolated(t *testing.T) {
	// 99% of observations inside the first bucket — the shape this lab actually has.
	zero := map[string]float64{}
	for _, le := range []string{"0.005", "0.01", "0.025", "0.05", "0.1", "0.25", "0.5", "1", "2.5", "5", "10", "+Inf"} {
		zero[le] = 0
	}
	end := map[string]float64{}
	for le := range zero {
		end[le] = 1000
	}
	end["0.005"] = 990

	h, ok := HistogramWindow(mustParse(t, histoText(zero, 0)), mustParse(t, histoText(end, 0.4)), "h", nil)
	if !ok {
		t.Fatal("expected a histogram window")
	}

	for _, q := range []float64{0.5, 0.9, 0.95} {
		got := h.Quantile(q)
		if got.Ok {
			t.Fatalf("p%v reported Ok with Seconds=%v; interpolating inside the lowest bucket invents a number",
				q*100, got.Seconds)
		}
		if got.Bound != Below {
			t.Fatalf("p%v bound = %q, want %q", q*100, got.Bound, Below)
		}
		if got.Edge != 0.005 {
			t.Fatalf("p%v edge = %v, want 0.005", q*100, got.Edge)
		}
		if want := "< 5.00 ms"; got.String() != want {
			t.Fatalf("p%v rendered %q, want %q", q*100, got.String(), want)
		}
	}
}

func TestQuantileInterpolatesWhenTheBucketsSupportIt(t *testing.T) {
	// Half the observations under 5 ms, the rest spread to 25 ms: a p75 genuinely
	// falls between two populated finite edges.
	zero := map[string]float64{"0.005": 0, "0.01": 0, "0.025": 0, "+Inf": 0}
	end := map[string]float64{"0.005": 50, "0.01": 75, "0.025": 100, "+Inf": 100}

	h, _ := HistogramWindow(mustParse(t, histoText(zero, 0)), mustParse(t, histoText(end, 1)), "h", nil)

	got := h.Quantile(0.75)
	if !got.Ok || got.Bound != Interpolated {
		t.Fatalf("got %+v, want an interpolated value", got)
	}
	// target 75 lands exactly on the le=0.01 cumulative boundary.
	if math.Abs(got.Seconds-0.01) > 1e-9 {
		t.Fatalf("seconds = %v, want 0.01", got.Seconds)
	}
}

func TestQuantileAboveTheLastFiniteEdge(t *testing.T) {
	zero := map[string]float64{"0.005": 0, "10": 0, "+Inf": 0}
	end := map[string]float64{"0.005": 1, "10": 1, "+Inf": 100}

	h, _ := HistogramWindow(mustParse(t, histoText(zero, 0)), mustParse(t, histoText(end, 900)), "h", nil)

	got := h.Quantile(0.95)
	if got.Ok {
		t.Fatal("a quantile past the last finite edge is unquantifiable")
	}
	if got.Bound != Above {
		t.Fatalf("bound = %q, want %q", got.Bound, Above)
	}
	if got.String() != "> 10.00 s" {
		t.Fatalf("rendered %q", got.String())
	}
}

func TestQuantileWithNoObservations(t *testing.T) {
	zero := map[string]float64{"0.005": 0, "+Inf": 0}
	h, _ := HistogramWindow(mustParse(t, histoText(zero, 0)), mustParse(t, histoText(zero, 0)), "h", nil)

	got := h.Quantile(0.95)
	if got.Ok || got.Bound != Unknown {
		t.Fatalf("got %+v, want Unknown", got)
	}
	if got.String() != "n/a" {
		t.Fatalf("rendered %q, want n/a", got.String())
	}
}

func TestHistogramWindowSubtractsBucketByBucket(t *testing.T) {
	start := map[string]float64{"0.005": 100, "0.01": 110, "+Inf": 120}
	end := map[string]float64{"0.005": 300, "0.01": 340, "+Inf": 400}

	h, ok := HistogramWindow(mustParse(t, histoText(start, 1)), mustParse(t, histoText(end, 5)), "h", nil)
	if !ok {
		t.Fatal("expected ok")
	}
	for le, want := range map[string]float64{"0.005": 200, "0.01": 230, "+Inf": 280} {
		if got := h.Buckets[le]; got != want {
			t.Fatalf("bucket %s = %v, want %v", le, got, want)
		}
	}
	if !h.HasSum || math.Abs(h.Sum-4) > 1e-9 {
		t.Fatalf("sum = %v/%v, want 4", h.Sum, h.HasSum)
	}
	if !h.HasCount || h.Count != 280 {
		t.Fatalf("count = %v/%v, want 280", h.Count, h.HasCount)
	}
}

func TestHistogramWindowAbsent(t *testing.T) {
	if _, ok := HistogramWindow(mustParse(t, ``), mustParse(t, ``), "h", nil); ok {
		t.Fatal("no buckets must not report ok")
	}
}

func TestHistogramMeanIsExact(t *testing.T) {
	start := map[string]float64{"0.005": 0, "+Inf": 100}
	end := map[string]float64{"0.005": 0, "+Inf": 300}
	h, _ := HistogramWindow(mustParse(t, histoText(start, 10)), mustParse(t, histoText(end, 110)), "h", nil)

	got, ok := h.Mean()
	if !ok {
		t.Fatal("expected ok")
	}
	if math.Abs(got-0.5) > 1e-12 {
		t.Fatalf("mean = %v, want 0.5", got)
	}
}

// --- against the real recorded exposition ---

// This is the run whose REPORT.md reported a client p95 of 933.01 ms against a
// runtime mean flow duration of 0.41 ms. See docs/LEARNINGS.md (L7, L8).
func TestRealOctoExposition(t *testing.T) {
	start := mustParseFile(t, "octo-0.5.0-window-start.prom")
	end := mustParseFile(t, "octo-0.5.0-window-end.prom")

	if got := end.LabelsOf("octo_build_info")["version"]; got != "0.5.0" {
		t.Fatalf("build_info version = %q", got)
	}

	completed := Labels{"flow": "page", "outcome": "completed"}
	h, ok := HistogramWindow(start, end, "octo_flow_duration_seconds", completed)
	if !ok {
		t.Fatal("expected a windowed histogram from the real fixture")
	}

	obs, ok := h.Observations()
	if !ok {
		t.Fatal("expected observations in the window")
	}
	if want := 682392.0 - 82884.0; obs != want {
		t.Fatalf("observations = %v, want %v", obs, want)
	}

	// The mean is exact and is the figure the report may print.
	mean, ok := h.Mean()
	if !ok {
		t.Fatal("expected a mean")
	}
	if ms := mean * 1000; math.Abs(ms-0.4118) > 0.001 {
		t.Fatalf("mean = %.4f ms, want ~0.41 ms", ms)
	}

	// 99.6% of the window's observations land in the lowest bucket, so no percentile
	// is supported. If this ever starts interpolating, the report will publish a
	// bucket-width artifact as Octo's latency.
	for _, q := range []float64{0.5, 0.95, 0.99} {
		got := h.Quantile(q)
		if got.Ok {
			t.Fatalf("p%v = %v s; the real histogram cannot support a percentile", q*100, got.Seconds)
		}
		if got.Bound != Below || got.Edge != 0.005 {
			t.Fatalf("p%v = %+v, want Below 0.005", q*100, got)
		}
	}

	// No work was shed: the outcomes the error gate reads must be zero, not missing.
	for _, outcome := range []string{"failed", "dropped"} {
		v, ok := CounterDelta(start, end, "octo_flow_messages_total", Labels{"flow": "page", "outcome": outcome})
		if !ok {
			continue // absent label combination: the event never occurred
		}
		if v != 0 {
			t.Fatalf("outcome %q advanced by %v in a run the client saw as clean", outcome, v)
		}
	}
}

func TestFormatSeconds(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0.0004118, "412 µs"},
		{0.005, "5.00 ms"},
		{0.93301, "933.01 ms"},
		{10, "10.00 s"},
	} {
		if got := formatSeconds(tc.in); got != tc.want {
			t.Fatalf("formatSeconds(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func mustParse(t *testing.T, text string) Exposition {
	t.Helper()
	e, err := ParseBytes([]byte(text))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return e
}

func mustParseFile(t *testing.T, name string) Exposition {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	e, err := ParseBytes(b)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return e
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }
