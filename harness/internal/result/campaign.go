package result

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/gate"
	"github.com/juancavallotti/octo-performance/harness/internal/stats"
)

// Campaign is the whole comparison, rolled up.
//
// Everything the report shows is computed here, once. The report formats and derives
// nothing — that is the rule that let the old report.py grow to a thousand lines, become
// a library by accident, and end up as the only place several numbers were calculated.
type Campaign struct {
	Schema int `json:"schema"`

	Name     string `json:"name"`
	Question string `json:"question"`
	PlanHash string `json:"planHash"`

	StartedAt time.Time     `json:"startedAt"`
	EndedAt   time.Time     `json:"endedAt"`
	Elapsed   time.Duration `json:"elapsed"`

	Harness Harness `json:"harness"`

	// Baseline is the arm every other arm is compared against: the first one the
	// campaign declared.
	Baseline    string   `json:"baseline"`
	Arms        []Arm    `json:"arms"`
	Reps        int      `json:"reps"`
	Order       string   `json:"order"`
	OrderReason string   `json:"orderReason,omitempty"`
	ObserveOnly bool     `json:"observeOnly"`
	Colocated   bool     `json:"colocated"`
	Notes       string   `json:"notes,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`

	Counts    Counts            `json:"counts"`
	Evidence  Evidence          `json:"evidence"`
	Scenarios []ScenarioRollup  `json:"scenarios"`
	Ledger    []LedgerRow       `json:"ledger"`
	Calib     []GateCalibration `json:"gateCalibration,omitempty"`

	// Verdict is the sentence that leads the report.
	Verdict string `json:"verdict"`
	// Regressions and Improvements are the findings that survived the noise band.
	Regressions  []Finding `json:"regressions,omitempty"`
	Improvements []Finding `json:"improvements,omitempty"`
}

// Arm is one thing compared, as executed.
type Arm struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	Mode    string `json:"mode,omitempty"`
	// AdminPort and Metrics say what the artifact could do. An arm that could not
	// serve metrics is not comparable to one that did on any server-side number, and
	// the report says so rather than leaving a blank cell.
	AdminPort bool `json:"adminPort"`
	Metrics   bool `json:"metrics"`
}

// Counts is the campaign's completeness, which leads the validity ledger.
type Counts struct {
	Planned  int `json:"planned"`
	Ran      int `json:"ran"`
	Valid    int `json:"valid"`
	Suspect  int `json:"suspect"`
	Invalid  int `json:"invalid"`
	Excluded int `json:"excluded"`
}

// Evidence is how much the campaign was actually able to conclude.
//
// It exists because "no regression was found" and "nothing could be established" are
// different statements, and only the first is a result. A campaign with too few valid
// repetitions produces no regressions at all — not because the arms agree, but because
// every comparison declined — and reporting that as a clean run would be the most
// consequential false negative this harness could produce.
type Evidence struct {
	Comparisons  int `json:"comparisons"`
	Conclusive   int `json:"conclusive"`
	Insufficient int `json:"insufficient"`
}

// ScenarioRollup is one workload across every arm.
type ScenarioRollup struct {
	Scenario string `json:"scenario"`
	Route    string `json:"route"`
	Model    string `json:"model"`
	Rate     int    `json:"rate"`
	// RateSource says where the rate came from, so a number nobody can account for
	// is visible as one.
	RateSource string `json:"rateSource,omitempty"`

	Arms        []ArmRollup  `json:"arms"`
	Comparisons []Comparison `json:"comparisons"`
}

