package series

import (
	"math"
	"sort"
	"time"
)

// Point is one observation.
type Point struct {
	At time.Time
	V  float64
}

// Series is a named, time-ordered sequence of observations.
//
// Unit is carried so the report never has to guess whether a number is seconds,
// bytes or requests per second. It is not interpreted by any method here.
type Series struct {
	Name   string
	Unit   string
	Points []Point
}

// New builds a Series, sorting the points by time. Use it at the edges — where
// samples arrive from a file or a channel and ordering is not yet guaranteed.
func New(name, unit string, pts []Point) Series {
	out := make([]Point, len(pts))
	copy(out, pts)
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return Series{Name: name, Unit: unit, Points: out}
}

// Len reports the number of points.
func (s Series) Len() int { return len(s.Points) }

// Empty reports whether the series carries no points.
func (s Series) Empty() bool { return len(s.Points) == 0 }

// Span returns the first and last timestamps. Ok is false for an empty series.
func (s Series) Span() (from, to time.Time, ok bool) {
	if len(s.Points) == 0 {
		return time.Time{}, time.Time{}, false
	}
	return s.Points[0].At, s.Points[len(s.Points)-1].At, true
}

// Duration is the wall time covered by the series.
func (s Series) Duration() time.Duration {
	from, to, ok := s.Span()
	if !ok {
		return 0
	}
	return to.Sub(from)
}

// Window returns the points falling inside [from, to], inclusive at both ends.
//
// The result keeps Name and Unit, so a windowed series is still self-describing in
// the report. A window that selects nothing yields an empty series rather than an
// error: "no samples in the measured window" is a finding for a gate to report, not
// a failure for this package to raise.
func (s Series) Window(from, to time.Time) Series {
	out := Series{Name: s.Name, Unit: s.Unit}
	for _, p := range s.Points {
		if p.At.Before(from) {
			continue
		}
		if p.At.After(to) {
			break
		}
		out.Points = append(out.Points, p)
	}
	return out
}

// Shift moves every timestamp by offset. This is how a subject-clock series is
// brought onto the runner's timeline once the offset has been measured; see the
// clock-skew reasoning in docs/LEARNINGS.md (L20).
func (s Series) Shift(offset time.Duration) Series {
	out := Series{Name: s.Name, Unit: s.Unit, Points: make([]Point, len(s.Points))}
	for i, p := range s.Points {
		out.Points[i] = Point{At: p.At.Add(offset), V: p.V}
	}
	return out
}

// Rate differentiates a cumulative counter into a per-second rate.
//
// Each emitted point is stamped at the later of the two samples it was derived from
// and carries the mean rate over the interval that preceded it. The first sample
// therefore produces no point — there is no interval before it, and inventing one is
// how sample-resources.py's predecessor understated short windows.
//
// resets counts the intervals where the counter went backwards. Those intervals are
// skipped rather than emitted as a negative rate: a counter only decreases when the
// process restarted, which makes the whole window suspect. Returning the count forces
// the caller to decide what that means instead of letting it vanish.
func (s Series) Rate() (rate Series, resets int) {
	rate = Series{Name: s.Name + ".rate", Unit: s.Unit + "/s"}
	for i := 1; i < len(s.Points); i++ {
		prev, cur := s.Points[i-1], s.Points[i]
		if cur.V < prev.V {
			resets++
			continue
		}
		dt := cur.At.Sub(prev.At).Seconds()
		if dt <= 0 {
			continue
		}
		rate.Points = append(rate.Points, Point{At: cur.At, V: (cur.V - prev.V) / dt})
	}
	return rate, resets
}

// Bucket aggregates into fixed-width buckets by mean, stamping each bucket at its
// start. Buckets containing no points are omitted rather than zero-filled, because a
// gap in sampling is not a measurement of zero.
//
// This is what turns k6's raw per-request output into the 1 s RPS bins that
// steady-state detection works over.
func (s Series) Bucket(width time.Duration) Series {
	out := Series{Name: s.Name, Unit: s.Unit}
	if len(s.Points) == 0 || width <= 0 {
		return out
	}
	origin := s.Points[0].At
	var (
		curIdx int64 = -1
		sum    float64
		n      int
	)
	flush := func() {
		if n > 0 {
			out.Points = append(out.Points, Point{
				At: origin.Add(time.Duration(curIdx) * width),
				V:  sum / float64(n),
			})
		}
	}
	for _, p := range s.Points {
		idx := int64(p.At.Sub(origin) / width)
		if idx != curIdx {
			flush()
			curIdx, sum, n = idx, 0, 0
		}
		sum += p.V
		n++
	}
	flush()
	return out
}

// Values returns the raw values in time order.
func (s Series) Values() []float64 {
	out := make([]float64, len(s.Points))
	for i, p := range s.Points {
		out[i] = p.V
	}
	return out
}

// Mean of the values. Ok is false for an empty series; callers must not treat a
// missing measurement as zero.
func (s Series) Mean() (float64, bool) {
	if len(s.Points) == 0 {
		return 0, false
	}
	var sum float64
	for _, p := range s.Points {
		sum += p.V
	}
	return sum / float64(len(s.Points)), true
}

