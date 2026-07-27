package promx

import (
	"fmt"
	"math"
	"sort"
	"strconv"
)

// CounterDelta reports how much a counter advanced between two scrapes.
//
// Ok is false when either scrape lacks the metric, and also when the counter went
// backwards. Counters do not decrease, so a negative result means the process
// restarted between the scrapes — and a restart mid-window makes the window
// meaningless. That must read as missing, not as an anomalous rate a caller might
// average in.
func CounterDelta(start, end Exposition, name string, match Labels) (float64, bool) {
	a, okA := start.Total(name, match)
	b, okB := end.Total(name, match)
	if !okA || !okB || b < a {
		return 0, false
	}
	return b - a, true
}

// CounterDeltaByLabel is [CounterDelta] split by one label. Keys whose counter went
// backwards are omitted, for the same reason.
func CounterDeltaByLabel(start, end Exposition, name, label string) map[string]float64 {
	a, b := start.ByLabel(name, label), end.ByLabel(name, label)
	out := make(map[string]float64, len(b))
	for k, endV := range b {
		if startV := a[k]; endV >= startV {
			out[k] = endV - startV
		}
	}
	return out
}

// Histogram is one histogram's observations over a window: buckets already
// differenced bucket-by-bucket, plus the windowed sum and count.
type Histogram struct {
	// Buckets maps the "le" label to the cumulative count within the window.
	Buckets map[string]float64
	// Sum and Count are the windowed totals. HasSum and HasCount are false when the
	// corresponding counter was absent or went backwards.
	Sum, Count       float64
	HasSum, HasCount bool
}

// HistogramWindow differences a histogram between two scrapes.
//
// Bucket-by-bucket subtraction is what makes a quantile computed from the result
// describe the measured window instead of the process's whole lifetime — which, by
// the time the measured pass runs, also contains the smoke gate and the warm-up.
//
// Ok is false when the end scrape carries no buckets for the metric.
func HistogramWindow(start, end Exposition, name string, match Labels) (Histogram, bool) {
	bucketsOf := func(e Exposition) map[string]float64 {
		out := map[string]float64{}
		for _, s := range e[name+"_bucket"] {
			le, ok := s.Labels["le"]
			if !ok || !s.Labels.Matches(match) {
				continue
			}
			out[le] += s.Value
		}
		return out
	}

	bEnd := bucketsOf(end)
	if len(bEnd) == 0 {
		return Histogram{}, false
	}
	bStart := bucketsOf(start)

	h := Histogram{Buckets: make(map[string]float64, len(bEnd))}
	for le, endV := range bEnd {
		h.Buckets[le] = math.Max(0, endV-bStart[le])
	}
	h.Sum, h.HasSum = CounterDelta(start, end, name+"_sum", match)
	h.Count, h.HasCount = CounterDelta(start, end, name+"_count", match)
	return h, true
}

// Observations is the number of observations in the window, taken from the +Inf
// bucket. Ok is false when there were none — a window with no observations cannot
// support a quantile.
func (h Histogram) Observations() (float64, bool) {
	n, ok := h.Buckets["+Inf"]
	if !ok || n <= 0 {
		return 0, false
	}
	return n, true
}

// Mean is the histogram's mean over the window: sum divided by count.
//
// Unlike a quantile, this is exact — both are counters and both were differenced —
// which is why it is often the only server-side latency figure worth printing.
func (h Histogram) Mean() (float64, bool) {
	if !h.HasSum || !h.HasCount || h.Count <= 0 {
		return 0, false
	}
	return h.Sum / h.Count, true
}

// Bound says how much the buckets support a quantile.
type Bound string

const (
	// Interpolated: the quantile fell between two finite edges that both carry real
	// counts. This is histogram_quantile's normal case.
	Interpolated Bound = "interpolated"
	// Below: it fell inside the lowest bucket. Every observation that matters is
	// somewhere in (0, Edge] and the histogram cannot say where.
	Below Bound = "below"
	// Above: it fell in +Inf, past the last finite edge. Unquantifiable.
	Above Bound = "above"
	// Unknown: there was nothing to compute from.
	Unknown Bound = "unknown"
)

// Quantile is a quantile read out of a windowed histogram, together with how much the
// buckets actually support it.
//
// Seconds is meaningless unless Ok. That is the entire reason this is a struct: for
// this lab the Below case is not a corner case, it is the common one, and a bare
// float64 return would let a bucket-width artifact be printed as a measurement. See
// the package doc and docs/LEARNINGS.md (L8).
type Quantile struct {
	Seconds float64
	Ok      bool
	Bound   Bound
	// Edge is the bucket boundary the answer is expressed against: the upper bound
	// for Below, the containing bucket's edge for Interpolated, the last finite edge
	// for Above.
	Edge    float64
	HasEdge bool
}

// Quantile computes q (0..1) from the windowed buckets.
func (h Histogram) Quantile(q float64) Quantile {
	total, ok := h.Observations()
	if !ok {
		return Quantile{Bound: Unknown}
	}

	type edge struct {
		le         float64
		cumulative float64
	}
	edges := make([]edge, 0, len(h.Buckets))
	for le, c := range h.Buckets {
		if le == "+Inf" {
			continue
		}
		v, err := strconv.ParseFloat(le, 64)
		if err != nil {
			continue
		}
		edges = append(edges, edge{le: v, cumulative: c})
	}
	if len(edges) == 0 {
		return Quantile{Bound: Unknown}
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].le < edges[j].le })

	target := q * total
	prevEdge, prevCount := 0.0, 0.0
	for i, e := range edges {
		if e.cumulative < target {
			prevEdge, prevCount = e.le, e.cumulative
			continue
		}
		if i == 0 {
			// Everything up to the quantile sits in the first bucket. The true value
			// is in (0, le] and the histogram has no more to say. Interpolating here
			// would report half the bucket width as if it had been observed.
			return Quantile{Bound: Below, Edge: e.le, HasEdge: true}
		}
		span := e.cumulative - prevCount
		if span <= 0 {
			// The containing bucket is empty; the boundary itself is the best answer.
			return Quantile{Seconds: e.le, Ok: true, Bound: Interpolated, Edge: e.le, HasEdge: true}
		}
		secs := prevEdge + (e.le-prevEdge)*(target-prevCount)/span
		return Quantile{Seconds: secs, Ok: true, Bound: Interpolated, Edge: e.le, HasEdge: true}
	}
	return Quantile{Bound: Above, Edge: edges[len(edges)-1].le, HasEdge: true}
}

// String renders the quantile the way a report must show it: a bound when that is all
// the histogram supports, never a number standing in for one.
func (q Quantile) String() string {
	switch q.Bound {
	case Interpolated:
		return formatSeconds(q.Seconds)
	case Below:
		return "< " + formatSeconds(q.Edge)
	case Above:
		return "> " + formatSeconds(q.Edge)
	default:
		return "n/a"
	}
}

func formatSeconds(s float64) string {
	switch {
	case s < 1e-3:
		return fmt.Sprintf("%.0f µs", s*1e6)
	case s < 1:
		return fmt.Sprintf("%.2f ms", s*1e3)
	default:
		return fmt.Sprintf("%.2f s", s)
	}
}