// ArmRollup is one arm's repetitions on one scenario, described.
type ArmRollup struct {
	Arm      string `json:"arm"`
	Cells    int    `json:"cells"`
	Used     int    `json:"used"`
	Excluded int    `json:"excluded"`

	Throughput Metric `json:"throughput"`
	ClientP50  Metric `json:"clientP50"`
	ClientP95  Metric `json:"clientP95"`
	ClientP99  Metric `json:"clientP99"`
	ServerMean Metric `json:"serverMean"`
	CPUPerReq  Metric `json:"cpuPerRequest"`
	RSSPeak    Metric `json:"rssPeak"`
	ColdStart  Metric `json:"coldStart"`

	// OrderSlopePct is how much the throughput moved per position in the execution
	// sequence, and R2 how much of the variance that explains. It turns "we
	// interleaved, so trust us" into a number.
	OrderSlopePct float64 `json:"orderSlopePct"`
	OrderR2       float64 `json:"orderR2"`
	HasOrder      bool    `json:"hasOrder"`
}

// Metric is one measured quantity across an arm's repetitions.
//
// Present is separate from the values because an arm with no admin port has no server
// mean, and a blank is not a zero.
type Metric struct {
	Label   string        `json:"label"`
	Unit    string        `json:"unit"`
	Present bool          `json:"present"`
	Absent  string        `json:"absent,omitempty"`
	Summary stats.Summary `json:"summary"`
	// LowerIsBetter decides how a delta is labelled. Throughput improves upward,
	// latency and cost improve downward, and getting it backwards would report a
	// regression as a win.
	LowerIsBetter bool `json:"lowerIsBetter"`
}

// Comparison is one arm against the baseline on one metric.
type Comparison struct {
	Metric   string `json:"metric"`
	Unit     string `json:"unit"`
	Baseline string `json:"baseline"`
	Arm      string `json:"arm"`

	BaselineMedian float64 `json:"baselineMedian"`
	ArmMedian      float64 `json:"armMedian"`
	DeltaPct       float64 `json:"deltaPct"`
	NoiseBandPct   float64 `json:"noiseBandPct"`

	// Decision is the statistical call, and Label is what it means for this metric:
	// the same downward move is an improvement in latency and a regression in
	// throughput.
	Decision string `json:"decision"`
	Label    string `json:"label"`
	Reason   string `json:"reason,omitempty"`

	BaselineN int `json:"baselineN"`
	ArmN      int `json:"armN"`
}

// Finding is a comparison that survived the noise band, for the verdict banner.
type Finding struct {
	Scenario string  `json:"scenario"`
	Arm      string  `json:"arm"`
	Metric   string  `json:"metric"`
	DeltaPct float64 `json:"deltaPct"`
	Detail   string  `json:"detail"`
}

// LedgerRow is one cell's entry in the validity ledger. Every cell appears, including
// the excluded ones — struck through, never silently dropped.
type LedgerRow struct {
	Cell     string   `json:"cell"`
	Scenario string   `json:"scenario"`
	Arm      string   `json:"arm"`
	Rep      int      `json:"rep"`
	Ordinal  int      `json:"ordinal"`
	Level    string   `json:"level"`
	Excluded bool     `json:"excluded"`
	Reasons  []string `json:"reasons,omitempty"`

	AchievedRPS float64 `json:"achievedRps"`
	OfferedRate float64 `json:"offeredRate"`
	ClientP95Ms float64 `json:"clientP95Ms"`
	ObservedVUs int     `json:"observedVus"`
	AllocVUs    int     `json:"allocVus"`
	Dropped     float64 `json:"dropped"`
}

// GateCalibration is the observed distribution of one gate's evidence across the whole
// campaign.
//
// The gates ship deliberately loose, because nobody knows the right runner-CPU ceiling
// or pool-growth ratio — the old lab never measured either. This is how a threshold
// stops being a guess: run a campaign in observe mode, read the distribution, tighten.
type GateCalibration struct {
	Gate    string   `json:"gate"`
	Fired   int      `json:"fired"`
	Cells   int      `json:"cells"`
	Levels  []string `json:"levels,omitempty"`
	Example string   `json:"example,omitempty"`
}

