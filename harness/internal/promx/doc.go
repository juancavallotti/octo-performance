// Package promx reads Prometheus exposition and differences two scrapes of it.
//
// It is a deliberately partial implementation. The harness reads one endpoint whose
// shape it knows; this package handles what that endpoint emits and ignores the parts
// of the specification it does not use (timestamps, exemplars) rather than pretending
// to be general.
//
// # Why differencing is the point
//
// Counters and histograms on /metrics are cumulative since process start, so a single
// scrape describes everything the process has ever done — including the smoke gate and
// the warm-up. Subtracting the scrape at the start of the measured window from the one
// at the end is what makes a number describe the window, and it does so exactly rather
// than by sampling.
//
// # The bounded quantile
//
// [Histogram.Quantile] returns a [Quantile], not a float64, and Seconds is meaningless
// unless Ok. That shape is load-bearing.
//
// Octo's flow-duration histogram uses Prometheus's default buckets, whose lowest edge
// is 5 ms, and the flows this lab measures complete in well under a millisecond. In the
// recorded fixture 99.6% of a window's observations fall in the first bucket.
// Interpolating there yields 2.5 ms for a p50 and 4.75 ms for a p95 — numbers that are
// a function of the bucket width and nothing else, and that would print as if they had
// been measured. Because no caller can obtain a bare float, nobody can publish that
// number by accident.
//
// See docs/LEARNINGS.md (L8). Nothing here does I/O beyond reading an io.Reader.
package promx
