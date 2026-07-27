package plan

import (
	"strings"
	"testing"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/spec"
)

func campaign(arms []string, reps int) *spec.Campaign {
	c := &spec.Campaign{
		Name:      "regression",
		Scenarios: []string{"001-template-page"},
		Reps:      reps,
		Order:     spec.Interleaved,
		Load:      spec.Load{Model: spec.Open, Rate: 16000, Duration: time.Minute},
	}
	for _, a := range arms {
		c.Arms = append(c.Arms, spec.Arm{
			Name:   a,
			Binary: spec.BinaryRef{Version: a},
			Config: spec.ConfigArm{Mode: spec.Baseline},
		})
	}
	return c
}

func scenarios(ids ...string) map[string]*spec.Scenario {
	out := map[string]*spec.Scenario{}
	for _, id := range ids {
		out[id] = &spec.Scenario{
			ID:    id,
			Route: "/page/octo",
			Load:  spec.Load{Test: "steady", Model: spec.Open, Duration: 30 * time.Second},
		}
	}
	return out
}

// The control that makes every comparison in this repository mean anything.
func TestInterleavingSequenceForTwoArms(t *testing.T) {
	p, err := Expand(campaign([]string{"A", "B"}, 5), scenarios("001-template-page"))
	if err != nil {
		t.Fatal(err)
	}
	// Rotate by one each rep: A B / B A / A B / B A / A B.
	if got, want := p.Sequence(""), "A B B A A B B A A B"; got != want {
		t.Fatalf("sequence = %q, want %q", got, want)
	}
}