// RollupInput is everything the roll-up needs that the cells do not carry.
type RollupInput struct {
	Name        string
	Question    string
	PlanHash    string
	Baseline    string
	Arms        []string
	Reps        int
	Order       string
	OrderReason string
	ObserveOnly bool
	Colocated   bool
	Notes       string
	Planned     int
	Routes      map[string]string
	// Seed makes the bootstrap deterministic, so re-rendering a report from the same
	// cells produces the same intervals. A confidence interval that moves when you
	// look at it twice is not one.
	Seed uint64
}

// Rollup computes the campaign model from the cells that ran.
//
// It is pure: no I/O, no clock, no randomness beyond the seeded bootstrap. Re-running
// it over the same cells produces the same bytes.
func Rollup(in RollupInput, cells []*Cell) *Campaign {
	seed := in.Seed
	if seed == 0 {
		seed = 0x0c70be9c
	}
	cfg := stats.DefaultCompareConfig(rand.New(rand.NewPCG(seed, seed^0x9e3779b9)))

	c := &Campaign{
		Schema:      Schema,
		Name:        in.Name,
		Question:    in.Question,
		PlanHash:    in.PlanHash,
		Baseline:    in.Baseline,
		Reps:        in.Reps,
		Order:       in.Order,
		OrderReason: in.OrderReason,
		ObserveOnly: in.ObserveOnly,
		Colocated:   in.Colocated,
		Notes:       in.Notes,
	}
	c.Counts.Planned = in.Planned
	c.Counts.Ran = len(cells)

	if len(cells) == 0 {
		c.Verdict = "No cells completed, so there is nothing to conclude."
		return c
	}

	c.Harness = cells[0].Harness
	c.StartedAt, c.EndedAt = spanOf(cells)
	c.Elapsed = c.EndedAt.Sub(c.StartedAt)
	c.Arms = armsOf(in.Arms, cells)

	for _, cell := range cells {
		switch cell.Verdict.Level {
		case gate.Valid:
			c.Counts.Valid++
		case gate.Suspect:
			c.Counts.Suspect++
		case gate.Invalid:
			c.Counts.Invalid++
		}
		if cell.Verdict.Excluded() {
			c.Counts.Excluded++
		}
		c.Ledger = append(c.Ledger, ledgerRow(cell))
	}
	sort.SliceStable(c.Ledger, func(i, j int) bool { return c.Ledger[i].Ordinal < c.Ledger[j].Ordinal })

	c.Calib = calibration(cells)
	c.Warnings = warningsFor(c, cells)

	for _, id := range scenarioOrder(cells) {
		c.Scenarios = append(c.Scenarios, scenarioRollup(id, in, cells, cfg))
	}

	c.Regressions, c.Improvements = findings(c.Scenarios)
	c.Evidence = evidenceOf(c.Scenarios)
	c.Verdict = verdictSentence(c)
	return c
}

// usable reports whether a cell may contribute to an aggregate.
//
// This is the structural guarantee: there is no other path into a median. A cell the
// gates excluded is on disk and in the ledger and nowhere else.
func usable(cell *Cell) bool { return !cell.Verdict.Excluded() }

func scenarioRollup(id string, in RollupInput, cells []*Cell, cfg stats.CompareConfig) ScenarioRollup {
	s := ScenarioRollup{Scenario: id, Route: in.Routes[id]}

	var mine []*Cell
	for _, cell := range cells {
		if cell.Scenario == id {
			mine = append(mine, cell)
		}
	}
	if len(mine) > 0 {
		s.Model = mine[0].Load.Model
		s.Rate = mine[0].Load.Rate
		s.RateSource = mine[0].Load.RateSource
	}

	byArm := map[string][]*Cell{}
	for _, cell := range mine {
		byArm[cell.Arm] = append(byArm[cell.Arm], cell)
	}

	for _, arm := range armOrder(in.Arms, mine) {
		s.Arms = append(s.Arms, armRollup(arm, byArm[arm]))
	}

	base := in.Baseline
	if base == "" && len(s.Arms) > 0 {
		base = s.Arms[0].Arm
	}
	for _, a := range s.Arms {
		if a.Arm == base {
			continue
		}
		s.Comparisons = append(s.Comparisons, compareArms(base, byArm[base], a, byArm[a.Arm], cfg)...)
	}
	return s
}

