package report

import (
	"strings"
	"testing"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/gate"
	"github.com/juancavallotti/octo-performance/harness/internal/result"
	"github.com/juancavallotti/octo-performance/harness/internal/stats"
)

func cell(scenario, arm string, rep, ordinal int, rps, p95 float64, opts ...func(*result.Cell)) *result.Cell {
	c := &result.Cell{
		Schema: result.Schema, Scenario: scenario, Arm: arm, Rep: rep, Ordinal: ordinal,
		StartedAt: time.Unix(1785200000+int64(ordinal)*100, 0),
		EndedAt:   time.Unix(1785200060+int64(ordinal)*100, 0),
		Load:      result.LoadSpec{Model: "open", Rate: 500, RateSource: "campaign"},
		Headline: result.Headline{
			OfferedRate: 500, AchievedRPS: rps, AchievedRatio: rps / 500,
			ClientP50Ms: p95 / 3, ClientP95Ms: p95, ClientP99Ms: p95 * 2,
			ServerMeanMs: p95 / 20, HasServerMean: true,
			SubjectCPUmsPerReq: 0.25, SubjectRSSPeakMB: 45, ColdStartMs: 40,
			PreAllocatedVUs: 50, ObservedMaxVUs: 50,
		},
		Verdict: gate.Verdict{Level: gate.Valid},
	}
	c.Binary.Version, c.Binary.SHA256 = arm, "c44e8fe43d93aaaabbbbccccdddd"
	c.Binary.Observability, c.Binary.Metrics = true, true
	for _, o := range opts {
		o(c)
	}
	return c
}

func campaign(t *testing.T, opts ...func(*result.RollupInput)) *result.Campaign {
	t.Helper()

	var cells []*result.Cell
	jitter := []float64{0, 3, -3, 5, -5}
	for i := 0; i < 5; i++ {
		cells = append(cells, cell("001-template-page", "0.5.0", i+1, i*2, 500+jitter[i], 1.0+jitter[i]/100))
		cells = append(cells, cell("001-template-page", "0.6.0", i+1, i*2+1, 420+jitter[i], 1.4+jitter[i]/100))
	}
	// One collapsed cell, so the ledger has something to strike through.
	cells = append(cells, cell("001-template-page", "0.6.0", 6, 20, 150, 900, func(c *result.Cell) {
		c.Verdict = gate.Verdict{Level: gate.Invalid, Findings: []gate.Finding{{
			Gate: "loadgen.saturated", Level: gate.Invalid,
			Summary: "achieved 150 req/s against 500 offered (30.0%)",
		}}}
	}))

	in := result.RollupInput{
		Name: "regression-050-vs-060", Question: "Did 0.6.0 regress against 0.5.0?",
		PlanHash: "94d193e3aaaa", Baseline: "0.5.0", Arms: []string{"0.5.0", "0.6.0"},
		Reps: 5, Order: "interleaved", Planned: 10, Colocated: true,
		Routes: map[string]string{"001-template-page": "/page/octo"},
		Seed:   7,
	}
	for _, o := range opts {
		o(&in)
	}
	return result.Rollup(in, cells)
}

