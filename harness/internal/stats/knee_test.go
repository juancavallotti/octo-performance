package stats

import (
	"strings"
	"testing"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/series"
)

var rampOrigin = time.Unix(1785200000, 0)

// ramp builds the four series a capacity probe produces, from a per-rung description.
func capacityRamp(t *testing.T, dwell time.Duration, rungs []struct {
	Offered, Achieved, Dropped, Failed, LatencyMs float64
}) ([]Step, series.Series, series.Series, series.Series, series.Series) {
	t.Helper()

	var (
		steps   []Step
		rps     = series.Series{Name: "k6.rps"}
		dropped = series.Series{Name: "k6.dropped"}
		failed  = series.Series{Name: "k6.failed"}
		latency = series.Series{Name: "k6.latency.mean"}
	)
	at := rampOrigin
	for _, r := range rungs {
		// Stopping an instant short of the next rung, exactly as StepWindows does:
		// Series.Window is a closed interval, so abutting windows would share the
		// next rung's first sample.
		steps = append(steps, Step{Offered: r.Offered, From: at, To: at.Add(dwell - 1)})
		for s := time.Duration(0); s < dwell; s += time.Second {
			ts := at.Add(s)
			rps.Points = append(rps.Points, series.Point{At: ts, V: r.Achieved})
			// Counts are per-second, so a rung's total is spread across its seconds.
			perSecond := r.Dropped / dwell.Seconds()
			dropped.Points = append(dropped.Points, series.Point{At: ts, V: perSecond})
			failed.Points = append(failed.Points, series.Point{At: ts, V: r.Failed / dwell.Seconds()})
			latency.Points = append(latency.Points, series.Point{At: ts, V: r.LatencyMs})
		}
		at = at.Add(dwell)
	}
	return steps, rps, dropped, failed, latency
}

type rung = struct{ Offered, Achieved, Dropped, Failed, LatencyMs float64 }

func TestAKneeIsTheLastRungTheSystemHeld(t *testing.T) {
	steps, rps, dropped, failed, latency := capacityRamp(t, 10*time.Second, []rung{
		{1000, 1000, 0, 0, 0.5},
		{2000, 2000, 0, 0, 0.6},
		{3000, 3000, 0, 0, 0.7},
		{4000, 2400, 9161, 0, 4.0}, // fold-over
		{5000, 2100, 24000, 0, 9.0},
	})

	k := FindKnee(steps, rps, dropped, failed, latency, DefaultKnee())
	if !k.Found {
		t.Fatalf("no knee found: %s", k.Note)
	}
	if k.Rate != 3000 {
		t.Errorf("knee at %.0f, want 3000", k.Rate)
	}
	if k.FailedAt != 4000 {
		t.Errorf("failed at %.0f, want 4000", k.FailedAt)
	}
	// It stops at the first failure rather than walking the whole ramp: a rung past
	// the knee tells you nothing except how the collapse progressed.
	if len(k.Steps) != 4 {
		t.Errorf("evaluated %d rungs, want to stop at the 4th", len(k.Steps))
	}
}

func TestTheReasonARungFailedIsNamed(t *testing.T) {
	// A bend in a line is not a diagnosis. Which of the three conditions gave way
	// decides what the number means, and the three mean genuinely different things.
	for _, tc := range []struct {
		name string
		rung rung
		want string
	}{
		{"shed iterations", rung{4000, 3990, 412, 0, 0.5}, "shed 412 iterations"},
		{"could not keep up", rung{4000, 2400, 0, 0, 0.5}, "achieved 2400 of 4000 offered"},
		{"queued", rung{4000, 4000, 0, 0, 9.0}, "mean latency 9.00ms"},
		{"failed requests", rung{4000, 4000, 0, 88, 0.5}, "88 requests failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps, rps, dropped, failed, latency := capacityRamp(t, 10*time.Second, []rung{
				{1000, 1000, 0, 0, 0.5},
				tc.rung,
			})
			k := FindKnee(steps, rps, dropped, failed, latency, DefaultKnee())
			if !k.Found {
				t.Fatalf("no knee: %s", k.Note)
			}
			last := k.Steps[len(k.Steps)-1]
			if !strings.Contains(last.Why, tc.want) {
				t.Errorf("reason was %q, want it to mention %q", last.Why, tc.want)
			}
		})
	}
}

func TestARampThatHeldEverythingReportsALowerBoundNotAKnee(t *testing.T) {
	// The distinction the old lab could not make. A ramp that never folded over has
	// established that capacity is at least the peak, and calling that "the knee" turns
	// a bound into a measurement — and then a campaign runs at half of a number that
	// was never the ceiling.
	steps, rps, dropped, failed, latency := capacityRamp(t, 10*time.Second, []rung{
		{1000, 1000, 0, 0, 0.5},
		{2000, 2000, 0, 0, 0.5},
		{3000, 3000, 0, 0, 0.6},
	})

	k := FindKnee(steps, rps, dropped, failed, latency, DefaultKnee())
	if k.Found {
		t.Error("a ramp that held every rung reported a knee")
	}
	if k.Rate != 3000 {
		t.Errorf("lower bound %.0f, want 3000", k.Rate)
	}
	if !strings.Contains(k.Note, "lower bound") {
		t.Errorf("the note does not say it is a bound: %q", k.Note)
	}
}