func armRollup(arm string, cells []*Cell) ArmRollup {
	a := ArmRollup{Arm: arm, Cells: len(cells)}

	var used []*Cell
	for _, cell := range cells {
		if usable(cell) {
			used = append(used, cell)
		} else {
			a.Excluded++
		}
	}
	a.Used = len(used)

	a.Throughput = metricOf("throughput", "req/s", false, used,
		func(c *Cell) (float64, bool) { return c.Headline.AchievedRPS, true })
	a.ClientP50 = metricOf("client p50", "ms", true, used,
		func(c *Cell) (float64, bool) { return c.Headline.ClientP50Ms, true })
	a.ClientP95 = metricOf("client p95", "ms", true, used,
		func(c *Cell) (float64, bool) { return c.Headline.ClientP95Ms, true })
	a.ClientP99 = metricOf("client p99", "ms", true, used,
		func(c *Cell) (float64, bool) { return c.Headline.ClientP99Ms, true })
	a.ServerMean = metricOf("runtime mean flow", "ms", true, used,
		func(c *Cell) (float64, bool) { return c.Headline.ServerMeanMs, c.Headline.HasServerMean })
	a.CPUPerReq = metricOf("cost", "cpu-ms/req", true, used,
		func(c *Cell) (float64, bool) {
			return c.Headline.SubjectCPUmsPerReq, c.Headline.SubjectCPUmsPerReq > 0
		})
	a.RSSPeak = metricOf("peak RSS", "MB", true, used,
		func(c *Cell) (float64, bool) { return c.Headline.SubjectRSSPeakMB, c.Headline.SubjectRSSPeakMB > 0 })
	a.ColdStart = metricOf("cold start", "ms", true, used,
		func(c *Cell) (float64, bool) { return c.Headline.ColdStartMs, c.Headline.ColdStartMs > 0 })

	if a.ServerMean.Absent == "" && !a.ServerMean.Present {
		a.ServerMean.Absent = "this build serves no metrics"
	}

	ords := make([]int, 0, len(used))
	vals := make([]float64, 0, len(used))
	for _, c := range used {
		ords = append(ords, c.Ordinal)
		vals = append(vals, c.Headline.AchievedRPS)
	}
	if slope, r2, ok := stats.OrderEffect(ords, vals); ok {
		a.OrderSlopePct, a.OrderR2, a.HasOrder = slope, r2, true
	}
	return a
}

