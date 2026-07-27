package result

import (
	"strings"
	"testing"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/gate"
)

// cell builds a cell with the headline numbers a roll-up reads.
func cell(scenario, arm string, rep, ordinal int, rps, p95 float64, opts ...func(*Cell)) *Cell {
	c := &Cell{
		Schema: Schema, Scenario: scenario, Arm: arm, Rep: rep, Ordinal: ordinal,
		StartedAt: time.Unix(1785200000+int64(ordinal)*100, 0),
		EndedAt:   time.Unix(1785200060+int64(ordinal)*100, 0),
		Load:      LoadSpec{Model: "open", Rate: 500, RateSource: "campaign"},
		Headline: Headline{
			OfferedRate: 500, AchievedRPS: rps, AchievedRatio: rps / 500,
			ClientP50Ms: p95 / 3, ClientP95Ms: p95, ClientP99Ms: p95 * 2,
			ServerMeanMs: p95 / 20, HasServerMean: true,
			SubjectCPUmsPerReq: 0.25, SubjectRSSPeakMB: 45, ColdStartMs: 40,
			PreAllocatedVUs: 50, ObservedMaxVUs: 50,
		},
		Verdict: gate.Verdict{Level: gate.Valid},
	}
	c.Binary.Version = arm
	c.Binary.Observability, c.Binary.Metrics = true, true
	for _, o := range opts {
		o(c)
	}
	return c
}

func invalid(reason string) func(*Cell) {
	return func(c *Cell) {
		c.Verdict = gate.Verdict{
			Level:    gate.Invalid,
			Findings: []gate.Finding{{Gate: "loadgen.saturated", Level: gate.Invalid, Summary: reason}},
		}
	}
}

func noServerMetrics(c *Cell) {
	c.Headline.HasServerMean, c.Headline.ServerMeanMs = false, 0
	c.Binary.Observability, c.Binary.Metrics = false, false
}

func input() RollupInput {
	return RollupInput{
		Name: "regression", Question: "did it regress?",
		Baseline: "0.5.0", Arms: []string{"0.5.0", "0.6.0"},
		Reps: 5, Order: "interleaved", Planned: 10,
		Routes: map[string]string{"001-page": "/page"},
		Seed:   7,
	}
}

// fiveReps builds a full arm at a given operating point.
func fiveReps(scenario, arm string, base, p95 float64, startOrdinal int, opts ...func(*Cell)) []*Cell {
	var out []*Cell
	jitter := []float64{0, 1, -1, 2, -2}
	for i := 0; i < 5; i++ {
		out = append(out, cell(scenario, arm, i+1, startOrdinal+i*2,
			base+jitter[i], p95+jitter[i]/100, opts...))
	}
	return out
}

func TestRollupSeparatesNothingEstablishedFromNoRegression(t *testing.T) {
	// This is the most consequential false negative available to this package. A
	// campaign with too few valid repetitions produces exactly zero regressions —
	// not because the arms agree, but because every comparison declined — and
	// calling that a clean run would be worse than publishing a wrong number, because
	// nobody would think to check it.
	var cells []*Cell
	cells = append(cells, cell("001-page", "0.5.0", 1, 0, 500, 1.0), cell("001-page", "0.5.0", 2, 3, 501, 1.1))
	cells = append(cells, cell("001-page", "0.6.0", 1, 1, 500, 1.2), cell("001-page", "0.6.0", 2, 2, 499, 1.3))

	c := Rollup(input(), cells)

	if len(c.Regressions) != 0 {
		t.Fatalf("regressions = %v on data too thin to decide anything", c.Regressions)
	}
	if c.Evidence.Conclusive != 0 {
		t.Fatalf("Conclusive = %d, want none: two reps cannot support a conclusion", c.Evidence.Conclusive)
	}
	if strings.Contains(strings.ToLower(c.Verdict), "no regression") {
		t.Errorf("verdict claims no regression from evidence that decided nothing:\n  %s", c.Verdict)
	}
	if !strings.Contains(c.Verdict, "Nothing was established") {
		t.Errorf("verdict = %q, want it to say nothing was established", c.Verdict)
	}
	// And it must say so in the reader's terms, not only in a field.
	if !strings.Contains(c.Verdict, "absence of a finding") {
		t.Errorf("verdict = %q", c.Verdict)
	}
}

func TestRollupReportsNoRegressionWhenTheEvidenceSupportsIt(t *testing.T) {
	var cells []*Cell
	cells = append(cells, fiveReps("001-page", "0.5.0", 500, 1.0, 0)...)
	cells = append(cells, fiveReps("001-page", "0.6.0", 501, 1.01, 1)...)

	c := Rollup(input(), cells)

	if c.Evidence.Conclusive == 0 {
		t.Fatalf("nothing was decided from five reps per arm: %+v", c.Evidence)
	}
	if len(c.Regressions) != 0 {
		t.Errorf("regressions = %v on two arms that agree", c.Regressions)
	}
	if !strings.HasPrefix(c.Verdict, "No regression on 1 scenario:") {
		t.Errorf("verdict = %q", c.Verdict)
	}
	if strings.Contains(c.Verdict, "1 scenarios") {
		t.Errorf("verdict = %q; a count of one is not plural", c.Verdict)
	}
}

