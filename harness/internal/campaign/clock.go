package campaign

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/exec"
	"github.com/juancavallotti/octo-performance/harness/internal/gate"
)

// clockProbes is how many round trips one offset estimate takes.
//
// The minimum-RTT sample is the estimate, not the mean, because a round trip's excess
// over the true minimum is entirely one-directional delay — and the minimum of several
// samples is the one least contaminated by it. Averaging would fold scheduling jitter
// on both machines into the answer.
const clockProbes = 7

// measureClock estimates the subject's clock offset from the runner's.
//
// It matters at the resolution the harness actually works at. Everything is bucketed
// into one-second bins, and at 1 Hz half a second of skew moves a sample into the
// neighbouring bucket — which is precisely enough to invert "the CPU spike preceded the
// throughput drop". An unmeasured offset is not a small problem, it is an unbounded one,
// so a topology where it cannot be measured says so rather than assuming zero.
func measureClock(ctx context.Context, subject exec.Runner) gate.Clock {
	best := time.Duration(math.MaxInt64)
	var offset time.Duration
	measured := false

	for i := 0; i < clockProbes; i++ {
		if ctx.Err() != nil {
			break
		}
		sent := time.Now()
		res, err := subject.Run(ctx, exec.Cmd{
			// Nanoseconds where the coreutils date supports it, and a whole-second
			// fallback where it does not. A busybox date returning the literal "%N"
			// is caught below rather than parsed into a nonsense offset.
			Path: "/bin/sh",
			Args: []string{"-c", `date +%s.%N 2>/dev/null || date +%s`},
		})
		received := time.Now()
		if err != nil || res.ExitCode != 0 {
			continue
		}

		remote, ok := parseEpoch(strings.TrimSpace(string(res.Stdout)))
		if !ok {
			continue
		}

		rtt := received.Sub(sent)
		if rtt >= best {
			continue
		}
		best = rtt
		// The remote stamp was taken somewhere inside the round trip; the midpoint is
		// the least-wrong assumption, and the residual error is bounded by half the
		// round trip, which is recorded as the uncertainty.
		midpoint := sent.Add(rtt / 2)
		offset = remote.Sub(midpoint)
		measured = true
	}

	if !measured {
		return gate.Clock{}
	}
	return gate.Clock{
		Measured:      true,
		OffsetMs:      float64(offset) / float64(time.Millisecond),
		UncertaintyMs: float64(best/2) / float64(time.Millisecond),
	}
}

// parseEpoch reads a "seconds.nanoseconds" stamp.
func parseEpoch(s string) (time.Time, bool) {
	secText, nsText, hasNS := strings.Cut(s, ".")
	sec, err := strconv.ParseInt(secText, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	var ns int64
	if hasNS {
		// A date that does not understand %N prints it literally. Treating "N" as
		// zero nanoseconds would be right by accident; treating it as an error and
		// falling back to whole seconds is right on purpose.
		if len(nsText) != 9 {
			return time.Unix(sec, 0), true
		}
		if ns, err = strconv.ParseInt(nsText, 10, 64); err != nil {
			return time.Unix(sec, 0), true
		}
	}
	return time.Unix(sec, ns), true
}

// clockFor returns the offset evidence for a cell.
//
// On one machine there is one clock, and reporting the offset as unmeasured would raise
// a finding about a discrepancy that cannot exist — a gate that cries wolf on the local
// loop is a gate people learn to skip.
func (r *Runner) clockFor(ctx context.Context) gate.Clock {
	if r.cfg.Hosts.Runner == r.cfg.Hosts.Subject {
		return gate.Clock{Measured: true}
	}
	return measureClock(ctx, r.cfg.Hosts.Subject)
}

// driftBetween is how far two offset estimates moved apart.
//
// Taken at cell start and cell end, because an offset measured once says nothing about
// whether it held. A clock being disciplined by NTP mid-cell moves the two series
// relative to each other while every individual sample looks fine.
func driftBetween(start, end gate.Clock) gate.Clock {
	out := start
	if !start.Measured || !end.Measured {
		out.Measured = false
		return out
	}
	out.DriftMs = end.OffsetMs - start.OffsetMs
	// The wider of the two, because the drift is only as trustworthy as the less
	// certain endpoint.
	out.UncertaintyMs = math.Max(start.UncertaintyMs, end.UncertaintyMs)
	return out
}

// String renders the offset for a log line.
func clockText(c gate.Clock) string {
	if !c.Measured {
		return "unmeasured"
	}
	return fmt.Sprintf("%+.1fms ±%.1fms, drift %+.1fms", c.OffsetMs, c.UncertaintyMs, c.DriftMs)
}