// Sum of the values.
//
// Meaningful only for a series whose points are counts of things that happened in their
// bucket — dropped iterations, failed requests. Summing a gauge produces a number with
// no unit, which is why this returns ok rather than a bare zero: an absent series and a
// series of zeroes are different claims, and only one of them is evidence.
func (s Series) Sum() (float64, bool) {
	if len(s.Points) == 0 {
		return 0, false
	}
	var sum float64
	for _, p := range s.Points {
		sum += p.V
	}
	return sum, true
}

// Max of the values.
func (s Series) Max() (float64, bool) {
	if len(s.Points) == 0 {
		return 0, false
	}
	m := s.Points[0].V
	for _, p := range s.Points[1:] {
		if p.V > m {
			m = p.V
		}
	}
	return m, true
}

// Min of the values.
func (s Series) Min() (float64, bool) {
	if len(s.Points) == 0 {
		return 0, false
	}
	m := s.Points[0].V
	for _, p := range s.Points[1:] {
		if p.V < m {
			m = p.V
		}
	}
	return m, true
}

// Delta is last minus first. For a cumulative counter over a window this is the
// exact amount consumed in that window — which is how CPU-seconds are accounted,
// rather than by trusting a whole-process total that also includes start-up and
// warm-up.
func (s Series) Delta() (float64, bool) {
	if len(s.Points) < 2 {
		return 0, false
	}
	return s.Points[len(s.Points)-1].V - s.Points[0].V, true
}

// StdDev is the sample standard deviation (Bessel-corrected). Ok is false below two
// points.
func (s Series) StdDev() (float64, bool) {
	n := len(s.Points)
	if n < 2 {
		return 0, false
	}
	mean, _ := s.Mean()
	var ss float64
	for _, p := range s.Points {
		d := p.V - mean
		ss += d * d
	}
	return math.Sqrt(ss / float64(n-1)), true
}

// CV is the coefficient of variation, stddev over mean — the scale-free measure of
// how much a series wobbles, and the primary steady-state criterion.
//
// Ok is false when the mean is zero or near enough that the ratio is meaningless.
func (s Series) CV() (float64, bool) {
	mean, ok := s.Mean()
	if !ok || math.Abs(mean) < 1e-12 {
		return 0, false
	}
	sd, ok := s.StdDev()
	if !ok {
		return 0, false
	}
	return sd / math.Abs(mean), true
}

// Slope fits an ordinary least-squares line against time in seconds and returns the
// gradient per second together with r².
//
// Both matter and for different reasons: the gradient says whether throughput is
// still climbing or decaying, and r² says whether that trend is real or an artifact
// of two noisy endpoints. Steady-state detection requires a small gradient; the
// order-effect check requires a high r² before it will claim position explains
// anything.
func (s Series) Slope() (perSecond, r2 float64, ok bool) {
	n := len(s.Points)
	if n < 3 {
		return 0, 0, false
	}
	origin := s.Points[0].At
	var sx, sy float64
	xs := make([]float64, n)
	for i, p := range s.Points {
		xs[i] = p.At.Sub(origin).Seconds()
		sx += xs[i]
		sy += p.V
	}
	mx, my := sx/float64(n), sy/float64(n)

	var sxy, sxx, syy float64
	for i, p := range s.Points {
		dx, dy := xs[i]-mx, p.V-my
		sxy += dx * dy
		sxx += dx * dx
		syy += dy * dy
	}
	if sxx == 0 {
		return 0, 0, false
	}
	perSecond = sxy / sxx
	if syy == 0 {
		// A perfectly flat series is perfectly explained by a zero slope.
		return perSecond, 1, true
	}
	r := sxy / math.Sqrt(sxx*syy)
	return perSecond, r * r, true
}

// Frame is a set of series sharing a timeline, keyed by name.
type Frame struct {
	Series map[string]Series
}

// NewFrame builds a Frame from the given series, keyed by their Name.
func NewFrame(ss ...Series) Frame {
	f := Frame{Series: make(map[string]Series, len(ss))}
	for _, s := range ss {
		f.Series[s.Name] = s
	}
	return f
}

// Get returns the named series.
func (f Frame) Get(name string) (Series, bool) {
	s, ok := f.Series[name]
	return s, ok
}

// Add inserts or replaces a series, keyed by its Name.
func (f *Frame) Add(s Series) {
	if f.Series == nil {
		f.Series = map[string]Series{}
	}
	f.Series[s.Name] = s
}

// Names returns the series names in sorted order, so callers and golden files see a
// stable sequence.
func (f Frame) Names() []string {
	out := make([]string, 0, len(f.Series))
	for k := range f.Series {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Window applies [Series.Window] to every series.
func (f Frame) Window(from, to time.Time) Frame {
	out := Frame{Series: make(map[string]Series, len(f.Series))}
	for k, s := range f.Series {
		out.Series[k] = s.Window(from, to)
	}
	return out
}

// Shift applies [Series.Shift] to every series.
func (f Frame) Shift(offset time.Duration) Frame {
	out := Frame{Series: make(map[string]Series, len(f.Series))}
	for k, s := range f.Series {
		out.Series[k] = s.Shift(offset)
	}
	return out
}
