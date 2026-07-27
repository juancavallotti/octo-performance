package stats

import (
	"math"
	"math/rand/v2"
	"sort"
)

// Interval is a closed range, used for confidence intervals.
type Interval struct{ Lo, Hi float64 }

// Overlaps reports whether two intervals share any value. Two arms whose intervals
// overlap have not been shown to differ.
func (i Interval) Overlaps(o Interval) bool { return i.Lo <= o.Hi && o.Lo <= i.Hi }

// Summary describes one arm's repetitions.
//
// Median rather than mean throughout: a single thermally-throttled repetition should
// move the reported figure as little as possible, and the lab's own rule has always
// been to report the median and keep every rep.
type Summary struct {
	N           int
	Median      float64
	Q1, Q3, IQR float64
	Min, Max    float64
	MAD         float64 // median absolute deviation
	SpreadPct   float64 // (max-min)/median, as a percentage: this arm's own noise
	CI          Interval
	HasCI       bool
	Values      []float64 // every repetition, in the order supplied
}

// Describe summarises a set of repetitions. Ok is false for an empty input; a caller
// must not treat "no measurements" as a measurement.
//
// Quantiles use linear interpolation between order statistics (the R type-7 / NumPy
// default), so the numbers match what anyone checking the work in a notebook will get.
func Describe(xs []float64) (Summary, bool) {
	if len(xs) == 0 {
		return Summary{}, false
	}
	vals := append([]float64(nil), xs...)

	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)

	s := Summary{
		N:      len(sorted),
		Median: quantile(sorted, 0.5),
		Q1:     quantile(sorted, 0.25),
		Q3:     quantile(sorted, 0.75),
		Min:    sorted[0],
		Max:    sorted[len(sorted)-1],
		Values: vals,
	}
	s.IQR = s.Q3 - s.Q1

	devs := make([]float64, len(sorted))
	for i, v := range sorted {
		devs[i] = math.Abs(v - s.Median)
	}
	sort.Float64s(devs)
	s.MAD = quantile(devs, 0.5)

	if s.Median != 0 {
		s.SpreadPct = (s.Max - s.Min) / math.Abs(s.Median) * 100
	}
	return s, true
}

// quantile interpolates linearly between order statistics. Input must be sorted.
func quantile(sorted []float64, q float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	pos := q * float64(n-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

// BootstrapMedianCI estimates a confidence interval for the median by resampling.
//
// The median's sampling distribution has no useful closed form at n=5, which is the
// n this lab actually runs, so resampling is the honest option. rnd is injected rather
// than taken from the global source: a report that renders differently on each run is
// not reviewable, and a test that flakes is not a test.
//
// Ok is false below three values — an interval derived from two points describes the
// two points.
func BootstrapMedianCI(xs []float64, level float64, iters int, rnd *rand.Rand) (Interval, bool) {
	if len(xs) < 3 || iters <= 0 || level <= 0 || level >= 1 || rnd == nil {
		return Interval{}, false
	}
	medians := make([]float64, iters)
	sample := make([]float64, len(xs))
	for i := range medians {
		for j := range sample {
			sample[j] = xs[rnd.IntN(len(xs))]
		}
		sort.Float64s(sample)
		medians[i] = quantile(sample, 0.5)
	}
	sort.Float64s(medians)

	tail := (1 - level) / 2
	return Interval{
		Lo: quantile(medians, tail),
		Hi: quantile(medians, 1-tail),
	}, true
}

// OrderEffect regresses a metric against execution position.
//
// Interleaving arms is what makes this measurable, and measuring it is what turns
// "we interleaved, so trust us" into a number a reader can check. It returns the
// gradient as a percentage of the mean per cell, plus r² — because a slope drawn
// through noise is not a trend, and only r² can say which one this is.
//
// Ok is false below three points or when the values have no usable mean.
func OrderEffect(ordinals []int, values []float64) (slopePctPerCell, r2 float64, ok bool) {
	n := len(ordinals)
	if n != len(values) || n < 3 {
		return 0, 0, false
	}
	var sx, sy float64
	for i := range values {
		sx += float64(ordinals[i])
		sy += values[i]
	}
	mx, my := sx/float64(n), sy/float64(n)
	if math.Abs(my) < 1e-12 {
		return 0, 0, false
	}

	var sxy, sxx, syy float64
	for i := range values {
		dx, dy := float64(ordinals[i])-mx, values[i]-my
		sxy += dx * dy
		sxx += dx * dx
		syy += dy * dy
	}
	if sxx == 0 {
		return 0, 0, false
	}
	slope := sxy / sxx
	if syy == 0 {
		return 0, 1, true
	}
	r := sxy / math.Sqrt(sxx*syy)
	return slope / math.Abs(my) * 100, r * r, true
}