func TestInterleavingSequenceForThreeArms(t *testing.T) {
	p, err := Expand(campaign([]string{"A", "B", "C"}, 3), scenarios("001-template-page"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := p.Sequence(""), "A B C B C A C A B"; got != want {
		t.Fatalf("sequence = %q, want %q", got, want)
	}
}

// Over n repetitions each arm must occupy each slot exactly once. Plain A,B,A,B fails
// this: arm A would take the post-cooldown slot every single time.
func TestEveryArmVisitsEveryPositionEqually(t *testing.T) {
	for _, n := range []int{2, 3, 4} {
		arms := make([]string, n)
		for i := range arms {
			arms[i] = string(rune('A' + i))
		}
		p, err := Expand(campaign(arms, n*2), scenarios("001-template-page"))
		if err != nil {
			t.Fatal(err)
		}

		counts := map[string]map[int]int{}
		for _, c := range p.Cells {
			if counts[c.ID.Arm] == nil {
				counts[c.ID.Arm] = map[int]int{}
			}
			counts[c.ID.Arm][c.PositionInRep]++
		}
		for arm, byPos := range counts {
			for pos := 0; pos < n; pos++ {
				if byPos[pos] != 2 {
					t.Fatalf("%d arms: arm %s took position %d %d times, want 2 — rotation is not balanced",
						n, arm, pos, byPos[pos])
				}
			}
		}
	}
}

func TestBlockedOrderRunsAllOfAThenAllOfB(t *testing.T) {
	c := campaign([]string{"A", "B"}, 3)
	c.Order = spec.Blocked
	c.OrderReason = "reproducing the old harness for a one-off comparison"

	p, err := Expand(c, scenarios("001-template-page"))
	if err != nil {
		t.Fatal(err)
	}
	// Blocked still groups by rep, so it is A B A B A B — the point is only that the
	// order never rotates. The confound it reintroduces is why it needs a reason.
	if got, want := p.Sequence(""), "A B A B A B"; got != want {
		t.Fatalf("sequence = %q, want %q", got, want)
	}
}

func TestBlockedOrderRequiresAReason(t *testing.T) {
	c := campaign([]string{"A", "B"}, 3)
	c.Order = spec.Blocked // no OrderReason

	if _, err := Expand(c, scenarios("001-template-page")); err == nil {
		t.Fatal("blocked order without a stated reason must not expand")
	}
}

func TestScenariosStayContiguous(t *testing.T) {
	c := campaign([]string{"A", "B"}, 2)
	c.Scenarios = []string{"001-template-page", "005-http-proxy"}

	p, err := Expand(c, scenarios("001-template-page", "005-http-proxy"))
	if err != nil {
		t.Fatal(err)
	}

	var seen []string
	for _, cell := range p.Cells {
		if len(seen) == 0 || seen[len(seen)-1] != cell.ID.Scenario {
			seen = append(seen, cell.ID.Scenario)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("scenarios were interleaved (%v); switching scenario restages config and deps", seen)
	}
}

func TestCellCountAndOrdinals(t *testing.T) {
	c := campaign([]string{"A", "B"}, 3)
	c.Scenarios = []string{"001-template-page", "005-http-proxy"}

	p, err := Expand(c, scenarios("001-template-page", "005-http-proxy"))
	if err != nil {
		t.Fatal(err)
	}
	if want := 2 * 3 * 2; len(p.Cells) != want {
		t.Fatalf("%d cells, want %d", len(p.Cells), want)
	}
	for i, cell := range p.Cells {
		if cell.Ordinal != i {
			t.Fatalf("cell %d has ordinal %d", i, cell.Ordinal)
		}
	}
}

func TestLoadIsResolvedScenarioThenCampaign(t *testing.T) {
	c := campaign([]string{"A", "B"}, 1)
	c.Load = spec.Load{Rate: 24000} // overrides only the rate

	sc := scenarios("001-template-page")
	sc["001-template-page"].Load = spec.Load{
		Test: "steady", Model: spec.Open, Rate: 16000, Duration: 45 * time.Second,
	}

	p, err := Expand(c, sc)
	if err != nil {
		t.Fatal(err)
	}
	got := p.Cells[0].Load
	if got.Rate != 24000 {
		t.Fatalf("rate = %d, campaign override should win", got.Rate)
	}
	if got.Duration != 45*time.Second {
		t.Fatalf("duration = %v, scenario default should survive", got.Duration)
	}
	if got.Test != "steady" {
		t.Fatalf("test = %q", got.Test)
	}
}

func TestMissingScenarioIsAnErrorNotASkippedCell(t *testing.T) {
	c := campaign([]string{"A", "B"}, 1)
	c.Scenarios = []string{"001-template-page", "099-does-not-exist"}

	_, err := Expand(c, scenarios("001-template-page"))
	if err == nil {
		t.Fatal("a campaign that silently measures less than it claimed must not expand")
	}
	if !strings.Contains(err.Error(), "099-does-not-exist") {
		t.Fatalf("error should name the missing scenario: %v", err)
	}
}

// --- identity and resume ---

func TestHashIsStableAcrossEquivalentSpecs(t *testing.T) {
	a, err := Expand(campaign([]string{"A", "B"}, 3), scenarios("001-template-page"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Expand(campaign([]string{"A", "B"}, 3), scenarios("001-template-page"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash != b.Hash {
		t.Fatal("identical specs produced different hashes")
	}
	if len(a.Short()) != 8 {
		t.Fatalf("short hash = %q", a.Short())
	}
}

func TestHashChangesWhenTheMeasurementWould(t *testing.T) {
	base, _ := Expand(campaign([]string{"A", "B"}, 3), scenarios("001-template-page"))

	tests := []struct {
		name   string
		mutate func(*spec.Campaign)
	}{
		{"more reps", func(c *spec.Campaign) { c.Reps = 5 }},
		{"different arm", func(c *spec.Campaign) { c.Arms[1].Binary.Version = "0.7.0" }},
		{"different knobs", func(c *spec.Campaign) {
			c.Arms[1].Config.Mode = spec.Tuned
			c.Arms[1].Config.Knobs = map[string]string{"workers": "128"}
		}},
		{"different rate", func(c *spec.Campaign) { c.Load.Rate = 24000 }},
		{"blocked order", func(c *spec.Campaign) {
			c.Order = spec.Blocked
			c.OrderReason = "because"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := campaign([]string{"A", "B"}, 3)
			tc.mutate(c)
			p, err := Expand(c, scenarios("001-template-page"))
			if err != nil {
				t.Fatal(err)
			}
			if p.Hash == base.Hash {
				t.Fatal("a change to what would be measured did not change the hash")
			}
		})
	}
}

func TestHashIgnoresPresentationOnlyChanges(t *testing.T) {
	base, _ := Expand(campaign([]string{"A", "B"}, 3), scenarios("001-template-page"))

	c := campaign([]string{"A", "B"}, 3)
	c.Question = "an entirely different question"
	c.Notes = "some notes"
	p, err := Expand(c, scenarios("001-template-page"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Hash != base.Hash {
		t.Fatal("reworded prose changed the plan hash, which would orphan a resume")
	}
}

func TestRemaining(t *testing.T) {
	p, _ := Expand(campaign([]string{"A", "B"}, 3), scenarios("001-template-page"))

	done := map[CellID]bool{p.Cells[0].ID: true, p.Cells[3].ID: true}
	rem := p.Remaining(done)

	if len(rem) != len(p.Cells)-2 {
		t.Fatalf("%d remaining, want %d", len(rem), len(p.Cells)-2)
	}
	for _, c := range rem {
		if done[c.ID] {
			t.Fatalf("completed cell %s was returned", c.ID)
		}
	}
	// Order must be preserved so a resume continues rather than reshuffles.
	for i := 1; i < len(rem); i++ {
		if rem[i].Ordinal <= rem[i-1].Ordinal {
			t.Fatal("remaining cells are out of order")
		}
	}
}

func TestArmOrdinals(t *testing.T) {
	p, _ := Expand(campaign([]string{"A", "B"}, 3), scenarios("001-template-page"))
	got := p.ArmOrdinals("001-template-page", "A")
	if len(got) != 3 {
		t.Fatalf("got %v, want 3 ordinals", got)
	}
	// A B B A A B -> A at 0, 3, 4
	if got[0] != 0 || got[1] != 3 || got[2] != 4 {
		t.Fatalf("ordinals = %v, want [0 3 4]", got)
	}
}

func TestSlugIsPathSafeAndKeepsIdentity(t *testing.T) {
	id := CellID{Scenario: "001-template-page", Arm: "0.5.0 (dev/dirty)", Rep: 3}
	got := id.Slug()
	for _, bad := range []string{"/", " ", "(", ")"} {
		if strings.Contains(got, bad) {
			t.Fatalf("slug %q contains %q", got, bad)
		}
	}
	if !strings.Contains(got, "001-template-page") || !strings.Contains(got, "rep3") {
		t.Fatalf("slug %q lost its identity", got)
	}
}

func TestExpandRejectsAnInvalidCampaign(t *testing.T) {
	if _, err := Expand(nil, nil); err == nil {
		t.Fatal("nil campaign must error")
	}
	one := campaign([]string{"A"}, 3) // a campaign compares arms
	if _, err := Expand(one, scenarios("001-template-page")); err == nil {
		t.Fatal("single-arm campaign must not expand")
	}
}
