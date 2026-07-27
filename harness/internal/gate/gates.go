package gate

import (
	"fmt"
	"math"
)

// Config holds the thresholds. Zero fields take conservative defaults.
type Config struct {
	RunnerCPUCeilingPct  float64
	PoolGrowthSuspect    float64
	PoolGrowthInvalid    float64
	PeerPoolRatioInvalid float64
	MaxFailedRate        float64
	MaxDroppedIterations float64
	AchievedVsOfferedMin float64
	MaxClockDriftMs      float64
	ObserveOnly          bool
}

func (c Config) withDefaults() Config {
	if c.RunnerCPUCeilingPct == 0 {
		c.RunnerCPUCeilingPct = 80
	}
	if c.PoolGrowthSuspect == 0 {
		c.PoolGrowthSuspect = 1.0
	}
	if c.PoolGrowthInvalid == 0 {
		c.PoolGrowthInvalid = 2.0
	}
	if c.PeerPoolRatioInvalid == 0 {
		c.PeerPoolRatioInvalid = 2.0
	}
	if c.AchievedVsOfferedMin == 0 {
		c.AchievedVsOfferedMin = 0.99
	}
	if c.MaxClockDriftMs == 0 {
		c.MaxClockDriftMs = 50
	}
	return c
}

// GeneratorSaturation is the gate that exists because of the 2.3x swing on a
// byte-identical binary.
//
// Five independent signals, because the failure has more than one face. Pool growth
// and runner CPU catch it directly; the peer comparison catches it even when the
// absolute thresholds were set too loosely, which on hardware nobody has profiled yet
// is the likely case.
type GeneratorSaturation struct{ Cfg Config }

func (g GeneratorSaturation) ID() string { return "loadgen.saturation" }

func (g GeneratorSaturation) Check(e Evidence) []Finding {
	cfg := g.Cfg.withDefaults()
	var out []Finding

	growth := e.Load.Growth()
	if e.Load.PreAllocatedVUs > 0 && growth > cfg.PoolGrowthSuspect {
		level := Suspect
		if growth >= cfg.PoolGrowthInvalid {
			level = Invalid
		}
		out = append(out, Finding{
			Gate:  g.ID(),
			Level: level,
			Summary: fmt.Sprintf("the generator grew its VU pool %.1fx past its allocation",
				growth),
			Detail: "extra virtual users are extra goroutines competing for the same cores as the subject, " +
				"which slows it, which grows the pool again",
			Evidence: map[string]any{
				"preAllocatedVUs": e.Load.PreAllocatedVUs,
				"observedMaxVUs":  e.Load.ObservedMaxVUs,
				"growth":          round(growth, 3),
				"suspectAt":       cfg.PoolGrowthSuspect,
				"invalidAt":       cfg.PoolGrowthInvalid,
			},
		})
	}

	if e.Load.VUCap > 0 && e.Load.ObservedMaxVUs >= e.Load.VUCap {
		out = append(out, Finding{
			Gate:    g.ID(),
			Level:   Invalid,
			Summary: "the generator reached its VU ceiling",
			Detail:  "the pool was still growing when it hit the cap, so the offered rate was never actually offered",
			Evidence: map[string]any{
				"observedMaxVUs": e.Load.ObservedMaxVUs,
				"vuCap":          e.Load.VUCap,
			},
		})
	}

	if e.Runner.Present && e.Runner.Cores > 0 {
		util := e.Runner.UtilisationPct()
		if util > cfg.RunnerCPUCeilingPct {
			out = append(out, Finding{
				Gate:  g.ID(),
				Level: Invalid,
				Summary: fmt.Sprintf("the load generator's own machine was %.0f%% busy",
					util),
				Detail: "a saturated generator measures itself; the subject's numbers describe queueing on the runner",
				Evidence: map[string]any{
					"runnerCpuPctMean": round(e.Runner.CPUPctMean, 1),
					"runnerCpuPctPeak": round(e.Runner.CPUPctPeak, 1),
					"runnerCores":      e.Runner.Cores,
					"utilisationPct":   round(util, 1),
					"ceilingPct":       cfg.RunnerCPUCeilingPct,
				},
			})
		}
	}

	// Throughput falling while the generator's own CPU climbs is the feedback loop
	// itself, visible in the correlation rather than in any single number.
	if r, ok := negativeCorrelation(e); ok {
		out = append(out, Finding{
			Gate:     g.ID(),
			Level:    Invalid,
			Summary:  "throughput fell as the generator's CPU rose",
			Detail:   "that is the generator competing with the subject, not the subject slowing down",
			Evidence: map[string]any{"correlation": round(r, 3)},
		})
	}

	// The cross-cell check, and the one that catches the incident this lab was rebuilt
	// around: 1,600 virtual users on one repetition and 7,113 on the next, same binary,
	// a day apart. That is invisible from inside a single cell — both look like
	// complete runs — so it needs siblings in scope.
	//
	// Siblings of the SAME scenario. Different workloads legitimately need different
	// generator capacity: scenario 001 offers a bare GET at sub-millisecond latency
	// while 005 holds every request for 70 ms, and their pools differ by more than a
	// factor by design. Comparing across scenarios fires on every cell of every
	// campaign, which is how a gate stops being read.
	for _, p := range e.Peers {
		if p.Scenario != e.Scenario {
			continue
		}
		if p.ObservedMaxVUs <= 0 || e.Load.ObservedMaxVUs <= 0 {
			continue
		}
		ratio := float64(e.Load.ObservedMaxVUs) / float64(p.ObservedMaxVUs)
		if ratio < 1 {
			ratio = 1 / ratio
		}
		if ratio >= cfg.PeerPoolRatioInvalid {
			out = append(out, Finding{
				Gate:  g.ID(),
				Level: Invalid,
				Summary: fmt.Sprintf("this cell's VU pool differs %.1fx from sibling %s",
					ratio, p.CellID),
				Detail: "cells in one campaign that needed wildly different generator capacity " +
					"were not measuring the same system",
				Evidence: map[string]any{
					"observedMaxVUs":     e.Load.ObservedMaxVUs,
					"peerCellId":         p.CellID,
					"peerObservedMaxVUs": p.ObservedMaxVUs,
					"ratio":              round(ratio, 2),
					"invalidAt":          cfg.PeerPoolRatioInvalid,
				},
			})
			break // one peer disagreement is enough; listing all of them is noise
		}
	}

	return out
}