func TestARampThatFailedItsFirstRungHasNoRate(t *testing.T) {
	// Nothing was held, so there is no rate to run at. Reporting the first rung would
	// be reporting a rate that demonstrably did not work.
	steps, rps, dropped, failed, latency := capacityRamp(t, 10*time.Second, []rung{
		{1000, 120, 4400, 0, 70.0},
	})

	k := FindKnee(steps, rps, dropped, failed, latency, DefaultKnee())
	if !k.Found {
		t.Fatal("a rung that failed was not reported")
	}
	if k.Rate != 0 {
		t.Errorf("rate %.0f, want 0 — nothing was held", k.Rate)
	}
	if k.Chosen(0.5) != 0 {
		t.Errorf("a knee with no held rung produced a rate of %d", k.Chosen(0.5))
	}
	if !strings.Contains(k.Note, "first rung") {
		t.Errorf("note does not explain: %q", k.Note)
	}
}

func TestARungWithNoObservationsIsNotCalledHeld(t *testing.T) {
	// The silent-success failure mode: an empty window looks like zero drops, zero
	// failures and a ratio of zero. Only the last of those would catch it, and a rung
	// whose offered rate is also zero would slip through entirely.
	steps := []Step{
		{Offered: 1000, From: rampOrigin, To: rampOrigin.Add(10 * time.Second)},
		// Nothing was recorded in this interval at all.
		{Offered: 2000, From: rampOrigin.Add(time.Hour), To: rampOrigin.Add(time.Hour + 10*time.Second)},
	}
	rps := series.Series{Points: []series.Point{{At: rampOrigin, V: 1000}}}
	empty := series.Series{}

	k := FindKnee(steps, rps, empty, empty, empty, DefaultKnee())
	if k.Steps[1].Held {
		t.Error("a rung with no observations was called held")
	}
	if !strings.Contains(k.Steps[1].Why, "no observations") {
		t.Errorf("reason was %q", k.Steps[1].Why)
	}
}

func TestChosenIsAFractionOfTheKnee(t *testing.T) {
	k := Knee{Found: true, Rate: 32000}
	if got := k.Chosen(0.5); got != 16000 {
		t.Errorf("Chosen(0.5) = %d, want 16000", got)
	}
	if got := k.Chosen(0); got != 0 {
		t.Errorf("Chosen(0) = %d, want 0", got)
	}
	if got := (Knee{}).Chosen(0.5); got != 0 {
		t.Errorf("a knee with no rate produced %d", got)
	}
}

func TestRampStagesHoldEachRateInsteadOfSweepingThroughIt(t *testing.T) {
	// The bug this shape exists to prevent: one stage per rung makes k6 interpolate
	// continuously, so no rate is ever actually offered for any length of time and
	// every rung's measurement is an average over a moving target.
	targets, stages := RampStages(1000, 5000, 5, 15*time.Second, time.Second)

	if len(targets) != 5 {
		t.Fatalf("got %d targets, want 5: %v", len(targets), targets)
	}
	if targets[0] != 1000 || targets[4] != 5000 {
		t.Errorf("ramp runs %v, want 1000 to 5000", targets)
	}
	if len(stages) != 10 {
		t.Fatalf("got %d stages, want a transition and a dwell for each of 5 rungs", len(stages))
	}
	for i := 0; i < len(stages); i += 2 {
		if stages[i].Target != stages[i+1].Target {
			t.Errorf("rung %d transitions to %d then dwells at %d",
				i/2, stages[i].Target, stages[i+1].Target)
		}
		if stages[i].Duration != time.Second || stages[i+1].Duration != 15*time.Second {
			t.Errorf("rung %d has durations %s/%s", i/2, stages[i].Duration, stages[i+1].Duration)
		}
	}
}

func TestStepWindowsSkipTheTransition(t *testing.T) {
	// Including the transition is how a ramp reports a rung it never held: for that
	// second the offered rate is somewhere between this rung and the last.
	targets, _ := RampStages(1000, 3000, 3, 10*time.Second, time.Second)
	steps := StepWindows(rampOrigin, targets, 10*time.Second, time.Second)

	if len(steps) != 3 {
		t.Fatalf("got %d windows, want 3", len(steps))
	}
	if !steps[0].From.Equal(rampOrigin.Add(time.Second)) {
		t.Errorf("first window starts at %s, want one second in", steps[0].From.Sub(rampOrigin))
	}
	if !steps[1].From.Equal(rampOrigin.Add(12 * time.Second)) {
		t.Errorf("second window starts at %s, want 12s in", steps[1].From.Sub(rampOrigin))
	}
	// A nanosecond short of the dwell, so it cannot reach the next rung's first sample.
	for i, s := range steps {
		if s.To.Sub(s.From) != 10*time.Second-1 {
			t.Errorf("window %d spans %s", i, s.To.Sub(s.From))
		}
		if i > 0 && !s.From.After(steps[i-1].To) {
			t.Errorf("window %d starts at or before the end of window %d", i, i-1)
		}
	}
}

func TestASingleStepRampTargetsThePeak(t *testing.T) {
	targets, _ := RampStages(500, 4000, 1, time.Minute, time.Second)
	if len(targets) != 1 || targets[0] != 4000 {
		t.Errorf("got %v, want a single rung at the peak", targets)
	}
}
