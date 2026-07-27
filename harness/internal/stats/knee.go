package stats

import (
	"fmt"
	"math"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/series"
)

// Step is one rung of a capacity ramp: an offered rate, held over an interval.
type Step struct {
	Offered float64
	From    time.Time
	To      time.Time
}

// StepResult is what a rung produced.
type StepResult struct {
	Offered   float64 `json:"offered"`
	Achieved  float64 `json:"achieved"`
	Ratio     float64 `json:"ratio"`
	Dropped   float64 `json:"dropped"`
	Failed    float64 `json:"failed"`
	LatencyMs float64 `json:"latencyMs"`
	Seconds   int     `json:"seconds"`

	// Held is whether the system kept up at this rung.
	Held bool `json:"held"`
	// Why names the first condition that failed, so a ramp is readable rather than
	// merely plotted. "dropped 412 iterations" is a diagnosis; a bend in a line is not.
	Why string `json:"why,omitempty"`
}

// KneeConfig is what "kept up" means.
//
// Deliberately three separate conditions rather than one composite score. A ramp can
// fold over in three different ways and they mean different things: shedding iterations
// is the generator failing to place load, an achieved-rate shortfall without drops is
// the server refusing it, and a latency blow-up with neither is queueing. A single
// number would report all three as the same event.
type KneeConfig struct {
	// AchievedVsOfferedMin is how close achieved must stay to offered.
	AchievedVsOfferedMin float64
	// LatencyFactor bounds a rung's mean latency as a multiple of the first rung's.
	// The first rung is the reference because it is the one place on the ramp where
	// the system is known not to be queueing.
	LatencyFactor float64
	// MaxDropped is the dropped-iteration count a rung may still be called held. Zero
	// is the honest default: an iteration the generator never placed is load the
	// server never saw, so the rung did not measure what it claims to.
	MaxDropped float64
	// MaxFailedRate bounds the share of requests that returned an error. Fast failures
	// look exactly like fast successes in a throughput number.
	MaxFailedRate float64
}

// DefaultKnee is the starting calibration. Like the gates, it ships loose and tightens
// from data — nobody has measured the right latency factor yet, because the old lab
// read its ramps by eye.
func DefaultKnee() KneeConfig {
	return KneeConfig{
		AchievedVsOfferedMin: 0.99,
		LatencyFactor:        3.0,
		MaxDropped:           0,
		MaxFailedRate:        0,
	}
}

func (c KneeConfig) withDefaults() KneeConfig {
	d := DefaultKnee()
	if c.AchievedVsOfferedMin == 0 {
		c.AchievedVsOfferedMin = d.AchievedVsOfferedMin
	}
	if c.LatencyFactor == 0 {
		c.LatencyFactor = d.LatencyFactor
	}
	return c
}

// Knee is where a capacity ramp stopped keeping up.
type Knee struct {
	// Found is whether a rung failed at all. A ramp that held every rung has not
	// located a knee — it has established a lower bound, and saying so is the
	// difference between a measurement and an extrapolation.
	Found bool `json:"found"`
	// Rate is the highest offered rate the system held.
	Rate float64 `json:"rate"`
	// FailedAt is the first rung it did not hold. Zero when Found is false.
	FailedAt float64      `json:"failedAt,omitempty"`
	Steps    []StepResult `json:"steps"`
	Note     string       `json:"note"`
}

// Chosen is the rate to run the campaign at: a stated fraction of the knee.
//
// The fraction exists because a benchmark held at its own capacity limit measures the
// limit, not the workload. Every arm of a scenario then runs at this one rate, so an arm
// that cannot hold it produces a saturation finding rather than a quietly lower number.
func (k Knee) Chosen(fraction float64) int {
	if k.Rate <= 0 || fraction <= 0 {
		return 0
	}
	return int(math.Floor(k.Rate * fraction))
}