// negativeCorrelation reports the Pearson correlation between achieved throughput and
// runner CPU when it is strongly negative — the signature of the feedback loop.
func negativeCorrelation(e Evidence) (float64, bool) {
	rps, cpu := e.Load.RPS, e.Runner.CPU
	if rps.Len() < 5 || cpu.Len() < 5 {
		return 0, false
	}
	// Pair by nearest timestamp within half a bucket; both are 1 Hz series over the
	// same window.
	var xs, ys []float64
	ci := 0
	for _, p := range rps.Points {
		for ci+1 < cpu.Len() &&
			cpu.Points[ci+1].At.Sub(p.At).Abs() < cpu.Points[ci].At.Sub(p.At).Abs() {
			ci++
		}
		if cpu.Points[ci].At.Sub(p.At).Abs() > 1500e6 { // 1.5s
			continue
		}
		xs = append(xs, p.V)
		ys = append(ys, cpu.Points[ci].V)
	}
	if len(xs) < 5 {
		return 0, false
	}
	r, ok := pearson(xs, ys)
	if !ok || r > -0.7 {
		return 0, false
	}
	return r, true
}

func pearson(xs, ys []float64) (float64, bool) {
	n := float64(len(xs))
	var sx, sy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
	}
	mx, my := sx/n, sy/n
	var sxy, sxx, syy float64
	for i := range xs {
		dx, dy := xs[i]-mx, ys[i]-my
		sxy += dx * dy
		sxx += dx * dx
		syy += dy * dy
	}
	if sxx == 0 || syy == 0 {
		return 0, false
	}
	return sxy / math.Sqrt(sxx*syy), true
}

// ErrorRate catches failures on both sides of the wire.
//
// A run that failed every request reports a throughput number like any other, and a
// fast 404 reads as excellent throughput. The server-side half catches the case the
// client cannot see at all: work the runtime shed after telling the client it had
// succeeded.
type ErrorRate struct{ Cfg Config }

func (g ErrorRate) ID() string { return "loadgen.errors" }

