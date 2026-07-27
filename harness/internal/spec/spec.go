package spec

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Model is how load is offered.
//
// It is recorded rather than inferred so that an open-model and a closed-model number
// can never end up in the same table. Published benchmarks are almost always closed —
// throughput against a fixed virtual-user population — and the "knee" is an artifact of
// that choice.
type Model string

const (
	// Open offers a fixed request rate regardless of how the server is coping, so
	// degradation surfaces honestly as rising latency and dropped iterations.
	Open Model = "open"
	// Closed loops a fixed VU population, so a slow server simply receives fewer
	// requests and throughput self-limits into a number that looks stable while
	// hiding the problem. Used only for cross-vendor comparability.
	Closed Model = "closed"
)

// Mode is which arm of the tuning comparison a config represents.
type Mode string

const (
	// Baseline has every tunable stripped, so the runtime falls back to whatever it
	// actually ships with. If a future version changes a default, this arm follows —
	// which is the regression the lab exists to catch.
	Baseline Mode = "baseline"
	// Tuned has the knobs written as literals, so the archived config records the
	// value that ran.
	Tuned Mode = "tuned"
)

// Order is how cells are sequenced.
type Order string

const (
	// Interleaved alternates arms and flips the within-rep order between reps, so no
	// arm systematically occupies the position that inherits the most thermal and
	// socket state. This is the default and should stay that way.
	Interleaved Order = "interleaved"
	// Blocked runs all of one arm then all of the next. It confounds any delta with
	// time-in-session, so it requires a stated reason that the report prints.
	Blocked Order = "blocked"
)

// Campaign is one comparison, start to finish.
type Campaign struct {
	Name string `yaml:"name"`
	// Question is what the campaign is trying to answer, in a sentence. It leads the
	// report.
	Question string `yaml:"question"`

	Scenarios []string `yaml:"scenarios"`
	Arms      []Arm    `yaml:"arms"`

	Reps        int           `yaml:"reps"`
	Order       Order         `yaml:"order,omitempty"`
	OrderReason string        `yaml:"orderReason,omitempty"`
	Cooldown    time.Duration `yaml:"cooldown,omitempty"`
	Seed        int64         `yaml:"seed,omitempty"`

	Load     Load     `yaml:"load,omitempty"`
	Topology Topology `yaml:"topology"`
	Gates    Gates    `yaml:"gates,omitempty"`

	// Notes is free text printed in the report. Unlike a directory-name label, it
	// travels with the result.
	Notes string `yaml:"notes,omitempty"`
}

// Arm is one thing being compared: a binary, a config mode, and its environment.
type Arm struct {
	Name   string            `yaml:"name"`
	Binary BinaryRef         `yaml:"binary"`
	Config ConfigArm         `yaml:"config"`
	Env    map[string]string `yaml:"env,omitempty"`
	Flags  []string          `yaml:"flags,omitempty"`
}

// BinaryRef says where the artifact under test comes from. Exactly one field is set.
type BinaryRef struct {
	// Version resolves to the release binary on the subject host.
	Version string `yaml:"version,omitempty"`
	// Path is an absolute path already present on the subject.
	Path string `yaml:"path,omitempty"`
	// Local is a path on the runner; the harness pushes it and records its sha256.
	Local string `yaml:"local,omitempty"`
}

// Resolved reports whether the reference names something. An arm that cannot say what
// it is testing does not run.
func (b BinaryRef) Resolved() bool {
	return b.Version != "" || b.Path != "" || b.Local != ""
}

func (b BinaryRef) String() string {
	switch {
	case b.Version != "":
		return "release " + b.Version
	case b.Path != "":
		return b.Path
	case b.Local != "":
		return "local " + b.Local
	default:
		return "unresolved"
	}
}

// ConfigArm selects the baseline or tuned rendering of the scenario's integration.
type ConfigArm struct {
	Mode Mode `yaml:"mode"`
	// Knobs are written as literals into the tuned rendering. Ignored for baseline.
	Knobs map[string]string `yaml:"knobs,omitempty"`
}

