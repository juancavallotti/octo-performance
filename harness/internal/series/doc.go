// Package series is the time-series primitive the rest of the harness reasons over.
//
// Every measurement source — k6's per-second output, the subject's /proc samples, the
// runner's host sampler, gauges scraped from /metrics — arrives as a [Series] of
// timestamped points, and everything downstream (windowing, steady-state detection,
// CPU accounting, the report's charts) is a function over that one shape.
//
// # Invariants
//
//   - Points are ordered by time, ascending. Constructors and loaders are responsible
//     for this; methods assume it and do not re-sort.
//   - Windows are inclusive at both ends. Series here are sparse 1 Hz samples, and a
//     measurement window wants the samples on its boundaries. Adjacent windows sharing
//     an endpoint is not a case the harness has.
//   - A cumulative counter that decreases means the process restarted. [Series.Rate]
//     reports how many times that happened rather than emitting a negative rate,
//     because a restart mid-window makes the window meaningless and the caller has to
//     decide, not this package.
//
// Nothing here does I/O.
package series
