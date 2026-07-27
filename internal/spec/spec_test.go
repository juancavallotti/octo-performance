package spec

import (
	"strings"
	"testing"
	"time"
)

const validCampaign = `
name: regression-050-vs-060
question: "Did 0.6.0 regress against 0.5.0?"
scenarios: [001-template-page]
reps: 5
arms:
  - name: "0.5.0"
    binary: {version: "0.5.0"}
    config: {mode: baseline}
  - name: "0.6.0"
    binary: {version: "0.6.0"}
    config: {mode: baseline}
load:
  model: open
  rate: 16000
  duration: 60s
topology:
  runner: {kind: local}
  subject: {kind: local}
`

func TestParseCampaign(t *testing.T) {
	c, err := ParseCampaign(strings.NewReader(validCampaign))
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "regression-050-vs-060" {
		t.Fatalf("name = %q", c.Name)
	}
	if c.Reps != 5 || len(c.Arms) != 2 {
		t.Fatalf("reps=%d arms=%d", c.Reps, len(c.Arms))
	}
	if c.Load.Duration != time.Minute {
		t.Fatalf("duration = %v", c.Load.Duration)
	}
	if c.Order != Interleaved {
		t.Fatalf("order defaulted to %q, want %q", c.Order, Interleaved)
	}
	if c.Cooldown != 30*time.Second {
		t.Fatalf("cooldown = %v", c.Cooldown)
	}
}

// A mistyped key is a silent behaviour change — the run proceeds with a default
// nobody intended. Refuse it.
func TestParseCampaignRejectsUnknownFields(t *testing.T) {
	bad := strings.Replace(validCampaign, "reps: 5", "repetitions: 5", 1)
	_, err := ParseCampaign(strings.NewReader(bad))
	if err == nil {
		t.Fatal("a mistyped key must not be silently ignored")
	}
	if !strings.Contains(err.Error(), "repetitions") {
		t.Fatalf("error should name the offending field: %v", err)
	}
}