// Load describes the offered load. Zero fields inherit from the scenario.
type Load struct {
	// Test is smoke, steady, capacity or vus.
	Test  string `yaml:"test,omitempty"`
	Model Model  `yaml:"model,omitempty"`

	// Rate is the offered rate for an open-model test. Zero with Calibrate set means
	// the campaign measures it.
	Rate      int  `yaml:"rate,omitempty"`
	Calibrate bool `yaml:"calibrate,omitempty"`
	// CalibrateFraction is the share of the measured knee to run at, e.g. 0.5.
	CalibrateFraction float64 `yaml:"calibrateFraction,omitempty"`

	// VUs is the virtual-user count for a closed-model test.
	VUs int `yaml:"vus,omitempty"`

	Duration time.Duration `yaml:"duration,omitempty"`
	Warmup   time.Duration `yaml:"warmup,omitempty"`

	// ExpectedLatency sizes the VU pool by Little's law. It is an input to a recorded
	// value, not a threshold.
	ExpectedLatency time.Duration `yaml:"expectedLatency,omitempty"`
	// VUCap is a hard ceiling. Reaching it means the generator was scaling to keep up,
	// which is the condition that made the old results bimodal.
	VUCap int `yaml:"vuCap,omitempty"`
}

// Merge returns l with any field set in over taking precedence. Campaign over scenario
// over zero.
func (l Load) Merge(over Load) Load {
	out := l
	if over.Test != "" {
		out.Test = over.Test
	}
	if over.Model != "" {
		out.Model = over.Model
	}
	if over.Rate != 0 {
		// Naming a rate is an instruction not to measure one. Without this the
		// override would collide with the scenario's calibrate flag and the campaign
		// would be rejected as self-contradictory, which is the wrong reading: the
		// contradiction is between two layers, and the outer layer is the answer.
		out.Rate, out.Calibrate = over.Rate, false
	}
	if over.Calibrate {
		out.Calibrate, out.Rate = true, 0
	}
	if over.CalibrateFraction != 0 {
		out.CalibrateFraction = over.CalibrateFraction
	}
	if over.VUs != 0 {
		out.VUs = over.VUs
	}
	if over.Duration != 0 {
		out.Duration = over.Duration
	}
	if over.Warmup != 0 {
		out.Warmup = over.Warmup
	}
	if over.ExpectedLatency != 0 {
		out.ExpectedLatency = over.ExpectedLatency
	}
	if over.VUCap != 0 {
		out.VUCap = over.VUCap
	}
	return out
}

// Topology names the machines.
type Topology struct {
	Runner  Host `yaml:"runner"`
	Subject Host `yaml:"subject"`
	Deps    Host `yaml:"deps,omitempty"`
}

// Host is one machine the harness drives.
type Host struct {
	// Kind is "local" or "ssh". Local is a supported topology, not only a test
	// convenience — it reproduces the colocated laptop arrangement through the same
	// code path, and the saturation gate correctly flags it.
	Kind    string `yaml:"kind"`
	Addr    string `yaml:"addr,omitempty"`
	User    string `yaml:"user,omitempty"`
	KeyFile string `yaml:"keyFile,omitempty"`
	WorkDir string `yaml:"workDir,omitempty"`
}

// Local reports whether this host runs in-process.
func (h Host) Local() bool { return h.Kind == "" || h.Kind == "local" }

// Gates configures the validity thresholds.
//
// They ship deliberately loose. Nobody knows the right runner-CPU ceiling or
// pool-growth ratio yet, because the old lab never measured them — so every gate
// records its evidence unconditionally from the first campaign and the thresholds
// tighten from data.
type Gates struct {
	RunnerCPUCeilingPct  float64 `yaml:"runnerCpuCeilingPct,omitempty"`
	PoolGrowthSuspect    float64 `yaml:"poolGrowthSuspect,omitempty"`
	PoolGrowthInvalid    float64 `yaml:"poolGrowthInvalid,omitempty"`
	PeerPoolRatioInvalid float64 `yaml:"peerPoolRatioInvalid,omitempty"`
	MaxFailedRate        float64 `yaml:"maxFailedRate,omitempty"`
	MaxDroppedIterations float64 `yaml:"maxDroppedIterations,omitempty"`
	AchievedVsOfferedMin float64 `yaml:"achievedVsOfferedMin,omitempty"`
	MaxClockDriftMs      float64 `yaml:"maxClockDriftMs,omitempty"`
	// ObserveOnly records every gate's evidence but never escalates past Suspect.
	// The first campaigns run this way so thresholds can be calibrated from data
	// rather than guessed.
	ObserveOnly bool `yaml:"observeOnly,omitempty"`
}

// Thresholds are the scenario's k6 pass/fail bounds.
type Thresholds struct {
	P95 time.Duration `yaml:"p95,omitempty"`
	P99 time.Duration `yaml:"p99,omitempty"`
}