func (g ErrorRate) Check(e Evidence) []Finding {
	cfg := g.Cfg.withDefaults()
	var out []Finding

	if e.Load.FailedRate > cfg.MaxFailedRate {
		out = append(out, Finding{
			Gate:    g.ID(),
			Level:   Invalid,
			Summary: fmt.Sprintf("%.2f%% of requests failed", e.Load.FailedRate*100),
			Detail:  "fast failures look like fast successes; a throughput figure over errors is not a result",
			Evidence: map[string]any{
				"failedRate": e.Load.FailedRate,
				"maxAllowed": cfg.MaxFailedRate,
			},
		})
	}

	if e.Server.Present {
		shed := e.Server.MessagesFailed + e.Server.MessagesDropped
		if shed > 0 {
			level := Suspect
			if e.Load.FailedRate == 0 {
				// The client was told everything succeeded. That is worse than an
				// honest error rate, because nothing downstream would ever notice.
				level = Invalid
			}
			out = append(out, Finding{
				Gate:    g.ID(),
				Level:   level,
				Summary: fmt.Sprintf("the runtime shed %.0f messages the client counted as successful", shed),
				Evidence: map[string]any{
					"messagesFailed":   e.Server.MessagesFailed,
					"messagesDropped":  e.Server.MessagesDropped,
					"clientFailedRate": e.Load.FailedRate,
				},
			})
		}
		if !e.Server.ReadyThroughout {
			out = append(out, Finding{
				Gate:    g.ID(),
				Level:   Suspect,
				Summary: "the runtime reported itself not ready during the measured window",
			})
		}
	}

	return out
}

// RateGap catches saturation: the condition under which every latency percentile means
// something other than what it appears to.
//
// Every measured cell in the old results was saturated — k6 exited 99 and shed between
// 262,000 and 629,000 iterations — and the index published their percentiles anyway.
type RateGap struct{ Cfg Config }

func (g RateGap) ID() string { return "loadgen.saturated" }

func (g RateGap) Check(e Evidence) []Finding {
	cfg := g.Cfg.withDefaults()
	var out []Finding

	if e.Load.DroppedIterations > cfg.MaxDroppedIterations {
		out = append(out, Finding{
			Gate:  g.ID(),
			Level: Invalid,
			Summary: fmt.Sprintf("the generator could not start %.0f iterations",
				e.Load.DroppedIterations),
			Detail: "dropped iterations mean the offered rate was never offered, so the latency " +
				"percentiles include time requests spent queued at the generator",
			Evidence: map[string]any{
				"droppedIterations": e.Load.DroppedIterations,
				"maxAllowed":        cfg.MaxDroppedIterations,
			},
		})
	}

	if ratio := e.Load.AchievedRatio(); e.Load.OfferedRate > 0 && ratio < cfg.AchievedVsOfferedMin {
		out = append(out, Finding{
			Gate:  g.ID(),
			Level: Invalid,
			Summary: fmt.Sprintf("achieved %.0f req/s against %.0f offered (%.1f%%)",
				e.Load.AchievedRPS, e.Load.OfferedRate, ratio*100),
			Detail: "a gap between offered and achieved is saturation regardless of what the percentiles say",
			Evidence: map[string]any{
				"offeredRate": e.Load.OfferedRate,
				"achievedRps": round(e.Load.AchievedRPS, 1),
				"ratio":       round(ratio, 4),
				"minRatio":    cfg.AchievedVsOfferedMin,
			},
		})
	}

	// Client latency vastly exceeding the runtime's own mean is queueing at the
	// generator, not work in the runtime. This is the comparison that would have
	// caught a 933 ms p95 published against a 0.41 ms server-side mean.
	if e.Server.Present && e.Server.HasFlowMean && e.Server.FlowMeanSeconds > 0 &&
		e.Load.DroppedIterations > 0 {
		out = append(out, Finding{
			Gate:  g.ID(),
			Level: Suspect,
			Summary: fmt.Sprintf("client-observed latency includes generator queueing (runtime mean was %.2f ms)",
				e.Server.FlowMeanSeconds*1000),
			Detail: "this run shed iterations, so client latency counts time spent waiting to be issued",
			Evidence: map[string]any{
				"flowMeanMs":        round(e.Server.FlowMeanSeconds*1000, 3),
				"droppedIterations": e.Load.DroppedIterations,
			},
		})
	}

	return out
}

// SteadyState fires when no defensible measurement window could be found.
type SteadyState struct{}

func (g SteadyState) ID() string { return "window.steady-state" }