func TestValidateRefusesIndefensibleCampaigns(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Campaign)
		wantErr string
	}{
		{
			name:    "one arm is not a comparison",
			mutate:  func(c *Campaign) { c.Arms = c.Arms[:1] },
			wantErr: "at least 2",
		},
		{
			// Two runs in the old results are stamped vunknown-dev and were
			// published anyway.
			name:    "an arm that cannot name its binary",
			mutate:  func(c *Campaign) { c.Arms[1].Binary = BinaryRef{} },
			wantErr: "which binary",
		},
		{
			name:    "duplicate arm names",
			mutate:  func(c *Campaign) { c.Arms[1].Name = c.Arms[0].Name },
			wantErr: "used twice",
		},
		{
			// Baseline strips, it never hardcodes: writing workers: 8 into a
			// baseline freezes it at today's default and stops tracking the real one.
			name: "a baseline that sets knobs",
			mutate: func(c *Campaign) {
				c.Arms[0].Config.Knobs = map[string]string{"workers": "8"}
			},
			wantErr: "baseline strips",
		},
		{
			name:    "no scenarios",
			mutate:  func(c *Campaign) { c.Scenarios = nil },
			wantErr: "no scenarios",
		},
		{
			name:    "blocked order with no reason",
			mutate:  func(c *Campaign) { c.Order = Blocked },
			wantErr: "orderReason",
		},
		{
			name:    "open model with no rate and no calibration",
			mutate:  func(c *Campaign) { c.Load.Rate = 0 },
			wantErr: "needs a rate",
		},
		{
			name: "closed model with no vus",
			mutate: func(c *Campaign) {
				c.Load.Model = Closed
				c.Load.VUs = 0
			},
			wantErr: "needs vus",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseCampaign(strings.NewReader(validCampaign))
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(c)
			err = c.Validate()
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateAcceptsCalibrationInsteadOfARate(t *testing.T) {
	c, _ := ParseCampaign(strings.NewReader(validCampaign))
	c.Load.Rate = 0
	c.Load.Calibrate = true
	if err := c.Validate(); err != nil {
		t.Fatalf("calibrate should satisfy the rate requirement: %v", err)
	}
}

func TestValidateAcceptsBlockedOrderWithAReason(t *testing.T) {
	c, _ := ParseCampaign(strings.NewReader(validCampaign))
	c.Order = Blocked
	c.OrderReason = "reproducing a historical run"
	if err := c.Validate(); err != nil {
		t.Fatalf("blocked with a reason should validate: %v", err)
	}
}

func TestLoadMergeIsCampaignOverScenarioOverZero(t *testing.T) {
	scenario := Load{
		Test: "steady", Model: Open, Rate: 16000,
		Duration: 60 * time.Second, Warmup: 10 * time.Second,
		ExpectedLatency: 25 * time.Millisecond, VUCap: 16000,
	}
	campaign := Load{Rate: 24000, Duration: 90 * time.Second}

	got := scenario.Merge(campaign)

	if got.Rate != 24000 {
		t.Fatalf("rate = %d, campaign must win", got.Rate)
	}
	if got.Duration != 90*time.Second {
		t.Fatalf("duration = %v", got.Duration)
	}
	if got.Warmup != 10*time.Second {
		t.Fatalf("warmup = %v, scenario default must survive", got.Warmup)
	}
	if got.Test != "steady" || got.Model != Open {
		t.Fatalf("test/model lost: %q/%q", got.Test, got.Model)
	}
	if got.VUCap != 16000 {
		t.Fatalf("vuCap = %d", got.VUCap)
	}
}

func TestLoadMergeDoesNotMutate(t *testing.T) {
	base := Load{Rate: 100}
	_ = base.Merge(Load{Rate: 200})
	if base.Rate != 100 {
		t.Fatal("Merge mutated the receiver")
	}
}

// workers, buffer and pool are root-flow only; a sub-flow declaring one would be a
// different measurement, which a bare name must not silently permit.
func TestSelectorDefaultsToTheRootFlowPath(t *testing.T) {
	if got := (Selector{Name: "workers"}).ResolvedPath(); got != "flows[*].workers" {
		t.Fatalf("path = %q", got)
	}
	explicit := Selector{Name: "maxOpenConns", Path: "resources.db.settings.maxOpenConns"}
	if got := explicit.ResolvedPath(); got != "resources.db.settings.maxOpenConns" {
		t.Fatalf("explicit path = %q", got)
	}
}

func TestScenarioValidate(t *testing.T) {
	s := &Scenario{ID: "001", Route: "/page/octo",
		Tunables: []Selector{{Name: "workers"}, {Name: "workers"}}}
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Fatalf("a knob declared twice must be caught: %v", err)
	}

	if err := (&Scenario{ID: "001"}).Validate(); err == nil {
		t.Fatal("a scenario with no route must not validate")
	}
}

func TestBinaryRefString(t *testing.T) {
	for _, tc := range []struct {
		in   BinaryRef
		want string
	}{
		{BinaryRef{Version: "0.5.0"}, "release 0.5.0"},
		{BinaryRef{Path: "/opt/octo"}, "/opt/octo"},
		{BinaryRef{Local: "./octo"}, "local ./octo"},
		{BinaryRef{}, "unresolved"},
	} {
		if got := tc.in.String(); got != tc.want {
			t.Fatalf("%+v = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGateDefaultsAreConservative(t *testing.T) {
	c, err := ParseCampaign(strings.NewReader(validCampaign))
	if err != nil {
		t.Fatal(err)
	}
	g := c.Gates
	if g.RunnerCPUCeilingPct <= 0 || g.PoolGrowthInvalid <= 1 || g.AchievedVsOfferedMin <= 0 {
		t.Fatalf("gate defaults not applied: %+v", g)
	}
}