// Scenario is one workload: the integration under test, its knobs, and its load shape.
type Scenario struct {
	// ID is the directory name and is filled in by the loader.
	ID string `yaml:"-"`
	// Dir is where the scenario was loaded from.
	Dir string `yaml:"-"`

	Route      string `yaml:"route"`
	ReadyRoute string `yaml:"readyRoute,omitempty"`

	// Integration is the runtime config, relative to the scenario directory. It
	// defaults to octo/integration.yaml, which is where every scenario keeps it.
	Integration string `yaml:"integration,omitempty"`

	// Tunables names the knobs this scenario exposes, with the node path each lives
	// at. A bare name defaults to the root-flow path, because workers, buffer and
	// pool are root-flow only and a sub-flow declaring one would be a different
	// measurement.
	Tunables []Selector `yaml:"tunables"`

	Load       Load       `yaml:"load"`
	Thresholds Thresholds `yaml:"thresholds,omitempty"`

	// RequiresCEL is an expression that must compile and return true against the
	// artifact under test. A function that is present but behaves differently fails
	// as loudly as one that is absent.
	RequiresCEL string `yaml:"requiresCel,omitempty"`

	Setup    string `yaml:"setup,omitempty"`
	Teardown string `yaml:"teardown,omitempty"`

	// Calibration records why the rate is what it is. In the old lab this reasoning
	// was the most valuable line in scenario.env and existed only as a shell comment,
	// so it never reached a report.
	Calibration string `yaml:"calibration,omitempty"`
}

// Selector locates a tunable knob in the integration document.
type Selector struct {
	Name string `yaml:"name"`
	// Path is a node path such as "flows[*].workers". Empty defaults to the root-flow
	// path for Name.
	Path string `yaml:"path,omitempty"`
}

// ResolvedPath returns Path, or the root-flow default for Name.
func (s Selector) ResolvedPath() string {
	if s.Path != "" {
		return s.Path
	}
	return "flows[*]." + s.Name
}

// UnmarshalYAML accepts either a bare name or a {name, path} mapping, so the common
// case stays terse.
func (s *Selector) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		s.Name = n.Value
		return nil
	}
	type raw Selector
	var r raw
	if err := n.Decode(&r); err != nil {
		return err
	}
	*s = Selector(r)
	return nil
}

// LoadCampaign reads a campaign spec, applies defaults and validates it.
func LoadCampaign(path string) (*Campaign, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("spec: opening campaign: %w", err)
	}
	defer f.Close()
	return ParseCampaign(f)
}

// ParseCampaign reads a campaign spec from r.
func ParseCampaign(r io.Reader) (*Campaign, error) {
	var c Campaign
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true) // a mistyped key is a silent behaviour change; refuse it
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("spec: parsing campaign: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// LoadScenario reads scenario.yaml from a scenario directory.
func LoadScenario(dir string) (*Scenario, error) {
	path := filepath.Join(dir, "scenario.yaml")
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("spec: opening scenario: %w", err)
	}
	defer f.Close()

	var s Scenario
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("spec: parsing %s: %w", path, err)
	}
	s.Dir = dir
	s.ID = filepath.Base(strings.TrimSuffix(dir, string(filepath.Separator)))
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

func (c *Campaign) applyDefaults() {
	if c.Reps == 0 {
		c.Reps = 5
	}
	if c.Order == "" {
		c.Order = Interleaved
	}
	if c.Cooldown == 0 {
		c.Cooldown = 30 * time.Second
	}
	if c.Topology.Runner.Kind == "" {
		c.Topology.Runner.Kind = "local"
	}
	if c.Topology.Subject.Kind == "" {
		c.Topology.Subject.Kind = "local"
	}
	for i := range c.Arms {
		if c.Arms[i].Config.Mode == "" {
			c.Arms[i].Config.Mode = Baseline
		}
	}
	c.Gates = c.Gates.withDefaults()
}

func (g Gates) withDefaults() Gates {
	if g.RunnerCPUCeilingPct == 0 {
		g.RunnerCPUCeilingPct = 80
	}
	if g.PoolGrowthSuspect == 0 {
		g.PoolGrowthSuspect = 1.0 // any growth beyond the allocation is worth noting
	}
	if g.PoolGrowthInvalid == 0 {
		g.PoolGrowthInvalid = 2.0
	}
	if g.PeerPoolRatioInvalid == 0 {
		g.PeerPoolRatioInvalid = 2.0
	}
	if g.AchievedVsOfferedMin == 0 {
		g.AchievedVsOfferedMin = 0.99
	}
	if g.MaxClockDriftMs == 0 {
		g.MaxClockDriftMs = 50
	}
	return g
}

