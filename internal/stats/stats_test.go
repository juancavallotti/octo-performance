package stats

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/juancavallotti/octo-performance/internal/series"
)

func seeded() *rand.Rand { return rand.New(rand.NewPCG(1, 2)) }

func TestDescribe(t *testing.T) {
	s, ok := Describe([]float64{4, 1, 3, 2, 5})
	if !ok {
		t.Fatal("expected ok")
	}
	if s.N != 5 {
		t.Fatalf("N = %d", s.N)
	}
	if s.Median != 3 {
		t.Fatalf("median = %v", s.Median)
	}
	if s.Q1 != 2 || s.Q3 != 4 {
		t.Fatalf("quartiles = %v/%v, want 2/4", s.Q1, s.Q3)
	}
	if s.IQR != 2 {
		t.Fatalf("IQR = %v", s.IQR)
	}
	if s.Min != 1 || s.Max != 5 {
		t.Fatalf("range = %v..%v", s.Min, s.Max)
	}
	if s.MAD != 1 {
		t.Fatalf("MAD = %v, want 1", s.MAD)
	}
	// (5-1)/3 * 100
	if math.Abs(s.SpreadPct-133.333333) > 1e-4 {
		t.Fatalf("spread = %v%%", s.SpreadPct)
	}
}

func TestDescribeKeepsEveryRepetition(t *testing.T) {
	in := []float64{3, 1, 2}
	s, _ := Describe(in)
	if len(s.Values) != 3 || s.Values[0] != 3 {
		t.Fatalf("Values must preserve the supplied order: %v", s.Values)
	}
	in[0] = 99
	if s.Values[0] == 99 {
		t.Fatal("Describe aliased its input")
	}
}

func TestDescribeEmpty(t *testing.T) {
	if _, ok := Describe(nil); ok {
		t.Fatal("no measurements must not report ok")
	}
}

func TestQuantileInterpolatesLikeNumPy(t *testing.T) {
	xs := []float64{1, 2, 3, 4}
	for _, tc := range []struct{ q, want float64 }{
		{0, 1}, {0.25, 1.75}, {0.5, 2.5}, {0.75, 3.25}, {1, 4},
	} {
		if got := quantile(xs, tc.q); math.Abs(got-tc.want) > 1e-9 {
			t.Fatalf("q%v = %v, want %v", tc.q, got, tc.want)
		}
	}
}

func TestBootstrapIsDeterministicGivenTheSameSource(t *testing.T) {
	xs := []float64{10, 11, 12, 13, 14}
	a, ok1 := BootstrapMedianCI(xs, 0.95, 500, seeded())
	b, ok2 := BootstrapMedianCI(xs, 0.95, 500, seeded())
	if !ok1 || !ok2 {
		t.Fatal("expected ok")
	}
	if a != b {
		t.Fatalf("same seed produced different intervals: %+v vs %+v", a, b)
	}
	if a.Lo > a.Hi {
		t.Fatalf("inverted interval %+v", a)
	}
}

func TestBootstrapRefusesTooFewPoints(t *testing.T) {
	if _, ok := BootstrapMedianCI([]float64{1, 2}, 0.95, 100, seeded()); ok {
		t.Fatal("an interval from two points describes the two points")
	}
	if _, ok := BootstrapMedianCI([]float64{1, 2, 3}, 0.95, 100, nil); ok {
		t.Fatal("a nil source must not silently use global randomness")
	}
}

