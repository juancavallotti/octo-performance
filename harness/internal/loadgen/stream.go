package loadgen

import (
	"bufio"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/series"
)

// Aggregator turns k6's per-observation CSV into one-second series as it arrives.
//
// This exists because of arithmetic. A sixty-second pass at sixteen thousand requests
// per second produces about a million requests, and k6 emits roughly a dozen rows per
// request: thirteen million lines, two gigabytes, for one cell out of seventy. The old
// lab's answer was to keep no time series at all, which is why it could not detect a
// steady window, could not see the achieved rate fall during a run, and could not tell
// a generator that grew its pool from one that did not.
//
// So the rows are never stored. They stream through a pipe, get folded into per-second
// buckets here, and what reaches disk is sixty rows. The raw count is recorded, so a
// reader can confirm nothing was dropped on the way.
type Aggregator struct {
	buckets map[int64]*bucket
	rows    int64
	// Unparsed counts rows that did not look like data. The header is one of them;
	// anything beyond that means the format moved.
	unparsed int64
}

type bucket struct {
	requests    float64
	failed      float64
	dropped     float64
	durationSum float64
	durationN   float64
	durationMax float64
	waitingSum  float64
	waitingN    float64
	vusMax      float64
	iterations  float64
}

// NewAggregator returns an empty aggregator.
func NewAggregator() *Aggregator { return &Aggregator{buckets: map[int64]*bucket{}} }

// Consume reads k6 CSV rows until r is exhausted.
//
// Only the first three columns are parsed, by hand rather than with a CSV reader.
// Metric names and numbers contain no commas and no quoting, and this runs thirteen
// million times per cell on the machine whose spare capacity the whole experiment
// depends on.
func (a *Aggregator) Consume(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)

	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		a.rows++

		name, rest, ok := strings.Cut(line, ",")
		if !ok {
			a.unparsed++
			continue
		}
		tsText, rest, ok := strings.Cut(rest, ",")
		if !ok {
			a.unparsed++
			continue
		}
		valText, _, _ := strings.Cut(rest, ",")

		ts, err := strconv.ParseInt(tsText, 10, 64)
		if err != nil {
			a.unparsed++ // the header row lands here, and so would a format change
			continue
		}
		val, err := strconv.ParseFloat(valText, 64)
		if err != nil {
			a.unparsed++
			continue
		}
		a.add(name, ts, val)
	}
	return sc.Err()
}

func (a *Aggregator) add(name string, ts int64, v float64) {
	b := a.buckets[ts]
	if b == nil {
		b = &bucket{}
		a.buckets[ts] = b
	}
	switch name {
	case "http_reqs":
		b.requests += v
	case "http_req_failed":
		b.failed += v // one row per request, 1 when it failed
	case "dropped_iterations":
		b.dropped += v
	case "iterations":
		b.iterations += v
	case "http_req_duration":
		b.durationSum += v
		b.durationN++
		if v > b.durationMax {
			b.durationMax = v
		}
	case "http_req_waiting":
		b.waitingSum += v
		b.waitingN++
	case "vus":
		if v > b.vusMax {
			b.vusMax = v
		}
	}
}

// Rows is how many CSV lines were seen, and Unparsed how many were not data. One
// unparsed row is the header; more than that means k6's output shape moved and the
// series is incomplete in a way that would otherwise be invisible.
func (a *Aggregator) Rows() (rows, unparsed int64) { return a.rows, a.unparsed }

// Frame returns the one-second series.
//
// Empty seconds are omitted rather than zero-filled. A second in which the generator
// produced nothing and a second it never reached are different claims, and only the
// series knows which happened.
func (a *Aggregator) Frame() series.Frame {
	secs := make([]int64, 0, len(a.buckets))
	for s := range a.buckets {
		secs = append(secs, s)
	}
	sort.Slice(secs, func(i, j int) bool { return secs[i] < secs[j] })

	var (
		rps        []series.Point
		latMean    []series.Point
		latMax     []series.Point
		waitMean   []series.Point
		vus        []series.Point
		dropped    []series.Point
		failed     []series.Point
		iterations []series.Point
	)
	for _, s := range secs {
		at := time.Unix(s, 0)
		b := a.buckets[s]

		rps = append(rps, series.Point{At: at, V: b.requests})
		iterations = append(iterations, series.Point{At: at, V: b.iterations})
		dropped = append(dropped, series.Point{At: at, V: b.dropped})
		failed = append(failed, series.Point{At: at, V: b.failed})
		if b.durationN > 0 {
			latMean = append(latMean, series.Point{At: at, V: b.durationSum / b.durationN})
			latMax = append(latMax, series.Point{At: at, V: b.durationMax})
		}
		if b.waitingN > 0 {
			waitMean = append(waitMean, series.Point{At: at, V: b.waitingSum / b.waitingN})
		}
		if b.vusMax > 0 {
			vus = append(vus, series.Point{At: at, V: b.vusMax})
		}
	}

	// A series with no points at all is left out entirely. Carrying an empty one
	// would let a run that produced nothing present the same shape as a run that
	// produced something, differing only in a length nobody checks.
	var f series.Frame
	for _, s := range []series.Series{
		{Name: "k6.rps", Unit: "requests/s", Points: rps},
		{Name: "k6.iterations", Unit: "iterations/s", Points: iterations},
		{Name: "k6.latency.mean", Unit: "milliseconds", Points: latMean},
		{Name: "k6.latency.max", Unit: "milliseconds", Points: latMax},
		{Name: "k6.waiting.mean", Unit: "milliseconds", Points: waitMean},
		{Name: "k6.vus", Unit: "count", Points: vus},
		{Name: "k6.dropped", Unit: "iterations/s", Points: dropped},
		{Name: "k6.failed", Unit: "requests/s", Points: failed},
	} {
		if len(s.Points) > 0 {
			f.Add(s)
		}
	}
	return f
}

// ObservedMaxVUs is the high-water mark of the virtual-user pool over the run.
//
// It is read from the series rather than from the summary so that it can also be read
// over a chosen window. A pool that grew only during a warm-up is a different finding
// from one that grew throughout the measured interval.
func (a *Aggregator) ObservedMaxVUs() int {
	max := 0.0
	for _, b := range a.buckets {
		if b.vusMax > max {
			max = b.vusMax
		}
	}
	return int(math.Round(max))
}