// Validate refuses a spec that cannot produce a defensible result.
func (c *Campaign) Validate() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if strings.TrimSpace(c.Name) == "" {
		add("campaign needs a name")
	}
	if len(c.Scenarios) == 0 {
		add("campaign names no scenarios")
	}
	if len(c.Arms) < 2 {
		add("a campaign compares arms: %d given, need at least 2", len(c.Arms))
	}
	if c.Reps < 1 {
		add("reps must be positive, got %d", c.Reps)
	}

	seen := map[string]bool{}
	for i, a := range c.Arms {
		switch {
		case strings.TrimSpace(a.Name) == "":
			add("arm %d has no name", i)
		case seen[a.Name]:
			add("arm name %q is used twice", a.Name)
		default:
			seen[a.Name] = true
		}
		// No version, no result: an arm that cannot name its artifact does not run.
		if !a.Binary.Resolved() {
			add("arm %q does not say which binary to test", a.Name)
		}
		if a.Config.Mode != Baseline && a.Config.Mode != Tuned {
			add("arm %q has config mode %q, want %q or %q", a.Name, a.Config.Mode, Baseline, Tuned)
		}
		if a.Config.Mode == Baseline && len(a.Config.Knobs) > 0 {
			add("arm %q is a baseline but sets knobs; baseline strips, it never hardcodes", a.Name)
		}
	}

	switch c.Order {
	case Interleaved:
	case Blocked:
		if strings.TrimSpace(c.OrderReason) == "" {
			add("order %q confounds any delta with time-in-session; state an orderReason", Blocked)
		}
	default:
		add("unknown order %q", c.Order)
	}

	// The load is deliberately not validated here. A campaign's load block is an
	// override layer over each scenario's defaults, so a campaign that sets only a
	// duration is perfectly well formed — the rate lives in the scenario. What has to
	// be coherent is the *resolved* load, which only exists once the two are merged,
	// so [Load.Validate] is called per cell during expansion.

	return errors.Join(errs...)
}

// Validate checks a fully resolved load — scenario defaults with campaign overrides
// already applied. Anything less than that is only half a specification.
func (l Load) Validate() error {
	var errs []error
	if l.Duration <= 0 {
		errs = append(errs, errors.New("needs a duration"))
	}
	switch l.Model {
	case Closed:
		if l.VUs == 0 {
			errs = append(errs, errors.New("a closed-model load needs vus"))
		}
	case Open, "":
		if l.Rate == 0 && !l.Calibrate {
			errs = append(errs, errors.New("an open-model load needs a rate, or calibrate: true"))
		}
		if l.Calibrate && l.Rate != 0 {
			errs = append(errs, errors.New("a load cannot both calibrate its rate and declare one"))
		}
		if l.Calibrate && (l.CalibrateFraction <= 0 || l.CalibrateFraction >= 1) {
			errs = append(errs, fmt.Errorf(
				"calibrateFraction must be between 0 and 1, got %v", l.CalibrateFraction))
		}
	default:
		errs = append(errs, fmt.Errorf("unknown load model %q", l.Model))
	}
	return errors.Join(errs...)
}

// Validate checks a scenario.
func (s *Scenario) Validate() error {
	var errs []error
	if strings.TrimSpace(s.Route) == "" {
		errs = append(errs, errors.New("scenario needs a route"))
	}
	names := map[string]bool{}
	for _, t := range s.Tunables {
		if strings.TrimSpace(t.Name) == "" {
			errs = append(errs, errors.New("a tunable has no name"))
			continue
		}
		if names[t.Name] {
			errs = append(errs, fmt.Errorf("tunable %q is declared twice", t.Name))
		}
		names[t.Name] = true
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("scenario %s: %w", s.ID, err)
	}
	return nil
}

// IntegrationPath is the runtime config this scenario runs.
//
// One file per scenario, from which both arms are derived — the baseline by stripping
// every tunable so the runtime falls back to its own defaults, the tuned arm by
// rewriting them as literals. Two checked-in configs would drift, and the drift would
// look like a result.
func (s *Scenario) IntegrationPath() string {
	rel := s.Integration
	if rel == "" {
		rel = filepath.Join("octo", "integration.yaml")
	}
	return filepath.Join(s.Dir, rel)
}

// TunableNames returns the knob names this scenario exposes.
func (s *Scenario) TunableNames() []string {
	out := make([]string, len(s.Tunables))
	for i, t := range s.Tunables {
		out[i] = t.Name
	}
	return out
}