func (g SteadyState) Check(e Evidence) []Finding {
	if e.WindowOK {
		if !e.Window.Corroborated {
			return []Finding{{
				Gate:    g.ID(),
				Level:   Suspect,
				Summary: "the measured window was accepted on throughput alone",
				Detail: "no corroborating series was available, so a throughput plateau masking a " +
					"still-warming subject would not have been caught",
				Evidence: map[string]any{"cv": round(e.Window.CV, 4)},
			}}
		}
		return nil
	}
	return []Finding{{
		Gate:    g.ID(),
		Level:   Invalid,
		Summary: "no steady window was found in the load pass",
		Detail:  "the run never settled, so there is no interval whose numbers describe a stable system",
	}}
}

// Identity checks that the process holding the port is the one the harness meant to
// start.
//
// A stale process on the port does not fail loudly — it answers 404 quickly, which
// reads as excellent throughput.
type Identity struct{}

func (g Identity) ID() string { return "subject.identity" }

func (g Identity) Check(e Evidence) []Finding {
	var out []Finding

	if e.Server.Present && e.Server.IntendedVersion != "" && e.Server.IdentityVersion != "" &&
		e.Server.IdentityVersion != e.Server.IntendedVersion {
		out = append(out, Finding{
			Gate:  g.ID(),
			Level: Invalid,
			Summary: fmt.Sprintf("the running process reports %s but the harness started %s",
				e.Server.IdentityVersion, e.Server.IntendedVersion),
			Evidence: map[string]any{
				"reported": e.Server.IdentityVersion,
				"intended": e.Server.IntendedVersion,
			},
		})
	}

	// Detection that is wrong in the optimistic direction is the dangerous direction.
	if e.Caps.AdminPortDetected && !e.Caps.AdminPortAnswered {
		out = append(out, Finding{
			Gate:    g.ID(),
			Level:   Invalid,
			Summary: "an admin port was detected but never answered",
			Detail:  "capability detection greps the artifact's own help text; this cell proves it was wrong",
		})
	}

	return out
}

// Fingerprint refuses to compare cells measured on materially different machines, or
// whose readiness was detected by different means.
type Fingerprint struct{}

func (g Fingerprint) ID() string { return "env.fingerprint" }

func (g Fingerprint) Check(e Evidence) []Finding {
	var out []Finding
	for _, p := range e.Peers {
		if e.FingerprintHash != "" && p.FingerprintHash != "" &&
			e.FingerprintHash != p.FingerprintHash {
			out = append(out, Finding{
				Gate:    g.ID(),
				Level:   Invalid,
				Summary: fmt.Sprintf("host fingerprint differs from sibling %s", p.CellID),
				Evidence: map[string]any{
					"fingerprint":     e.FingerprintHash,
					"peerCellId":      p.CellID,
					"peerFingerprint": p.FingerprintHash,
				},
			})
			break
		}
	}
	for _, p := range e.Peers {
		if e.Ready.Method != "" && p.ReadyMethod != "" && e.Ready.Method != p.ReadyMethod {
			out = append(out, Finding{
				Gate:  g.ID(),
				Level: Suspect,
				Summary: fmt.Sprintf("readiness measured by %q here and %q in sibling %s",
					e.Ready.Method, p.ReadyMethod, p.CellID),
				Detail: "a build with no admin port reaches ready at a later moment than one that " +
					"answers /readyz, so the cold-start figures are not the same measurement",
			})
			break
		}
	}
	return out
}

// ClockDrift fires when the runner and subject clocks moved apart enough to reorder
// events between the two timelines.
type ClockDrift struct{ Cfg Config }

func (g ClockDrift) ID() string { return "env.clock" }

func (g ClockDrift) Check(e Evidence) []Finding {
	cfg := g.Cfg.withDefaults()
	if !e.Clock.Measured {
		return []Finding{{
			Gate:    g.ID(),
			Level:   Suspect,
			Summary: "the clock offset between runner and subject was not measured",
			Detail:  "cross-machine correlation is unverifiable without it",
		}}
	}
	if math.Abs(e.Clock.DriftMs) > cfg.MaxClockDriftMs {
		return []Finding{{
			Gate:  g.ID(),
			Level: Invalid,
			Summary: fmt.Sprintf("the clock offset moved %.0f ms during the cell",
				e.Clock.DriftMs),
			Detail: "at 1 Hz sampling this is enough to move a sample by a whole bucket and invert " +
				"the apparent order of events",
			Evidence: map[string]any{
				"driftMs":  round(e.Clock.DriftMs, 1),
				"offsetMs": round(e.Clock.OffsetMs, 1),
				"maxDrift": cfg.MaxClockDriftMs,
			},
		}}
	}
	return nil
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