func render(t *testing.T, c *result.Campaign) string {
	t.Helper()
	b, err := Render(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReportIsSelfContained(t *testing.T) {
	// One file, no network. A report that fetches anything is a report that stops
	// rendering, and these are meant to be readable years later from a copied
	// directory. The failure is invisible on the machine that made it, which is
	// exactly why it is asserted rather than reviewed.
	html := render(t, campaign(t))

	if bad := SelfContained([]byte(html)); len(bad) > 0 {
		t.Errorf("the report reaches outside itself: %s", strings.Join(bad, ", "))
	}
	if !strings.Contains(html, "<style>") {
		t.Error("the stylesheet is not inlined")
	}
	if !strings.Contains(html, "<!doctype html>") {
		t.Error("no doctype")
	}
}

func TestReportLeadsWithASentence(t *testing.T) {
	c := campaign(t)
	html := render(t, c)

	// The verdict must appear before any table. A reader who stops at the top should
	// still have the answer.
	//
	// Matched on the leading clause rather than the whole sentence: html/template
	// escapes "+40.0%" to "&#43;40.0%", which is correct and would make an
	// exact-match assertion test the escaper rather than the layout.
	lead, _, _ := strings.Cut(c.Verdict, ".")
	verdictAt := strings.Index(html, lead)
	tableAt := strings.Index(html, "<table")
	if verdictAt < 0 {
		t.Fatalf("the verdict is not in the report: %q", c.Verdict)
	}
	if tableAt >= 0 && tableAt < verdictAt {
		t.Error("a table appears before the verdict")
	}
	if !strings.Contains(html, "&#43;40.0%") {
		t.Error("the escaped delta is missing; the verdict was truncated or mangled")
	}
}

func TestReportShowsClientAndServerLatencySideBySide(t *testing.T) {
	// The comparison that would have caught a published p95 of 933 ms against a
	// runtime that said 0.41 ms. Both numbers existed in the same directory; only
	// one was shown.
	html := render(t, campaign(t))

	if !strings.Contains(html, "What the client saw, against what the runtime said") {
		t.Error("the client-against-server section is missing")
	}
	if !strings.Contains(html, "runtime mean flow") {
		t.Error("the runtime's own view of its latency is not shown")
	}
	if !strings.Contains(html, "is not runtime work") {
		t.Error("the gap between the two is not explained")
	}
}

func TestReportNeverPrintsAMedianWithoutItsSpread(t *testing.T) {
	// Sixty-one rows of unqualified point estimates is what the old index published.
	// Every median in this report is followed by its dispersion and its n.
	html := render(t, campaign(t))

	if !strings.Contains(html, "IQR") {
		t.Error("no dispersion shown alongside the medians")
	}
	if strings.Count(html, "over 5 reps") < 4 {
		t.Error("repetition counts are not shown next to the summaries")
	}
}

func TestReportShowsEveryRepetitionAsAPoint(t *testing.T) {
	// With n=5 the honest chart is the five points: a reader can see whether a delta
	// comes from a tight cluster or from two runs that disagree.
	html := render(t, campaign(t))

	if !strings.Contains(html, "<svg") {
		t.Fatal("no chart drawn")
	}
	// Two arms x five reps on throughput, and the same again on p95.
	if n := strings.Count(html, "class=\"dot\""); n < 20 {
		t.Errorf("%d dots drawn; every repetition must appear", n)
	}
	if !strings.Contains(html, "class=\"median\"") {
		t.Error("the median is not marked")
	}
}

func TestExcludedCellsAreVisibleAndStruckThrough(t *testing.T) {
	// Never silently dropped. It is on disk, it is in the ledger, and it contributes
	// to nothing.
	html := render(t, campaign(t))

	if !strings.Contains(html, `class="excluded"`) {
		t.Error("the excluded cell is not marked in the ledger")
	}
	if !strings.Contains(html, "achieved 150 req/s against 500 offered") {
		t.Error("the reason the cell was excluded is not shown")
	}
	if !strings.Contains(html, "Validity ledger") {
		t.Error("no validity ledger")
	}
}

func TestADeltaInsideTheNoiseBandIsNeverStyledAsAResult(t *testing.T) {
	// Two arms that agree. Every delta must be labelled noise and carry the band it
	// was measured against.
	var cells []*result.Cell
	jitter := []float64{0, 4, -4, 6, -6}
	for i := 0; i < 5; i++ {
		cells = append(cells, cell("001-page", "0.5.0", i+1, i*2, 500+jitter[i], 1.0))
		cells = append(cells, cell("001-page", "0.6.0", i+1, i*2+1, 501+jitter[i], 1.0))
	}
	c := result.Rollup(result.RollupInput{
		Name: "aa", Baseline: "0.5.0", Arms: []string{"0.5.0", "0.6.0"}, Reps: 5,
		Order: "interleaved", Planned: 10, Routes: map[string]string{"001-page": "/page"}, Seed: 3,
	}, cells)

	html := render(t, c)
	if strings.Contains(html, "tag bad") {
		t.Error("a delta inside the noise band was styled as a regression")
	}
	if !strings.Contains(html, "tag noise") {
		t.Error("no delta was labelled noise")
	}
	// The band itself is shown, so a reader can see what the delta was measured
	// against rather than being asked to trust the label.
	if !strings.Contains(html, "band") {
		t.Error("the noise band is not shown")
	}
}

func TestReportCarriesTheWarningsThatQualifyItsNumbers(t *testing.T) {
	html := render(t, campaign(t))

	if !strings.Contains(html, "shared a host with the subject") {
		t.Error("colocation is not stated")
	}
	if !strings.Contains(html, "feedback loop") {
		t.Error("the warning does not say why colocation matters")
	}
}

func TestReportCarriesProvenance(t *testing.T) {
	html := render(t, campaign(t))

	for _, want := range []string{
		"94d193e3aaaa", // plan hash
		"c44e8fe43d93", // artifact digest, shortened
		"interleaved",  // execution order
		"Provenance",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("provenance is missing %q", want)
		}
	}
}

func TestReportShowsGateCalibration(t *testing.T) {
	// Thresholds ship loose because nobody has measured the right ones. This table
	// is how they stop being guesses.
	html := render(t, campaign(t))

	if !strings.Contains(html, "Gate calibration") {
		t.Error("no calibration section")
	}
	if !strings.Contains(html, "loadgen.saturated") {
		t.Error("the gate that fired is not listed")
	}
}

func TestReportOfACampaignThatConcludedNothing(t *testing.T) {
	// Rendering must not fall over on the shape it will most often meet during
	// development: a couple of cells and nothing decidable.
	c := result.Rollup(result.RollupInput{
		Name: "thin", Baseline: "a", Arms: []string{"a", "b"}, Reps: 1, Planned: 2,
		Order: "interleaved", Routes: map[string]string{"001-page": "/page"},
	}, []*result.Cell{
		cell("001-page", "a", 1, 0, 500, 1.0),
		cell("001-page", "b", 1, 1, 500, 1.0),
	})

	html := render(t, c)
	if !strings.Contains(html, "Nothing was established") {
		t.Errorf("a campaign that decided nothing does not say so:\n%s", c.Verdict)
	}
	if bad := SelfContained([]byte(html)); len(bad) > 0 {
		t.Errorf("not self-contained: %v", bad)
	}
}

func TestReportOfAnEmptyCampaign(t *testing.T) {
	c := result.Rollup(result.RollupInput{Name: "empty"}, nil)
	html := render(t, c)
	if !strings.Contains(html, "nothing to conclude") {
		t.Error("an empty campaign does not say so")
	}
}

func TestNumberFormattingDoesNotOverstatePrecision(t *testing.T) {
	// The old index published throughput as "15999.221107831117".
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{15999.221107831117, "15999"},
		{933.0142, "933.0"},
		{1.2345, "1.23"},
		{0.0482, "0.048"},
		{0.00041, "0.0004"},
		{0, "0"},
	} {
		if got := num(tc.in); got != tc.want {
			t.Errorf("num(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	c := campaign(t)
	if render(t, c) != render(t, c) {
		t.Error("two renders of the same campaign differ")
	}
}

func TestTheReportSaysHowEachRateWasChosen(t *testing.T) {
	// The single most consequential unexplained number in the old lab. Every arm of a
	// scenario is offered the same rate, so if that rate was above the runtime's knee
	// the whole scenario measured saturation — and no published result said where the
	// number came from or when it was last true.
	c := campaign(t, func(in *result.RollupInput) {
		in.Rates = []result.RateChoice{{
			Scenario: "001-template-page", Rate: 16000, Source: "measured",
			Arm: "0.5.0", Fraction: 0.5, KneeFound: true, KneeRate: 32000,
			Note: "16000 req/s, 50% of a measured knee at 32000.",
			Steps: []stats.StepResult{
				{Offered: 28000, Achieved: 27998, Ratio: 0.9999, LatencyMs: 0.9, Held: true},
				{Offered: 32000, Achieved: 31990, Ratio: 0.9997, LatencyMs: 1.1, Held: true},
				{Offered: 36000, Achieved: 24010, Ratio: 0.667, Dropped: 9161, LatencyMs: 8.4,
					Why: "the generator shed 9161 iterations"},
			},
		}}
	})
	html := render(t, c)

	if !strings.Contains(html, "How each rate was chosen") {
		t.Fatal("the report does not say where the offered rate came from")
	}
	if !strings.Contains(html, "measured") {
		t.Error("a measured rate is not distinguished from a declared one")
	}
	// The rung that broke, and why. A bend in a curve is not a diagnosis.
	if !strings.Contains(html, "shed 9161 iterations") {
		t.Error("the ramp's fold-over reason is not shown")
	}
	if !strings.Contains(html, "36000") {
		t.Error("the rung that failed is not in the table")
	}
	if bad := SelfContained([]byte(html)); len(bad) > 0 {
		t.Errorf("not self-contained: %v", bad)
	}
}

func TestARateNobodyMeasuredIsLabelledDeclared(t *testing.T) {
	c := campaign(t, func(in *result.RollupInput) {
		in.Rates = []result.RateChoice{{
			Scenario: "005-http-proxy", Rate: 800, Source: "scenario",
			Note: "Fixed by design: the gap between the two arms' ceilings is the result.",
		}}
	})
	html := render(t, c)

	if !strings.Contains(html, "declared") {
		t.Error("a declared rate is not labelled as one")
	}
	if !strings.Contains(html, "the gap between the two arms") {
		t.Error("the reason a rate was declared rather than measured is not carried")
	}
}