func metricOf(label, unit string, lowerIsBetter bool, cells []*Cell, pick func(*Cell) (float64, bool)) Metric {
	m := Metric{Label: label, Unit: unit, LowerIsBetter: lowerIsBetter}
	var vals []float64
	for _, c := range cells {
		if v, ok := pick(c); ok {
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		m.Absent = "not measured"
		return m
	}
	s, ok := stats.Describe(vals)
	if !ok {
		return m
	}
	m.Summary, m.Present = s, true
	return m
}

// compareArms runs every metric both arms measured.
func compareArms(baseName string, base []*Cell, arm ArmRollup, armCells []*Cell, cfg stats.CompareConfig) []Comparison {
	baseRollup := armRollup(baseName, base)

	pairs := []struct{ b, a Metric }{
		{baseRollup.Throughput, arm.Throughput},
		{baseRollup.ClientP95, arm.ClientP95},
		{baseRollup.ClientP50, arm.ClientP50},
		{baseRollup.ServerMean, arm.ServerMean},
		{baseRollup.CPUPerReq, arm.CPUPerReq},
		{baseRollup.RSSPeak, arm.RSSPeak},
	}

	var out []Comparison
	for _, p := range pairs {
		// A metric only one arm could measure is not comparable, and inventing a
		// zero for the other side would produce a hundred-percent delta out of an
		// absence.
		if !p.b.Present || !p.a.Present {
			continue
		}
		cmp := stats.Compare(p.b.Summary.Values, p.a.Summary.Values, cfg)
		c := Comparison{
			Metric:         p.a.Label,
			Unit:           p.a.Unit,
			Baseline:       baseName,
			Arm:            arm.Arm,
			BaselineMedian: p.b.Summary.Median,
			ArmMedian:      p.a.Summary.Median,
			DeltaPct:       cmp.DeltaPct,
			NoiseBandPct:   cmp.NoiseBandPct,
			Decision:       string(cmp.Decision),
			Reason:         cmp.Reason,
			BaselineN:      p.b.Summary.N,
			ArmN:           p.a.Summary.N,
		}
		c.Label = labelFor(cmp.Decision, p.a.LowerIsBetter)
		out = append(out, c)
	}
	return out
}

// labelFor turns a direction into a judgement.
//
// The same downward move is an improvement in latency and a regression in throughput,
// so the metric decides, not the sign.
func labelFor(d stats.Decision, lowerIsBetter bool) string {
	switch d {
	case stats.Noise:
		return "noise"
	case stats.Insufficient:
		return "insufficient"
	case stats.Higher:
		if lowerIsBetter {
			return "regression"
		}
		return "improvement"
	case stats.Lower:
		if lowerIsBetter {
			return "improvement"
		}
		return "regression"
	}
	return string(d)
}

func findings(scenarios []ScenarioRollup) (regressions, improvements []Finding) {
	for _, s := range scenarios {
		for _, c := range s.Comparisons {
			f := Finding{
				Scenario: s.Scenario, Arm: c.Arm, Metric: c.Metric, DeltaPct: c.DeltaPct,
				Detail: fmt.Sprintf("%s %+.1f%% against %s (noise band %.1f%%)",
					c.Metric, c.DeltaPct, c.Baseline, c.NoiseBandPct),
			}
			switch c.Label {
			case "regression":
				regressions = append(regressions, f)
			case "improvement":
				improvements = append(improvements, f)
			}
		}
	}
	return regressions, improvements
}

// evidenceOf counts how many comparisons reached a decision.
func evidenceOf(scenarios []ScenarioRollup) Evidence {
	var e Evidence
	for _, s := range scenarios {
		for _, c := range s.Comparisons {
			e.Comparisons++
			if c.Decision == string(stats.Insufficient) {
				e.Insufficient++
			} else {
				e.Conclusive++
			}
		}
	}
	return e
}

// verdictSentence is the line that leads the report. A sentence, not a table.
//
// The order of the cases is the point. "Nothing could be established" is checked before
// "nothing regressed", because a campaign that could not conclude produces exactly zero
// regressions, and reporting that as a clean result is the worst thing this file could
// do. The old index published sixty-one rows of unqualified point estimates; the
// equivalent mistake here would be one confident sentence.
func verdictSentence(c *Campaign) string {
	if c.Counts.Ran == 0 {
		return "No cells completed, so there is nothing to conclude."
	}

	usableCells := c.Counts.Ran - c.Counts.Excluded
	if usableCells == 0 {
		return fmt.Sprintf(
			"Every one of the %d cells that ran was excluded by a gate, so this campaign concludes nothing.",
			c.Counts.Ran)
	}

	var b strings.Builder
	total := len(c.Scenarios)

	switch {
	case c.Evidence.Comparisons == 0:
		fmt.Fprintf(&b, "No comparison could be made: %s.", whyNoComparison(c))

	case c.Evidence.Conclusive == 0:
		fmt.Fprintf(&b,
			"Nothing was established. All %d comparisons across %s declined for want of evidence, "+
				"which is the absence of a finding rather than a finding that the arms agree.",
			c.Evidence.Comparisons, plural(total, "scenario", "scenarios"))

	case len(c.Regressions) == 0:
		fmt.Fprintf(&b, "No regression on %s: every delta that could be decided is inside its noise band.",
			plural(total, "scenario", "scenarios"))

	default:
		touched := map[string]bool{}
		for _, f := range c.Regressions {
			touched[f.Scenario] = true
		}
		clean := total - len(touched)
		fmt.Fprintf(&b, "Regressions on %d of %s", len(touched), plural(total, "scenario", "scenarios"))
		if clean > 0 {
			fmt.Fprintf(&b, "; the other %d are inside their noise bands", clean)
		}
		b.WriteString(". ")
		for i, f := range c.Regressions {
			if i == 3 {
				fmt.Fprintf(&b, "…and %d more.", len(c.Regressions)-3)
				break
			}
			fmt.Fprintf(&b, "%s: %s %+.1f%% on %s. ", f.Scenario, f.Metric, f.DeltaPct, f.Arm)
		}
	}

	// The qualifications belong in the same sentence, not in a footnote nobody reads.
	if c.Evidence.Conclusive > 0 && c.Evidence.Insufficient > 0 {
		fmt.Fprintf(&b, " %d of %d comparisons had too few valid repetitions to decide.",
			c.Evidence.Insufficient, c.Evidence.Comparisons)
	}
	if c.Counts.Excluded > 0 {
		fmt.Fprintf(&b, " %d of %d cells were excluded by a gate and contribute to nothing above.",
			c.Counts.Excluded, c.Counts.Ran)
	}
	return strings.TrimSpace(b.String())
}

// whyNoComparison explains an empty comparison set, which would otherwise be
// indistinguishable from a clean run.
func whyNoComparison(c *Campaign) string {
	if len(c.Arms) < 2 {
		return fmt.Sprintf("only %s produced usable cells", plural(len(c.Arms), "arm", "arms"))
	}
	return "no metric was measured by more than one arm"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// warningsFor states the things that qualify every number in the report.
func warningsFor(c *Campaign, cells []*Cell) []string {
	var w []string

	if c.Colocated {
		w = append(w, "The load generator shared a host with the subject. That is a feedback "+
			"loop rather than a constant tax — more virtual users, less CPU for the subject, "+
			"higher latency, more virtual users — and it is what made every result in the "+
			"previous lab bimodal. Treat these numbers as a check on the harness, not on the runtime.")
	}
	if c.ObserveOnly {
		w = append(w, "Gates ran in observe mode: every finding was recorded but none escalated "+
			"past suspect, so nothing here was excluded on a threshold that has not yet been calibrated.")
	}
	if c.Order == "blocked" {
		w = append(w, "Cells ran in blocked order, which confounds every delta with time in "+
			"session and chassis temperature. Stated reason: "+c.OrderReason)
	}
	if c.Harness.Dev {
		w = append(w, "Produced by a development build of the harness. Usable for deciding "+
			"whether a change worked; not publishable.")
	}

	// An arm that could not serve metrics is not comparable to one that did on any
	// server-side number, and a blank column is easy to read as a zero.
	var without []string
	for _, a := range c.Arms {
		if !a.Metrics {
			without = append(without, a.Name)
		}
	}
	if len(without) > 0 && len(without) < len(c.Arms) {
		w = append(w, "Arms "+strings.Join(without, ", ")+" serve no metrics, so no server-side "+
			"number exists for them and no server-side comparison was made against them.")
	}

	nonSteady := 0
	for _, cell := range cells {
		if !cell.WindowOK {
			nonSteady++
		}
	}
	if nonSteady > 0 {
		w = append(w, fmt.Sprintf(
			"%d of %d cells had no interval flat enough to measure, so their numbers describe "+
				"the whole load pass rather than a steady window.", nonSteady, len(cells)))
	}
	return w
}

func calibration(cells []*Cell) []GateCalibration {
	byGate := map[string]*GateCalibration{}
	for _, cell := range cells {
		seen := map[string]bool{}
		for _, f := range cell.Verdict.Findings {
			g := byGate[f.Gate]
			if g == nil {
				g = &GateCalibration{Gate: f.Gate, Example: f.Summary}
				byGate[f.Gate] = g
			}
			if !seen[f.Gate] {
				g.Fired++
				seen[f.Gate] = true
			}
			level := f.Level.String()
			if !contains(g.Levels, level) {
				g.Levels = append(g.Levels, level)
			}
		}
	}
	out := make([]GateCalibration, 0, len(byGate))
	for _, g := range byGate {
		g.Cells = len(cells)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Fired != out[j].Fired {
			return out[i].Fired > out[j].Fired
		}
		return out[i].Gate < out[j].Gate
	})
	return out
}

func ledgerRow(c *Cell) LedgerRow {
	return LedgerRow{
		Cell:        c.Slug(),
		Scenario:    c.Scenario,
		Arm:         c.Arm,
		Rep:         c.Rep,
		Ordinal:     c.Ordinal,
		Level:       c.Verdict.Level.String(),
		Excluded:    c.Verdict.Excluded(),
		Reasons:     c.Verdict.Reasons(),
		AchievedRPS: c.Headline.AchievedRPS,
		OfferedRate: c.Headline.OfferedRate,
		ClientP95Ms: c.Headline.ClientP95Ms,
		ObservedVUs: c.Headline.ObservedMaxVUs,
		AllocVUs:    c.Headline.PreAllocatedVUs,
		Dropped:     c.Headline.Dropped,
	}
}

func armsOf(declared []string, cells []*Cell) []Arm {
	seen := map[string]*Arm{}
	for _, c := range cells {
		a := seen[c.Arm]
		if a == nil {
			a = &Arm{Name: c.Arm}
			seen[c.Arm] = a
		}
		if a.Version == "" {
			a.Version = c.Binary.Version
			a.SHA256 = c.Binary.SHA256
			a.AdminPort = c.Binary.Observability
			a.Metrics = c.Binary.Metrics
			a.Mode = string(c.Config.Mode)
		}
	}
	var out []Arm
	for _, name := range armOrderNames(declared, seen) {
		out = append(out, *seen[name])
	}
	return out
}

func armOrderNames(declared []string, seen map[string]*Arm) []string {
	var out []string
	for _, d := range declared {
		if _, ok := seen[d]; ok {
			out = append(out, d)
		}
	}
	for name := range seen {
		if !contains(out, name) {
			out = append(out, name)
		}
	}
	if len(declared) == 0 {
		sort.Strings(out)
	}
	return out
}

// armOrder keeps the campaign's declared arm order, which is the order a reader
// expects and the order the baseline comes first in.
func armOrder(declared []string, cells []*Cell) []string {
	seen := map[string]bool{}
	for _, c := range cells {
		seen[c.Arm] = true
	}
	var out []string
	for _, d := range declared {
		if seen[d] {
			out = append(out, d)
			delete(seen, d)
		}
	}
	rest := make([]string, 0, len(seen))
	for name := range seen {
		rest = append(rest, name)
	}
	sort.Strings(rest)
	return append(out, rest...)
}

func scenarioOrder(cells []*Cell) []string {
	var out []string
	for _, c := range cells {
		if !contains(out, c.Scenario) {
			out = append(out, c.Scenario)
		}
	}
	sort.Strings(out)
	return out
}

func spanOf(cells []*Cell) (from, to time.Time) {
	for i, c := range cells {
		if i == 0 || c.StartedAt.Before(from) {
			from = c.StartedAt
		}
		if i == 0 || c.EndedAt.After(to) {
			to = c.EndedAt
		}
	}
	return from, to
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// Round2 is a formatting helper the report uses; kept here so the report derives
// nothing at all, not even a rounding rule.
func Round2(f float64) float64 { return math.Round(f*100) / 100 }