// FindKnee reads a capacity ramp.
//
// It is pure and takes the series rather than the run, because the interesting failures
// here are interpretive — a rung called held that shed four hundred iterations, a knee
// read off the ramp segment rather than the dwell — and those are only cheap to test if
// no process has to be started to produce them.
func FindKnee(steps []Step, rps, dropped, failed, latency series.Series, cfg KneeConfig) Knee {
	cfg = cfg.withDefaults()

	k := Knee{Steps: make([]StepResult, 0, len(steps))}
	if len(steps) == 0 {
		k.Note = "the ramp declared no steps"
		return k
	}

	reference := 0.0
	for i, s := range steps {
		r := StepResult{Offered: s.Offered}

		win := rps.Window(s.From, s.To)
		r.Seconds = len(win.Points)
		if mean, ok := win.Mean(); ok {
			r.Achieved = mean
		}
		if sum, ok := dropped.Window(s.From, s.To).Sum(); ok {
			r.Dropped = sum
		}
		if sum, ok := failed.Window(s.From, s.To).Sum(); ok {
			r.Failed = sum
		}
		if mean, ok := latency.Window(s.From, s.To).Mean(); ok {
			r.LatencyMs = mean
		}
		if s.Offered > 0 {
			r.Ratio = r.Achieved / s.Offered
		}
		// The first rung with a latency reading is the reference. Not step zero
		// unconditionally: a ramp whose first rung produced no observations would
		// otherwise fix the reference at zero and call every later rung a blow-up.
		if reference == 0 && r.LatencyMs > 0 {
			reference = r.LatencyMs
		}

		switch {
		case r.Seconds == 0:
			r.Why = "produced no observations"
		case r.Dropped > cfg.MaxDropped:
			r.Why = fmt.Sprintf("the generator shed %.0f iterations", r.Dropped)
		case r.Failed > 0 && r.Achieved > 0 &&
			r.Failed/(r.Achieved*float64(r.Seconds)) > cfg.MaxFailedRate:
			r.Why = fmt.Sprintf("%.0f requests failed", r.Failed)
		case r.Ratio < cfg.AchievedVsOfferedMin:
			r.Why = fmt.Sprintf("achieved %.0f of %.0f offered (%.1f%%)",
				r.Achieved, r.Offered, r.Ratio*100)
		case reference > 0 && r.LatencyMs > reference*cfg.LatencyFactor:
			r.Why = fmt.Sprintf("mean latency %.2fms against %.2fms at %.0f req/s (%.1fx)",
				r.LatencyMs, reference, steps[0].Offered, r.LatencyMs/reference)
		default:
			r.Held = true
		}

		k.Steps = append(k.Steps, r)

		// Stop at the first rung that did not hold. A ramp that recovers at a higher
		// rate has not un-broken: the recovery is the generator having shed enough
		// load to get back under the ceiling, and treating a later rung as the knee
		// would report the collapse as capacity.
		if !r.Held && !k.Found {
			k.Found = true
			k.FailedAt = s.Offered
			if i == 0 {
				k.Note = fmt.Sprintf(
					"the ramp did not hold its first rung of %.0f req/s: %s", s.Offered, r.Why)
			} else {
				k.Rate = steps[i-1].Offered
				k.Note = fmt.Sprintf("held %.0f req/s; %.0f req/s %s",
					k.Rate, s.Offered, r.Why)
			}
			break
		}
		if r.Held {
			k.Rate = s.Offered
		}
	}

	if !k.Found {
		// An honest lower bound, not a knee. A campaign may still run against it, and
		// the report has to say which of the two it got.
		k.Note = fmt.Sprintf(
			"no rung folded over: the ramp held every step up to %.0f req/s, which is a "+
				"lower bound on capacity rather than a measurement of it", k.Rate)
	}
	return k
}

// RampStages turns a capacity specification into the stage list k6 executes.
//
// Two stages per rung, and that is the whole trick. A ramping-arrival-rate stage
// interpolates linearly from the current rate to its target over its duration, so a
// naive one-stage-per-rung ramp never actually holds any rate — it is a continuous
// sweep, and every measurement taken from it is an average over a moving target. Pairing
// a short transition with a dwell produces a staircase, and only the dwell is measured.
func RampStages(startRate, peakRate, steps int, dwell, transition time.Duration) (targets []int, stages []RampStage) {
	if steps < 1 {
		steps = 1
	}
	if transition <= 0 {
		transition = time.Second
	}
	for i := 0; i < steps; i++ {
		target := startRate
		if steps > 1 {
			target = startRate + (peakRate-startRate)*i/(steps-1)
		} else {
			target = peakRate
		}
		targets = append(targets, target)
		stages = append(stages,
			RampStage{Target: target, Duration: transition},
			RampStage{Target: target, Duration: dwell},
		)
	}
	return targets, stages
}

// RampStage is one k6 stage.
type RampStage struct {
	Target   int
	Duration time.Duration
}

// StepWindows returns the dwell interval of each rung, given when the ramp started.
//
// The transition is excluded deliberately: it is the segment where the offered rate is
// changing, and including it is how a ramp reports a rung it never held.
//
// Each window also stops an instant short of the next one. [series.Series.Window] is a
// closed interval — which is right for a measured window, whose endpoints are chosen
// from the data — but it means two abutting windows would share a sample. On a ramp that
// sample is the first observation of the *next*, higher rung, so a rung would inherit
// the drops and the latency of the rung that broke, and the knee would be reported one
// step low.
func StepWindows(start time.Time, targets []int, dwell, transition time.Duration) []Step {
	out := make([]Step, 0, len(targets))
	at := start
	for _, t := range targets {
		at = at.Add(transition)
		out = append(out, Step{Offered: float64(t), From: at, To: at.Add(dwell - 1)})
		at = at.Add(dwell)
	}
	return out
}
