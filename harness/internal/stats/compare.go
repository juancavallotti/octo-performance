package stats

import (
	"fmt"
	"math"
	"math/rand/v2"
)

// Decision is what a comparison concluded.
//
// Deliberately direction-neutral: higher throughput is good and higher latency is not,
// and this package does not know which metric it is holding.
type Decision string

const (
	// Noise: the arms were not shown to differ.
	Noise Decision = "noise"
	// Higher: B exceeded A beyond the noise band.
	Higher Decision = "higher"
	// Lower: B fell below A beyond the noise band.
	Lower Decision = "lower"
	// Insufficient: there were not enough valid repetitions to say anything.
	Insufficient Decision = "insufficient"
)

// Significant reports whether the comparison found a real difference.
func (d Decision) Significant() bool { return d == Higher || d == Lower }

// Comparison is the result of comparing two arms on one metric.
//
// Reason is populated whenever Decision is not significant, and says which rule
// declined. A report that shows a delta without saying why it is or is not being
// believed is the thing this type exists to prevent.
type Comparison struct {
	A, B         Summary
	DeltaPct     float64
	NoiseBandPct float64
	Decision     Decision
	Reason       string
}

// CompareConfig tunes the comparison. The zero value is not useful; use
// [DefaultCompareConfig].
type CompareConfig struct {
	// NoiseFloorPct is the finest resolution that will ever be claimed, regardless
	// of how tight the measurements look.
	NoiseFloorPct float64
	// MinN is the fewest repetitions per arm that can support a conclusion.
	MinN int
	// Level is the confidence level for the bootstrap interval, e.g. 0.95.
	Level float64
	// BootstrapIters is the resampling count.
	BootstrapIters int
	// Rand is injected so a report renders identically on every run.
	Rand *rand.Rand
}

// DefaultCompareConfig is the configuration campaigns use.
//
// The 2% floor is inherited from lab/bin/compare-report.py, where it was chosen so a
// thermally variable host could not be made to yield a 1% conclusion. On dedicated
// hardware it should come down — but only once a run of A/A campaigns has shown what
// the real floor is, which is a measurement nobody has taken yet.
func DefaultCompareConfig(rnd *rand.Rand) CompareConfig {
	return CompareConfig{
		NoiseFloorPct:  2.0,
		MinN:           3,
		Level:          0.95,
		BootstrapIters: 2000,
		Rand:           rnd,
	}
}

// Compare tests whether arm B differs from arm A.
//
// Three rules can decline, in order, and the first that fires is the Reason:
//
//  1. Either arm has fewer than MinN repetitions.
//  2. The delta is inside the noise band — the larger of the configured floor and
//     the rep-to-rep spread observed within either arm. A difference smaller than
//     the spread of the measurements it is drawn from is not evidence.
//  3. The bootstrap confidence intervals of the two medians overlap.
//
// Rules 2 and 3 are both applied because they fail differently: a tight-but-tiny
// delta passes 3 and fails 2, and a large delta drawn from wildly scattered reps
// passes 2 and fails 3.
func Compare(a, b []float64, cfg CompareConfig) Comparison {
	sa, okA := Describe(a)
	sb, okB := Describe(b)

	c := Comparison{A: sa, B: sb}
	if !okA || !okB {
		c.Decision = Insufficient
		c.Reason = "one or both arms produced no valid repetitions"
		return c
	}

	if cfg.Rand != nil {
		if ci, ok := BootstrapMedianCI(a, cfg.Level, cfg.BootstrapIters, cfg.Rand); ok {
			c.A.CI, c.A.HasCI = ci, true
		}
		if ci, ok := BootstrapMedianCI(b, cfg.Level, cfg.BootstrapIters, cfg.Rand); ok {
			c.B.CI, c.B.HasCI = ci, true
		}
	}

	c.NoiseBandPct = math.Max(cfg.NoiseFloorPct, math.Max(sa.SpreadPct, sb.SpreadPct))
	if sa.Median != 0 {
		c.DeltaPct = (sb.Median - sa.Median) / math.Abs(sa.Median) * 100
	}

	if sa.N < cfg.MinN || sb.N < cfg.MinN {
		c.Decision = Insufficient
		c.Reason = fmt.Sprintf("needs %d valid repetitions per arm, have %d and %d",
			cfg.MinN, sa.N, sb.N)
		return c
	}

	if math.Abs(c.DeltaPct) < c.NoiseBandPct {
		c.Decision = Noise
		c.Reason = fmt.Sprintf("%+.1f%% is inside the %.1f%% noise band (rep-to-rep spread was %.1f%% and %.1f%%)",
			c.DeltaPct, c.NoiseBandPct, sa.SpreadPct, sb.SpreadPct)
		return c
	}

	if c.A.HasCI && c.B.HasCI && c.A.CI.Overlaps(c.B.CI) {
		c.Decision = Noise
		c.Reason = fmt.Sprintf("%.0f%% confidence intervals overlap: [%.1f, %.1f] against [%.1f, %.1f]",
			cfg.Level*100, c.A.CI.Lo, c.A.CI.Hi, c.B.CI.Lo, c.B.CI.Hi)
		return c
	}

	if c.DeltaPct > 0 {
		c.Decision = Higher
	} else {
		c.Decision = Lower
	}
	return c
}