func TestIntervalOverlaps(t *testing.T) {
	for _, tc := range []struct {
		a, b Interval
		want bool
	}{
		{Interval{1, 5}, Interval{4, 9}, true},
		{Interval{1, 5}, Interval{5, 9}, true}, // touching counts
		{Interval{1, 5}, Interval{6, 9}, false},
		{Interval{6, 9}, Interval{1, 5}, false},
	} {
		if got := tc.a.Overlaps(tc.b); got != tc.want {
			t.Fatalf("%+v vs %+v = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// --- Compare: the decisions that stop a noise figure being published ---

// The exact numbers the old lab published as a +5.1% gain.
func TestCompareRefusesTheHistoricalFalsePositive(t *testing.T) {
	baseline := []float64{11600.8, 10781.5, 9989.8}
	tuned := []float64{10457.9, 11577.5, 11333.4}

	c := Compare(baseline, tuned, DefaultCompareConfig(seeded()))
	if c.Decision.Significant() {
		t.Fatalf("called a %.1f%% delta significant; the old lab published this as +5.1%%", c.DeltaPct)
	}
	if c.Reason == "" {
		t.Fatal("declining without a reason is what the old report did")
	}
	t.Logf("correctly declined: %s", c.Reason)
}

func TestCompareDecisions(t *testing.T) {
	tests := []struct {
		name     string
		a, b     []float64
		want     Decision
		inReason string
	}{
		{
			name: "tight arms, large delta",
			a:    []float64{1000, 1002, 998, 1001, 999},
			b:    []float64{1500, 1502, 1498, 1501, 1499},
			want: Higher,
		},
		{
			name: "tight arms, large drop",
			a:    []float64{1500, 1502, 1498, 1501, 1499},
			b:    []float64{1000, 1002, 998, 1001, 999},
			want: Lower,
		},
		{
			name:     "tiny delta between tight arms is still inside the floor",
			a:        []float64{1000, 1000, 1000, 1000, 1000},
			b:        []float64{1010, 1010, 1010, 1010, 1010},
			want:     Noise,
			inReason: "noise band",
		},
		{
			name:     "large delta but the reps are scattered",
			a:        []float64{500, 1000, 1500, 900, 1100},
			b:        []float64{600, 1200, 1700, 1000, 1300},
			want:     Noise,
			inReason: "noise band",
		},
		{
			name:     "not enough repetitions",
			a:        []float64{1000, 2000},
			b:        []float64{3000, 4000},
			want:     Insufficient,
			inReason: "valid repetitions per arm",
		},
		{
			name:     "an arm with nothing valid left",
			a:        nil,
			b:        []float64{1000, 1001, 1002},
			want:     Insufficient,
			inReason: "no valid repetitions",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Compare(tc.a, tc.b, DefaultCompareConfig(seeded()))
			if c.Decision != tc.want {
				t.Fatalf("decision = %q, want %q (delta %.1f%%, band %.1f%%, reason %q)",
					c.Decision, tc.want, c.DeltaPct, c.NoiseBandPct, c.Reason)
			}
			if tc.want != Higher && tc.want != Lower {
				if c.Reason == "" {
					t.Fatal("a non-significant decision must carry a reason")
				}
				if tc.inReason != "" && !contains(c.Reason, tc.inReason) {
					t.Fatalf("reason %q does not mention %q", c.Reason, tc.inReason)
				}
			}
		})
	}
}

// The band is the larger of the floor and either arm's own spread — ported from
// lab/bin/compare-report.py, which had this right.
func TestNoiseBandTakesTheWidestSpread(t *testing.T) {
	tight := []float64{1000, 1000, 1000, 1000, 1000}
	scattered := []float64{900, 1000, 1100, 950, 1050} // spread 20% of median

	c := Compare(tight, scattered, DefaultCompareConfig(seeded()))
	if c.NoiseBandPct < 19 {
		t.Fatalf("band = %.1f%%, must widen to the scattered arm's spread", c.NoiseBandPct)
	}
}

func TestNoiseBandNeverGoesBelowTheFloor(t *testing.T) {
	// Two arms with literally zero internal spread still cannot claim 0.5%.
	a := []float64{1000, 1000, 1000}
	b := []float64{1005, 1005, 1005}
	c := Compare(a, b, DefaultCompareConfig(seeded()))
	if c.NoiseBandPct != 2.0 {
		t.Fatalf("band = %v, want the 2%% floor", c.NoiseBandPct)
	}
	if c.Decision != Noise {
		t.Fatalf("decision = %q, want noise", c.Decision)
	}
}

func TestCompareIsDirectionNeutral(t *testing.T) {
	// The package must never say "regression": only the caller knows whether higher
	// is better for the metric in hand.
	c := Compare([]float64{100, 100, 100, 100, 100}, []float64{200, 200, 200, 200, 200},
		DefaultCompareConfig(seeded()))
	if c.Decision != Higher {
		t.Fatalf("decision = %q", c.Decision)
	}
}

// A property check: two samples drawn from the same distribution must not be called
// significant. This is the A/A campaign, in miniature.
func TestCompareDoesNotInventDifferencesBetweenIdenticalDistributions(t *testing.T) {
	rnd := rand.New(rand.NewPCG(42, 7))
	falsePositives := 0
	const trials = 200

	for i := 0; i < trials; i++ {
		a := make([]float64, 5)
		b := make([]float64, 5)
		for j := range a {
			a[j] = 1000 + rnd.NormFloat64()*20
			b[j] = 1000 + rnd.NormFloat64()*20
		}
		if Compare(a, b, DefaultCompareConfig(rand.New(rand.NewPCG(uint64(i), 9)))).Decision.Significant() {
			falsePositives++
		}
	}
	// With a 2% floor against a 2% sigma this should essentially never fire.
	if falsePositives > trials/20 {
		t.Fatalf("%d/%d false positives; the noise band is not holding", falsePositives, trials)
	}
	t.Logf("false positives: %d/%d", falsePositives, trials)
}

// --- OrderEffect ---

func TestOrderEffectFindsDrift(t *testing.T) {
	// The -metricson run's three reps decayed 8073 -> 7598 -> 7273 within one arm:
	// a textbook ordering artifact, published as a version characteristic.
	slope, r2, ok := OrderEffect([]int{1, 2, 3}, []float64{8073, 7598, 7273})
	if !ok {
		t.Fatal("expected ok")
	}
	if slope >= 0 {
		t.Fatalf("slope = %v%%/cell, expected a decay", slope)
	}
	if r2 < 0.9 {
		t.Fatalf("r2 = %v; a monotone decay should read as a strong trend", r2)
	}
}

func TestOrderEffectIgnoresZigzag(t *testing.T) {
	_, r2, ok := OrderEffect([]int{1, 2, 3, 4, 5}, []float64{1000, 1100, 1000, 1100, 1000})
	if !ok {
		t.Fatal("expected ok")
	}
	if r2 > 0.3 {
		t.Fatalf("r2 = %v; alternating values are not a trend", r2)
	}
}

func TestOrderEffectDegenerate(t *testing.T) {
	if _, _, ok := OrderEffect([]int{1, 2}, []float64{1, 2}); ok {
		t.Fatal("two points cannot establish an order effect")
	}
	if _, _, ok := OrderEffect([]int{1, 2, 3}, []float64{1, 2}); ok {
		t.Fatal("mismatched lengths must not report ok")
	}
	if _, _, ok := OrderEffect([]int{1, 1, 1}, []float64{1, 2, 3}); ok {
		t.Fatal("no variation in position cannot yield a slope")
	}
}

// --- DetectSteady ---

var t0 = time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

// ramp builds a 60 s per-second series: a warm-up climb for rampSecs, then a plateau
// at target with the given jitter.
func ramp(rampSecs int, target, jitter float64) series.Series {
	pts := make([]series.Point, 0, 60)
	for i := 0; i < 60; i++ {
		v := target
		if i < rampSecs {
			v = target * float64(i+1) / float64(rampSecs+1)
		} else if jitter != 0 {
			// Deterministic wobble, alternating sign.
			if i%2 == 0 {
				v = target * (1 + jitter)
			} else {
				v = target * (1 - jitter)
			}
		}
		pts = append(pts, series.Point{At: t0.Add(time.Duration(i) * time.Second), V: v})
	}
	return series.Series{Name: "rps", Points: pts}
}

func flat(target float64) series.Series { return ramp(0, target, 0) }

func TestDetectSteadySkipsTheWarmUp(t *testing.T) {
	w, ok := DetectSteady(ramp(10, 16000, 0.005), DefaultSteadyConfig())
	if !ok {
		t.Fatal("expected a window")
	}
	if w.Discarded < 8*time.Second {
		t.Fatalf("discarded only %v; the 10 s ramp should have been cut", w.Discarded)
	}
	if w.Duration() < 30*time.Second {
		t.Fatalf("window %v is below the minimum", w.Duration())
	}
	if w.CV > 0.05 {
		t.Fatalf("accepted a window with CV %v", w.CV)
	}
}

func TestDetectSteadyTakesTheEarliestQualifyingWindow(t *testing.T) {
	// A completely flat pass should be accepted whole, not trimmed to the calmest tail.
	w, ok := DetectSteady(flat(16000), DefaultSteadyConfig())
	if !ok {
		t.Fatal("expected a window")
	}
	if w.Discarded != 0 {
		t.Fatalf("discarded %v from an already-steady pass", w.Discarded)
	}
}

func TestDetectSteadyRejectsAContinuousRamp(t *testing.T) {
	// Never stabilises: climbing across the whole pass.
	climbing := ramp(59, 16000, 0)
	if w, ok := DetectSteady(climbing, DefaultSteadyConfig()); ok {
		t.Fatalf("accepted a window (%s) from a pass that never settles", w)
	}
}

func TestDetectSteadyRejectsAWildlyNoisyPass(t *testing.T) {
	if _, ok := DetectSteady(ramp(0, 16000, 0.40), DefaultSteadyConfig()); ok {
		t.Fatal("accepted a window with 40% swings")
	}
}

// A window can always be made flat by making it short enough. MinDuration is what
// stops the search finding steadiness in the last two seconds of a collapsing run.
func TestDetectSteadyWillNotShrinkItsWayToStability(t *testing.T) {
	cfg := DefaultSteadyConfig()
	pts := make([]series.Point, 0, 60)
	for i := 0; i < 60; i++ {
		v := 16000.0
		if i < 55 {
			v = 16000 * float64(i+1) / 56 // long ramp
		}
		pts = append(pts, series.Point{At: t0.Add(time.Duration(i) * time.Second), V: v})
	}
	if w, ok := DetectSteady(series.Series{Name: "rps", Points: pts}, cfg); ok {
		t.Fatalf("accepted a %v window; only the last 5 s were flat", w.Duration())
	}
}

// Throughput plateaus because the offered rate caps it, while the subject is still
// warming. From the client alone a saturated server and a settled one look the same.
func TestDetectSteadyRequiresTheCorroboratingSeriesToAgree(t *testing.T) {
	rps := flat(16000)

	climbingCPU := series.Series{Name: "cpu"}
	for i := 0; i < 60; i++ {
		climbingCPU.Points = append(climbingCPU.Points, series.Point{
			At: t0.Add(time.Duration(i) * time.Second),
			V:  100 + float64(i)*8, // still ramping hard throughout
		})
	}

	cfg := DefaultSteadyConfig()
	cfg.Corroborate = climbingCPU
	if w, ok := DetectSteady(rps, cfg); ok {
		t.Fatalf("accepted %s while the subject was still warming", w)
	}

	// The same throughput with settled CPU is accepted, and says so.
	steadyCPU := series.Series{Name: "cpu"}
	for i := 0; i < 60; i++ {
		steadyCPU.Points = append(steadyCPU.Points, series.Point{
			At: t0.Add(time.Duration(i) * time.Second), V: 300,
		})
	}
	cfg.Corroborate = steadyCPU
	w, ok := DetectSteady(rps, cfg)
	if !ok {
		t.Fatal("expected a window when both series are flat")
	}
	if !w.Corroborated {
		t.Fatal("window must record that it was corroborated")
	}
}

func TestDetectSteadyDegenerate(t *testing.T) {
	if _, ok := DetectSteady(series.Series{}, DefaultSteadyConfig()); ok {
		t.Fatal("empty input must not yield a window")
	}
	short := series.Series{Points: []series.Point{{At: t0, V: 1}, {At: t0.Add(time.Second), V: 1}}}
	if _, ok := DetectSteady(short, DefaultSteadyConfig()); ok {
		t.Fatal("two points must not yield a window")
	}
}

func TestFixedWindowIsRecordedForComparison(t *testing.T) {
	w, ok := FixedWindow(flat(16000), 10*time.Second)
	if !ok {
		t.Fatal("expected a fixed window")
	}
	if w.Discarded != 10*time.Second {
		t.Fatalf("discarded = %v", w.Discarded)
	}
	if w.Duration() != 49*time.Second {
		t.Fatalf("duration = %v, want 49s (t=10 through t=59)", w.Duration())
	}
	if _, ok := FixedWindow(flat(16000), 2*time.Minute); ok {
		t.Fatal("discarding more than the pass must not yield a window")
	}
}

func TestWindowString(t *testing.T) {
	w := Window{
		From: t0, To: t0.Add(45 * time.Second), Discarded: 15 * time.Second,
		CV: 0.012, SlopePctPerMin: -0.4, Corroborated: true,
	}
	got := w.String()
	for _, want := range []string{"45s", "15s", "0.012", "-0.40%/min", "corroborated"} {
		if !contains(got, want) {
			t.Fatalf("%q missing %q", got, want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