func TestRollupNamesARealRegressionAndItsDirection(t *testing.T) {
	// A 40% throughput drop, well outside any plausible noise band.
	var cells []*Cell
	cells = append(cells, fiveReps("001-page", "0.5.0", 500, 1.0, 0)...)
	cells = append(cells, fiveReps("001-page", "0.6.0", 300, 1.0, 1)...)

	c := Rollup(input(), cells)

	if len(c.Regressions) == 0 {
		t.Fatalf("a 40%% throughput drop was not reported: %s", c.Verdict)
	}
	if !strings.HasPrefix(c.Verdict, "Regressions on 1 of 1 scenario") {
		t.Errorf("verdict = %q", c.Verdict)
	}

	var found bool
	for _, s := range c.Scenarios {
		for _, cmp := range s.Comparisons {
			if cmp.Metric != "throughput" {
				continue
			}
			found = true
			// Throughput falling is a regression; latency falling would be an
			// improvement. The metric decides, not the sign.
			if cmp.Label != "regression" {
				t.Errorf("throughput %+.1f%% labelled %q", cmp.DeltaPct, cmp.Label)
			}
			if cmp.DeltaPct > -30 {
				t.Errorf("DeltaPct = %v, want about -40", cmp.DeltaPct)
			}
		}
	}
	if !found {
		t.Error("no throughput comparison was made")
	}
}

func TestLatencyFallingIsAnImprovementAndThroughputFallingIsNot(t *testing.T) {
	var cells []*Cell
	cells = append(cells, fiveReps("001-page", "0.5.0", 500, 10.0, 0)...)
	cells = append(cells, fiveReps("001-page", "0.6.0", 500, 5.0, 1)...)

	c := Rollup(input(), cells)
	if len(c.Improvements) == 0 {
		t.Fatalf("halving latency was not reported as an improvement: %s", c.Verdict)
	}
	for _, s := range c.Scenarios {
		for _, cmp := range s.Comparisons {
			if cmp.Metric == "client p95" && cmp.Label != "improvement" {
				t.Errorf("client p95 %+.1f%% labelled %q, want improvement", cmp.DeltaPct, cmp.Label)
			}
		}
	}
}

func TestExcludedCellsReachTheLedgerAndNothingElse(t *testing.T) {
	// The structural guarantee: there is no path from an excluded cell into a
	// median. It stays on disk, it stays in the ledger, and it contributes nothing.
	var cells []*Cell
	cells = append(cells, fiveReps("001-page", "0.5.0", 500, 1.0, 0)...)
	cells = append(cells, fiveReps("001-page", "0.6.0", 500, 1.0, 1)...)
	// Three collapsed cells that would drag the median down by a third if counted.
	cells = append(cells, cell("001-page", "0.6.0", 6, 20, 150, 900, invalid("achieved 150 of 500 offered")))
	cells = append(cells, cell("001-page", "0.6.0", 7, 21, 140, 950, invalid("achieved 140 of 500 offered")))
	cells = append(cells, cell("001-page", "0.6.0", 8, 22, 160, 880, invalid("achieved 160 of 500 offered")))

	c := Rollup(input(), cells)

	if c.Counts.Excluded != 3 {
		t.Fatalf("Excluded = %d, want 3", c.Counts.Excluded)
	}
	if len(c.Ledger) != len(cells) {
		t.Errorf("ledger has %d rows for %d cells; an excluded cell must still be visible",
			len(c.Ledger), len(cells))
	}

	for _, s := range c.Scenarios {
		for _, a := range s.Arms {
			if a.Arm != "0.6.0" {
				continue
			}
			if a.Excluded != 3 || a.Used != 5 {
				t.Errorf("arm %s: used %d excluded %d, want 5 and 3", a.Arm, a.Used, a.Excluded)
			}
			// 150 rps cells would move a median of 500 if they had been counted.
			if a.Throughput.Summary.Median < 495 {
				t.Errorf("median throughput = %v; an excluded cell reached an aggregate",
					a.Throughput.Summary.Median)
			}
			if a.Throughput.Summary.Min < 495 {
				t.Errorf("min throughput = %v; an excluded cell reached an aggregate",
					a.Throughput.Summary.Min)
			}
		}
	}
	if !strings.Contains(c.Verdict, "3 of 13 cells were excluded") {
		t.Errorf("verdict does not account for the excluded cells: %q", c.Verdict)
	}
}

