package series

import (
	"math"
	"testing"
	"time"
)

var epoch = time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

// at builds a timestamp n seconds into the test epoch.
func at(n float64) time.Time {
	return epoch.Add(time.Duration(n * float64(time.Second)))
}

// build makes a series from (second, value) pairs.
func build(name string, pairs ...[2]float64) Series {
	pts := make([]Point, len(pairs))
	for i, p := range pairs {
		pts[i] = Point{At: at(p[0]), V: p[1]}
	}
	return Series{Name: name, Points: pts}
}

func TestNewSortsByTime(t *testing.T) {
	s := New("x", "", []Point{
		{At: at(3), V: 3},
		{At: at(1), V: 1},
		{At: at(2), V: 2},
	})
	want := []float64{1, 2, 3}
	if got := s.Values(); !eqFloats(got, want) {
		t.Fatalf("New did not sort: got %v want %v", got, want)
	}
}

func TestNewCopiesInput(t *testing.T) {
	in := []Point{{At: at(1), V: 1}}
	s := New("x", "", in)
	in[0].V = 99
	if s.Points[0].V != 1 {
		t.Fatal("New aliased its input; mutating the caller's slice changed the series")
	}
}

func TestWindowIsInclusiveAtBothEnds(t *testing.T) {
	s := build("x", [2]float64{0, 0}, [2]float64{1, 1}, [2]float64{2, 2}, [2]float64{3, 3})

	tests := []struct {
		name     string
		from, to time.Time
		want     []float64
	}{
		{"exact bounds include both endpoints", at(1), at(2), []float64{1, 2}},
		{"whole span", at(0), at(3), []float64{0, 1, 2, 3}},
		{"single point", at(2), at(2), []float64{2}},
		{"wider than the data", at(-5), at(10), []float64{0, 1, 2, 3}},
		{"selects nothing", at(10), at(20), nil},
		{"inverted window selects nothing", at(3), at(1), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.Window(tc.from, tc.to).Values(); !eqFloats(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestWindowPreservesIdentity(t *testing.T) {
	s := Series{Name: "cpu", Unit: "seconds", Points: []Point{{At: at(1), V: 1}}}
	w := s.Window(at(0), at(2))
	if w.Name != "cpu" || w.Unit != "seconds" {
		t.Fatalf("window lost identity: %q/%q", w.Name, w.Unit)
	}
}

// Rate is the mechanism that replaces reading an instantaneous %cpu, which on a
// short benchmark window reports a decaying lifetime average and understates the
// truth. See docs/LEARNINGS.md.
func TestRateDifferentiatesACumulativeCounter(t *testing.T) {
	// A counter climbing 2 units/s, sampled every second.
	s := build("cpu", [2]float64{0, 10}, [2]float64{1, 12}, [2]float64{2, 14}, [2]float64{3, 16})

	rate, resets := s.Rate()
	if resets != 0 {
		t.Fatalf("unexpected resets: %d", resets)
	}
	if got, want := rate.Values(), []float64{2, 2, 2}; !eqFloats(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	// The first sample has no preceding interval, so it yields no point.
	if rate.Len() != s.Len()-1 {
		t.Fatalf("expected %d rate points, got %d", s.Len()-1, rate.Len())
	}
	// Each point is stamped at the later sample of the pair it came from.
	if !rate.Points[0].At.Equal(at(1)) {
		t.Fatalf("first rate point stamped %v, want %v", rate.Points[0].At, at(1))
	}
}

func TestRateReportsCounterResetsInsteadOfNegativeRates(t *testing.T) {
	// The subject restarted between t=2 and t=3: the counter drops.
	s := build("cpu", [2]float64{0, 10}, [2]float64{1, 12}, [2]float64{2, 14}, [2]float64{3, 1}, [2]float64{4, 3})

	rate, resets := s.Rate()
	if resets != 1 {
		t.Fatalf("resets = %d, want 1", resets)
	}
	for _, p := range rate.Points {
		if p.V < 0 {
			t.Fatalf("emitted a negative rate %v; a restart must be reported, not averaged in", p.V)
		}
	}
	if got, want := rate.Values(), []float64{2, 2, 2}; !eqFloats(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRateHandlesDegenerateInput(t *testing.T) {
	tests := []struct {
		name string
		in   Series
	}{
		{"empty", Series{}},
		{"single point", build("x", [2]float64{0, 5})},
		{"duplicate timestamps", build("x", [2]float64{1, 5}, [2]float64{1, 7})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rate, _ := tc.in.Rate()
			if !rate.Empty() {
				t.Fatalf("expected no rate points, got %v", rate.Values())
			}
		})
	}
}

func TestBucketAggregatesByMeanAndOmitsGaps(t *testing.T) {
	// Two points in the first second, none in the second, one in the third.
	s := build("rps",
		[2]float64{0.0, 10}, [2]float64{0.5, 20},
		[2]float64{2.0, 40},
	)
	b := s.Bucket(time.Second)

	if got, want := b.Values(), []float64{15, 40}; !eqFloats(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	// A sampling gap is not a measurement of zero, so the empty bucket is absent.
	if b.Len() != 2 {
		t.Fatalf("expected 2 buckets, got %d — an empty bucket must be omitted, not zero-filled", b.Len())
	}
	if !b.Points[1].At.Equal(at(2)) {
		t.Fatalf("bucket stamped %v, want its start %v", b.Points[1].At, at(2))
	}
}

func TestBucketDegenerate(t *testing.T) {
	s := build("x", [2]float64{0, 1})
	if got := s.Bucket(0); !got.Empty() {
		t.Fatal("non-positive width must yield nothing")
	}
	if got := (Series{}).Bucket(time.Second); !got.Empty() {
		t.Fatal("empty input must yield nothing")
	}
}

func TestDeltaIsLastMinusFirst(t *testing.T) {
	s := build("cpu", [2]float64{0, 8.61}, [2]float64{60, 57.79})
	got, ok := s.Delta()
	if !ok {
		t.Fatal("expected ok")
	}
	if math.Abs(got-49.18) > 1e-9 {
		t.Fatalf("delta = %v, want 49.18", got)
	}
	if _, ok := build("cpu", [2]float64{0, 1}).Delta(); ok {
		t.Fatal("a single point cannot yield a delta")
	}
}

func TestEmptySeriesNeverReportsZeroAsAMeasurement(t *testing.T) {
	var s Series
	for _, tc := range []struct {
		name string
		fn   func() (float64, bool)
	}{
		{"Mean", s.Mean},
		{"Max", s.Max},
		{"Min", s.Min},
		{"Delta", s.Delta},
		{"StdDev", s.StdDev},
		{"CV", s.CV},
	} {
		if _, ok := tc.fn(); ok {
			t.Fatalf("%s reported ok on an empty series; a missing measurement must not read as zero", tc.name)
		}
	}
}

func TestStdDevIsSampleCorrected(t *testing.T) {
	s := build("x", [2]float64{0, 2}, [2]float64{1, 4}, [2]float64{2, 4}, [2]float64{3, 4},
		[2]float64{4, 5}, [2]float64{5, 5}, [2]float64{6, 7}, [2]float64{7, 9})
	got, ok := s.StdDev()
	if !ok {
		t.Fatal("expected ok")
	}
	// Population sd is 2; Bessel-corrected sd over n=8 is sqrt(32/7).
	if want := math.Sqrt(32.0 / 7.0); math.Abs(got-want) > 1e-9 {
		t.Fatalf("stddev = %v, want %v (sample, not population)", got, want)
	}
}

func TestCVIsScaleFree(t *testing.T) {
	small := build("x", [2]float64{0, 10}, [2]float64{1, 12}, [2]float64{2, 8})
	large := build("x", [2]float64{0, 1000}, [2]float64{1, 1200}, [2]float64{2, 800})

	a, ok1 := small.CV()
	b, ok2 := large.CV()
	if !ok1 || !ok2 {
		t.Fatal("expected ok")
	}
	if math.Abs(a-b) > 1e-12 {
		t.Fatalf("CV is not scale-free: %v vs %v", a, b)
	}
}

func TestCVRejectsAZeroMean(t *testing.T) {
	s := build("x", [2]float64{0, -1}, [2]float64{1, 1})
	if _, ok := s.CV(); ok {
		t.Fatal("CV around a zero mean is meaningless and must not report ok")
	}
}

func TestSlope(t *testing.T) {
	tests := []struct {
		name      string
		in        Series
		wantSlope float64
		wantR2    float64
		wantOK    bool
	}{
		{
			name:      "clean linear climb",
			in:        build("x", [2]float64{0, 0}, [2]float64{1, 2}, [2]float64{2, 4}, [2]float64{3, 6}),
			wantSlope: 2, wantR2: 1, wantOK: true,
		},
		{
			name:      "perfectly flat is perfectly explained",
			in:        build("x", [2]float64{0, 5}, [2]float64{1, 5}, [2]float64{2, 5}, [2]float64{3, 5}),
			wantSlope: 0, wantR2: 1, wantOK: true,
		},
		{
			name:      "decay",
			in:        build("x", [2]float64{0, 10}, [2]float64{1, 8}, [2]float64{2, 6}, [2]float64{3, 4}),
			wantSlope: -2, wantR2: 1, wantOK: true,
		},
		{
			name:   "too few points to fit",
			in:     build("x", [2]float64{0, 1}, [2]float64{1, 2}),
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			slope, r2, ok := tc.in.Slope()
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if math.Abs(slope-tc.wantSlope) > 1e-9 {
				t.Fatalf("slope = %v, want %v", slope, tc.wantSlope)
			}
			if math.Abs(r2-tc.wantR2) > 1e-9 {
				t.Fatalf("r2 = %v, want %v", r2, tc.wantR2)
			}
		})
	}
}

func TestSlopeSeparatesTrendFromNoise(t *testing.T) {
	// Same endpoints, very different stories. r2 is what tells them apart, which is
	// why the order-effect check requires it before claiming position explains a delta.
	noisy := build("x", [2]float64{0, 0}, [2]float64{1, 9}, [2]float64{2, 1}, [2]float64{3, 8}, [2]float64{4, 2})
	_, r2, ok := noisy.Slope()
	if !ok {
		t.Fatal("expected ok")
	}
	if r2 > 0.3 {
		t.Fatalf("r2 = %v; a zigzag must not read as a strong trend", r2)
	}
}

func TestShiftMovesEveryTimestamp(t *testing.T) {
	s := build("x", [2]float64{0, 1}, [2]float64{1, 2})
	got := s.Shift(-3200 * time.Millisecond)
	if !got.Points[0].At.Equal(at(0).Add(-3200 * time.Millisecond)) {
		t.Fatalf("shift did not apply: %v", got.Points[0].At)
	}
	if got.Points[0].V != 1 {
		t.Fatal("shift must not touch values")
	}
	if !s.Points[0].At.Equal(at(0)) {
		t.Fatal("shift mutated the receiver")
	}
}

func TestFrame(t *testing.T) {
	f := NewFrame(
		build("rps", [2]float64{0, 100}, [2]float64{1, 110}),
		build("cpu", [2]float64{0, 1}, [2]float64{1, 2}),
	)

	if got, want := f.Names(), []string{"cpu", "rps"}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Names = %v, want sorted %v", got, want)
	}
	if _, ok := f.Get("rps"); !ok {
		t.Fatal("Get missed a present series")
	}
	if _, ok := f.Get("absent"); ok {
		t.Fatal("Get invented a series")
	}

	w := f.Window(at(1), at(1))
	for _, n := range w.Names() {
		s, _ := w.Get(n)
		if s.Len() != 1 {
			t.Fatalf("%s: window applied unevenly, got %d points", n, s.Len())
		}
	}

	sh := f.Shift(time.Second)
	s, _ := sh.Get("rps")
	if !s.Points[0].At.Equal(at(1)) {
		t.Fatalf("frame shift did not apply: %v", s.Points[0].At)
	}
}

func TestFrameAddOnZeroValue(t *testing.T) {
	var f Frame
	f.Add(build("x", [2]float64{0, 1}))
	if _, ok := f.Get("x"); !ok {
		t.Fatal("Add must work on a zero-value Frame")
	}
}

func eqFloats(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(a[i]-b[i]) > 1e-9 {
			return false
		}
	}
	return true
}
