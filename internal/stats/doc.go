// Package stats decides whether a difference between two arms is a difference.
//
// The old lab published `+5.1%` from baseline reps {11600.8, 10781.5, 9989.8} against
// tuned reps {10457.9, 11577.5, 11333.4} — distributions that almost entirely overlap —
// and printed `+305.1%` and `-0.0%` in the same table with identical typographic
// weight. Nothing in the code could tell those three cases apart, because the only
// summary that survived a run was a median.
//
// So [Compare] does not return a number. It returns a [Comparison] carrying the
// decision, the noise band it was measured against, and a populated Reason whenever it
// declines to call a delta significant. There is no way to ask this package "what is
// the delta" and receive an unqualified answer.
//
// # The noise band
//
// The band is the larger of a configured floor and the observed rep-to-rep spread
// within either arm. That rule is ported from lab/bin/compare-report.py, which had it
// right: a delta smaller than the spread of the measurements it is drawn from is not
// evidence of anything, whatever its sign.
//
// # Direction is not this package's business
//
// [Compare] reports Higher or Lower, never "regression" or "improvement". Higher
// throughput is good and higher latency is not, and only the caller knows which metric
// it is holding.
//
// # Steady state
//
// [DetectSteady] replaces a fixed warm-up sleep. It finds the earliest suffix of the
// load pass that is flat enough to measure, corroborated against a second series so
// that a plateau in throughput masking a still-climbing subject is not mistaken for
// steadiness. When no such window exists it says so rather than falling back, because
// a silent fallback to "the last 60 seconds" is how the previous harness measured the
// wrong window without anyone noticing.
//
// Nothing here does I/O. Randomness is injected so results are reproducible.
package stats