func TestAMetricOnlyOneArmMeasuredIsNotCompared(t *testing.T) {
	// Comparing 0.4.3 against 0.5.0 means one arm serves metrics and one does not.
	// Treating the absent side as zero would manufacture a hundred-percent delta out
	// of an absence.
	var cells []*Cell
	cells = append(cells, fiveReps("001-page", "0.4.3", 500, 1.0, 0, noServerMetrics)...)
	cells = append(cells, fiveReps("001-page", "0.5.0", 500, 1.0, 1)...)

	in := input()
	in.Baseline, in.Arms = "0.4.3", []string{"0.4.3", "0.5.0"}
	c := Rollup(in, cells)

	for _, s := range c.Scenarios {
		for _, cmp := range s.Comparisons {
			if cmp.Metric == "runtime mean flow" {
				t.Errorf("a server-side comparison was made against an arm that serves no metrics: %+v", cmp)
			}
		}
		for _, a := range s.Arms {
			if a.Arm != "0.4.3" {
				continue
			}
			if a.ServerMean.Present {
				t.Error("a server mean was reported for a build with no admin port")
			}
			if a.ServerMean.Absent == "" {
				t.Error("the absence must be explained, not left as a blank that reads as zero")
			}
		}
	}
	// And the reader is told, rather than left to notice a blank column.
	var warned bool
	for _, w := range c.Warnings {
		if strings.Contains(w, "0.4.3") && strings.Contains(w, "no metrics") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning about the arm that serves no metrics: %v", c.Warnings)
	}
}

func TestColocationIsStatedBeforeAnyNumber(t *testing.T) {
	in := input()
	in.Colocated = true
	c := Rollup(in, fiveReps("001-page", "0.5.0", 500, 1.0, 0))

	if len(c.Warnings) == 0 || !strings.Contains(c.Warnings[0], "shared a host") {
		t.Fatalf("colocation is the fact that qualifies everything else; warnings = %v", c.Warnings)
	}
	if !strings.Contains(c.Warnings[0], "feedback loop") {
		t.Error("the warning must say why it matters, not merely that it happened")
	}
}

func TestRollupIsDeterministic(t *testing.T) {
	// The bootstrap is seeded, so a report re-rendered from the same cells is the
	// same report. A confidence interval that moves when you look at it twice is not
	// a confidence interval.
	var cells []*Cell
	cells = append(cells, fiveReps("001-page", "0.5.0", 500, 1.0, 0)...)
	cells = append(cells, fiveReps("001-page", "0.6.0", 480, 1.2, 1)...)

	a, b := Rollup(input(), cells), Rollup(input(), cells)
	if a.Verdict != b.Verdict {
		t.Errorf("verdict differs between runs:\n  %s\n  %s", a.Verdict, b.Verdict)
	}
	for i := range a.Scenarios {
		for j := range a.Scenarios[i].Comparisons {
			x, y := a.Scenarios[i].Comparisons[j], b.Scenarios[i].Comparisons[j]
			if x != y {
				t.Errorf("comparison %d differs between runs:\n  %+v\n  %+v", j, x, y)
			}
		}
	}
}

func TestRollupOfNothing(t *testing.T) {
	c := Rollup(input(), nil)
	if !strings.Contains(c.Verdict, "nothing to conclude") {
		t.Errorf("verdict = %q", c.Verdict)
	}
	if len(c.Scenarios) != 0 {
		t.Error("scenarios were invented from no cells")
	}
}

func TestEveryCellExcludedConcludesNothing(t *testing.T) {
	cells := fiveReps("001-page", "0.6.0", 150, 900, 0, invalid("saturated"))
	c := Rollup(input(), cells)
	if !strings.Contains(c.Verdict, "concludes nothing") {
		t.Errorf("verdict = %q", c.Verdict)
	}
}

func TestGateCalibrationCountsWhatFired(t *testing.T) {
	// The gates ship loose on purpose. This table is how a threshold stops being a
	// guess, so it has to count cells rather than findings.
	var cells []*Cell
	cells = append(cells, fiveReps("001-page", "0.5.0", 500, 1.0, 0)...)
	cells = append(cells, cell("001-page", "0.6.0", 1, 11, 150, 900, invalid("saturated")))

	c := Rollup(input(), cells)
	if len(c.Calib) != 1 {
		t.Fatalf("calibration = %+v, want one gate", c.Calib)
	}
	g := c.Calib[0]
	if g.Gate != "loadgen.saturated" || g.Fired != 1 || g.Cells != 6 {
		t.Errorf("calibration = %+v", g)
	}
	if g.Example == "" {
		t.Error("a gate that fired must show what it saw")
	}
}

func TestOrderEffectIsPublishedRatherThanAsserted(t *testing.T) {
	// A steady downward drift with execution position: exactly the artifact
	// interleaving exists to expose. The report states the slope and r² instead of
	// claiming the ordering made it go away.
	var cells []*Cell
	for i := 0; i < 5; i++ {
		cells = append(cells, cell("001-page", "0.5.0", i+1, i*2, 500-float64(i)*20, 1.0))
	}
	c := Rollup(input(), cells)

	for _, s := range c.Scenarios {
		for _, a := range s.Arms {
			if !a.HasOrder {
				t.Fatal("no order effect computed from five cells")
			}
			if a.OrderR2 < 0.9 {
				t.Errorf("r² = %v on a perfectly linear drift", a.OrderR2)
			}
			if a.OrderSlopePct >= 0 {
				t.Errorf("slope = %v on a downward drift", a.OrderSlopePct)
			}
		}
	}
}
