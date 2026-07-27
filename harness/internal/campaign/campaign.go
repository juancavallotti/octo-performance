// Package campaign is the orchestrator. It is the only package that sequences effects.
//
// [Runner.RunCell] exists once and replaces four near-identical loops in the old
// lab — run-bench.sh, run-vuramp.sh, sweep.sh and run-profile.sh — each of which
// re-implemented start, warm up, sample, measure, stop, cool down around a copy-pasted
// k6 invocation, and each of which drifted. A sweep is now reps: 1 with an arm per grid
// point. A capacity probe is a load test with stages. A VU ramp is the closed model
// with an arm per level. No new programs.
//
// Everything with an effect arrives as an interface, so the whole procedure runs
// against a fake subject in an ordinary go test in seconds — which is what makes it
// possible to change the orchestration without an eight-hour campaign to find out.
package campaign

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/agent"
	"github.com/juancavallotti/octo-performance/harness/internal/collect"
	"github.com/juancavallotti/octo-performance/harness/internal/exec"
	"github.com/juancavallotti/octo-performance/harness/internal/gate"
	"github.com/juancavallotti/octo-performance/harness/internal/loadgen"
	"github.com/juancavallotti/octo-performance/harness/internal/plan"
	"github.com/juancavallotti/octo-performance/harness/internal/promx"
	"github.com/juancavallotti/octo-performance/harness/internal/render"
	"github.com/juancavallotti/octo-performance/harness/internal/result"
	"github.com/juancavallotti/octo-performance/harness/internal/series"
	"github.com/juancavallotti/octo-performance/harness/internal/spec"
	"github.com/juancavallotti/octo-performance/harness/internal/stats"
	"github.com/juancavallotti/octo-performance/harness/internal/subject"
	"gopkg.in/yaml.v3"
)

// Hosts are the machines a campaign drives.
type Hosts struct {
	// Runner carries the harness and the load generator.
	Runner exec.Runner
	// Subject carries the runtime under test. It may be the same machine as Runner:
	// that is the colocated laptop topology, which is supported and which the
	// saturation gate correctly marks suspect.
	Subject exec.Runner
}

// Endpoints is where the subject answers, as seen from the runner.
type Endpoints struct {
	// Host is the address the runner reaches the subject on.
	Host string
	// WorkloadPort is where the integration serves.
	WorkloadPort int
	// AdminPort is the runtime's admin port.
	AdminPort int
}

func (e Endpoints) base() string {
	return fmt.Sprintf("http://%s:%d", hostOr(e.Host), e.WorkloadPort)
}

func (e Endpoints) admin() string {
	return fmt.Sprintf("http://%s:%d", hostOr(e.Host), e.AdminPort)
}

func hostOr(h string) string {
	if h == "" {
		return "127.0.0.1"
	}
	return h
}

// Config is everything a campaign runner needs that is not the plan.
type Config struct {
	Hosts     Hosts
	Endpoints Endpoints

	// Dir is where artifacts land, on the machine running the harness.
	Dir string

	// SubjectDir is where per-cell files are staged on the SUBJECT, and it must be
	// absolute. It defaults to Dir, which is right only while the two machines are
	// the same one.
	//
	// The distinction is not cosmetic. Every path handed to the subject — the config
	// to read, the directory to run in — is interpreted by the subject's filesystem,
	// and a relative path is not merely awkward there, it means something different.
	// Even on one machine it breaks: the runtime is started with its working
	// directory set to the cell, so a relative config path resolves against the cell
	// rather than against the harness's own working directory.
	SubjectDir string

	// ResolveBinary turns an arm's reference into a path on the subject host.
	ResolveBinary func(spec.BinaryRef) (string, error)

	// SubjectSource and RunnerSource sample the two machines. A nil source means
	// that machine is not sampled, which is recorded rather than assumed away.
	SubjectSource agent.Source
	RunnerSource  agent.Source

	// LoadGen offers the load. Injected so the procedure can be exercised without k6.
	LoadGen LoadGenerator

	Gates       []gate.Gate
	ObserveOnly bool
	Steady      stats.SteadyConfig

	// SampleInterval is how often the machines and the exposition are read.
	SampleInterval time.Duration
	// ReadyTimeout bounds start-up.
	ReadyTimeout time.Duration
	// StopGrace is how long the subject gets to shut down before it is killed.
	StopGrace time.Duration
	// Cooldown is the pause after a cell, so the next one does not inherit this
	// one's thermal state.
	Cooldown time.Duration

	// Log receives progress. An eight-hour campaign that says nothing is
	// indistinguishable from one that hung.
	Log func(format string, args ...any)
}

// LoadGenerator is how load is offered.
type LoadGenerator interface {
	Name() string
	Version(ctx context.Context) (string, error)
	Run(ctx context.Context, req loadgen.Request) (loadgen.Run, error)
}

func (c *Config) withDefaults() {
	// Absolute from here on. A subject resolves paths against its own working
	// directory, which is never the harness's, so a relative path is a different
	// path — silently, and only once a run reaches the subject.
	if abs, err := filepath.Abs(c.Dir); err == nil {
		c.Dir = abs
	}
	if c.SubjectDir == "" {
		c.SubjectDir = c.Dir
	}
	if c.SampleInterval <= 0 {
		c.SampleInterval = time.Second
	}
	if c.ReadyTimeout <= 0 {
		c.ReadyTimeout = 60 * time.Second
	}
	if c.StopGrace <= 0 {
		c.StopGrace = 15 * time.Second
	}
	// Steady detection is left zero here on purpose: it is scaled per cell from the
	// pass being measured, because a bound stated per minute means something
	// different on a four-second pass than on a sixty-second one.
	if c.Log == nil {
		c.Log = func(string, ...any) {}
	}
	if c.ResolveBinary == nil {
		c.ResolveBinary = DefaultResolver("")
	}
}

// DefaultResolver resolves a binary reference against a directory of releases.
//
// An empty root means ~/.octo-versions, which is where this lab keeps them. A
// reference that names nothing is an error rather than a default: an arm that cannot
// say what it is testing does not run.
func DefaultResolver(root string) func(spec.BinaryRef) (string, error) {
	return func(ref spec.BinaryRef) (string, error) {
		switch {
		case ref.Path != "":
			return ref.Path, nil
		case ref.Local != "":
			return ref.Local, nil
		case ref.Version != "":
			dir := root
			if dir == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return "", fmt.Errorf("campaign: resolving %s: %w", ref, err)
				}
				dir = filepath.Join(home, ".octo-versions")
			}
			return filepath.Join(dir, "octo-"+ref.Version), nil
		default:
			return "", fmt.Errorf("campaign: arm does not say which binary to test")
		}
	}
}

// Runner executes cells.
type Runner struct {
	cfg    Config
	prober *subject.Prober
}

// New returns a campaign runner.
func New(cfg Config) *Runner {
	cfg.withDefaults()
	return &Runner{cfg: cfg, prober: subject.NewProber()}
}

// RunCell is the one procedure.
//
// Peers are the sibling cells this one will be compared against. They are an input
// because the check that catches a generator collapsing on one cell and not another is
// necessarily cross-cell: 1,600 virtual users on one repetition and 7,113 on the next
// is only visible from outside a single cell.
func (r *Runner) RunCell(ctx context.Context, cell plan.Cell, peers []gate.Peer) (*result.Cell, error) {
	cfg := r.cfg
	started := time.Now()

	out := &result.Cell{
		Schema:        result.Schema,
		Campaign:      cell.ID.Campaign,
		Scenario:      cell.ID.Scenario,
		Arm:           cell.ID.Arm,
		Rep:           cell.ID.Rep,
		Ordinal:       cell.Ordinal,
		PositionInRep: cell.PositionInRep,
		StartedAt:     started,
		Harness:       harnessInfo(),
		Load:          loadSpec(cell.Load),
	}

	dir := filepath.Join(cfg.Dir, "cells", out.Slug())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("campaign: %w", err)
	}
	subjectDir := filepath.Join(cfg.SubjectDir, "cells", out.Slug())
	cfg.Log("cell %s (ordinal %d)", cell.ID, cell.Ordinal)

	// 1. Render the arm's config and put it where the subject will read it.
	rendered, err := r.renderConfig(ctx, cell, dir, subjectDir)
	if err != nil {
		return nil, err
	}
	out.Config = rendered.result

	// 2. Ask the artifact what it accepts. Never infer it from a version string.
	binary, err := cfg.ResolveBinary(cell.Arm.Binary)
	if err != nil {
		return nil, err
	}
	caps, err := r.prober.Probe(ctx, cfg.Hosts.Subject, binary)
	if err != nil {
		return nil, err
	}
	out.Binary = caps
	if err := writeFile(filepath.Join(dir, "help.txt"), caps.Help); err != nil {
		return nil, err
	}
	cfg.Log("  artifact: %s", caps.Summary())

	// 3. Refuse to start against a port something else already holds. A stale
	// process answers 404 quickly and reads as excellent throughput.
	if err := subject.AssertPortsFree(
		fmt.Sprintf("%s:%d", hostOr(cfg.Endpoints.Host), cfg.Endpoints.WorkloadPort),
	); err != nil {
		return nil, fmt.Errorf("campaign: %w", err)
	}

	logFile, err := os.Create(filepath.Join(dir, "octo.log"))
	if err != nil {
		return nil, fmt.Errorf("campaign: %w", err)
	}
	defer logFile.Close()

	req := subject.StartRequest{
		Caps:       caps,
		ConfigPath: rendered.path,
		WorkDir:    subjectDir,
		AdminAddr:  fmt.Sprintf(":%d", cfg.Endpoints.AdminPort),
		Metrics:    true,
		Env:        cell.Arm.Env,
		ExtraFlags: cell.Arm.Flags,
		Log:        logFile,
		Endpoints:  subject.Endpoints{Base: cfg.Endpoints.base()},
	}
	if caps.Observability {
		req.Endpoints.Admin = cfg.Endpoints.admin()
	}

	// 4. Start, and record the exact argv that ran.
	h, err := subject.Start(ctx, cfg.Hosts.Subject, req)
	if err != nil {
		return nil, err
	}
	out.Argv, out.Withheld = h.Argv, h.Withheld
	if err := writeJSONFile(filepath.Join(dir, "argv.json"), h.Argv); err != nil {
		return nil, err
	}

	stopped := false
	defer func() {
		if !stopped {
			h.Stop(cfg.StopGrace)
		}
	}()

	// 5. Wait for readiness, confirming a detected admin port actually answers.
	ready, err := h.AwaitReady(ctx, cell.Scenario.ReadyRoute, cfg.ReadyTimeout)
	out.Ready = ready
	if err != nil {
		return nil, err
	}
	cfg.Log("  ready by %s in %s", ready.Method, ready.ColdStart.Round(time.Millisecond))

	// 6. Ask the running process what it is, as distinct from what was started.
	identity, err := h.Identify(ctx)
	if err != nil {
		cfg.Log("  identity: %v", err)
	}
	out.Identity = identity

	// 7. Start every collector BEFORE the window opens. The window is chosen
	// afterwards, from the data; a collector that starts when the measurement starts
	// cannot answer whether the measurement was steady.
	subjectSampler, runnerSampler := r.startSamplers(ctx, h.PID())
	scraper := r.startScraper(ctx, h, caps)

	// 8. Warm-up. Its artifacts are kept and labelled, never silently discarded.
	if cell.Load.Warmup > 0 {
		warm, err := cfg.LoadGen.Run(ctx, r.loadRequest(cell, loadgen.Warmup, cell.Load.Warmup, dir))
		if err != nil {
			return nil, fmt.Errorf("campaign: warm-up: %w", err)
		}
		out.Warmup = &warm
		cfg.Log("  warm-up: %.0f rps achieved", warm.Summary.AchievedRPS())
	}

	// 9. The measured pass.
	measured, err := cfg.LoadGen.Run(ctx, r.loadRequest(cell, loadgen.Measured, cell.Load.Duration, dir))
	if err != nil {
		return nil, fmt.Errorf("campaign: measured pass: %w", err)
	}
	out.Measured = measured

	// 10. Stop the collectors, then the subject. Whole-lifetime cost comes from
	// being its parent, not from discovering its pid.
	scrapes := collect.Scrapes{}
	if scraper != nil {
		scrapes = scraper.Stop()
	}
	if subjectSampler != nil {
		out.Subject = subjectSampler.Stop()
	}
	if runnerSampler != nil {
		out.Runner = runnerSampler.Stop()
	}

	totals, err := h.Stop(cfg.StopGrace)
	stopped = true
	out.Totals = totals
	if err != nil {
		cfg.Log("  stop: %v", err)
	}

	// 11. Choose the window from the data. Both the detected one and the naive
	// fixed-offset one are kept, so detection can be audited across a campaign.
	r.selectWindow(out, measured)

	// 12. Window everything against the interval that was chosen.
	r.windowSubject(out)
	r.windowServer(out, scrapes, rendered.flows)
	r.fillHeadline(out, measured)

	// 13. Gate it. Gates read; they never mutate a measurement and never abort.
	out.Verdict = gate.Evaluate(cfg.Gates, r.evidence(out, cell, peers), cfg.ObserveOnly)

	// 14. Write it all down, atomically.
	out.EndedAt = time.Now()
	out.Elapsed = out.EndedAt.Sub(started)
	if err := r.writeArtifacts(out, dir, measured, scrapes); err != nil {
		return nil, err
	}
	cfg.Log("  %s — %.0f rps, client p95 %.2fms, verdict %s",
		out.Slug(), out.Headline.AchievedRPS, out.Headline.ClientP95Ms, out.Verdict.Level)

	// 15. Cool down, so the next cell does not inherit this one's thermal state.
	if cfg.Cooldown > 0 {
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(cfg.Cooldown):
		}
	}
	return out, nil
}

type renderedConfig struct {
	result render.Result
	path   string
	// flows are the flow names the runtime will label its metrics with, read from
	// the config that ran.
	flows []string
}

// renderConfig derives this arm's config and stages a complete, runnable copy of the
// integration beside it.
//
// The whole directory, not just the one file. A runtime config references its resources
// by paths relative to itself — scenario 001's template is "templates/page.tmpl" — so a
// rendered config staged on its own resolves nothing and the subject exits before it
// listens. Copying the tree also makes the cell directory a complete record: what ran
// is there in full, and re-running it needs nothing from the repository.
func (r *Runner) renderConfig(ctx context.Context, cell plan.Cell, dir, subjectDir string) (renderedConfig, error) {
	srcPath := cell.Scenario.IntegrationPath()
	src, err := os.ReadFile(srcPath)
	if err != nil {
		return renderedConfig{}, fmt.Errorf("campaign: reading the scenario's integration: %w", err)
	}

	res, err := render.Render(render.Request{
		Source:     src,
		SourceName: srcPath,
		Mode:       cell.Arm.Config.Mode,
		Tunables:   cell.Scenario.Tunables,
		Values:     cell.Arm.Config.Knobs,
	})
	if err != nil {
		return renderedConfig{}, fmt.Errorf("campaign: %w", err)
	}

	srcDir := filepath.Dir(srcPath)
	stageDir := filepath.Join(subjectDir, "octo")
	if err := r.stageTree(ctx, srcDir, stageDir); err != nil {
		return renderedConfig{}, err
	}

	// The rendered config replaces the source one in the staged tree, so what the
	// subject reads and what the cell archives are the same bytes.
	configPath := filepath.Join(stageDir, filepath.Base(srcPath))
	if err := r.cfg.Hosts.Subject.Put(ctx, configPath, 0o644, bytes.NewReader(res.Rendered)); err != nil {
		return renderedConfig{}, fmt.Errorf("campaign: staging the rendered config: %w", err)
	}

	// Because yaml.v3 re-emits with its own indentation, byte-exactness is not
	// available as provenance — so provenance says what changed and where instead,
	// which is strictly more than the archived file alone ever conveyed.
	if err := writeJSONFile(filepath.Join(dir, "config.render.json"), res); err != nil {
		return renderedConfig{}, err
	}
	return renderedConfig{result: res, path: configPath, flows: flowNames(res.Rendered)}, nil
}

// stageTree copies a scenario's integration directory to where the subject will read
// it, through the Runner so the same code serves a local subject and a remote one.
func (r *Runner) stageTree(ctx context.Context, src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return r.cfg.Hosts.Subject.MkdirAll(ctx, target, 0o755)
		}
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("campaign: staging %s: %w", rel, err)
		}
		defer f.Close()

		mode := fs.FileMode(0o644)
		if info, err := d.Info(); err == nil {
			mode = info.Mode().Perm()
		}
		return r.cfg.Hosts.Subject.Put(ctx, target, mode, f)
	})
}

func (r *Runner) startSamplers(ctx context.Context, pid int) (subjectS, runnerS *agent.Sampler) {
	if r.cfg.SubjectSource != nil && pid > 0 {
		s := agent.NewSampler(r.cfg.SubjectSource, pid, r.cfg.SampleInterval)
		if err := s.Start(ctx); err != nil {
			r.cfg.Log("  subject sampler: %v", err)
		} else {
			subjectS = s
		}
	}
	if r.cfg.RunnerSource != nil {
		s := agent.NewSampler(r.cfg.RunnerSource, os.Getpid(), r.cfg.SampleInterval)
		if err := s.Start(ctx); err != nil {
			r.cfg.Log("  runner sampler: %v", err)
		} else {
			runnerS = s
		}
	}
	return subjectS, runnerS
}

func (r *Runner) startScraper(ctx context.Context, h *subject.Handle, caps subject.Caps) *collect.PromScraper {
	if !caps.Metrics || h.Endpoints.Admin == "" {
		return nil
	}
	s := collect.NewPromScraper(h.Scrape, r.cfg.SampleInterval)
	if err := s.Start(ctx); err != nil {
		r.cfg.Log("  scraper: %v", err)
		return nil
	}
	return s
}

func (r *Runner) loadRequest(cell plan.Cell, phase loadgen.Phase, dur time.Duration, dir string) loadgen.Request {
	l := cell.Load
	pool := loadgen.SizePool(l.Rate, l.ExpectedLatency, l.VUCap)
	return loadgen.Request{
		Phase:    phase,
		URL:      strings.TrimRight(r.cfg.Endpoints.base(), "/") + cell.Scenario.Route,
		Model:    l.Model,
		Rate:     l.Rate,
		VUs:      l.VUs,
		Duration: dur,
		Pool:     pool,
		OutDir:   filepath.Join(dir, "k6-"+string(phase)),
	}
}

// selectWindow chooses the interval the cell's numbers describe.
//
// The subject's CPU *rate* corroborates: throughput can plateau while the runtime is
// still warming, because the offered rate caps what the generator can deliver and a
// saturated server looks identical to a settled one from the client alone.
func (r *Runner) selectWindow(out *result.Cell, run loadgen.Run) {
	rps, ok := run.Series.Get("k6.rps")
	if !ok || rps.Empty() {
		out.WindowNote = "the load pass produced no throughput series"
		return
	}
	rps = wholeSeconds(rps)
	if rps.Empty() {
		out.WindowNote = "the load pass was too short to contain a whole second of throughput"
		return
	}

	// The naive window is what a fixed warm-up offset would have chosen. Keeping it
	// beside the detected one is what makes detection auditable across a campaign
	// rather than something to be trusted.
	if naive, ok := stats.FixedWindow(rps, 10*time.Second); ok {
		out.NaiveWindow = naive
	}

	cfg := r.cfg.Steady
	if cfg.Bucket == 0 {
		cfg = stats.SteadyConfigFor(out.Load.Duration)
	}
	// Corroborate with the subject's CPU only where the sampler can actually see it.
	//
	// Throughput can plateau while the runtime is still warming, because the offered
	// rate caps what the generator delivers and a saturated server looks identical to
	// a settled one from the client alone. That is what a second series is for. But a
	// coarse source reports cumulative CPU at ten-millisecond resolution, and
	// differentiating that yields a series made mostly of quantisation steps — which
	// never looks flat, and would reject every window on a machine with no /proc.
	// Rejecting a window for want of evidence is not the same as rejecting it on
	// evidence, so the weaker claim is made explicitly instead.
	if out.Subject.Present() && out.Subject.Fidelity == agent.Full {
		if rate, _ := out.Subject.CPU().Rate(); !rate.Empty() {
			cfg.Corroborate = rate
		}
	}
	w, ok := stats.DetectSteady(rps, cfg)
	out.Window, out.WindowOK = w, ok
	if !ok {
		out.WindowNote = "no interval of this pass was flat enough to measure; there is no silent fallback"
		return
	}
	if !w.Corroborated {
		out.WindowNote = fmt.Sprintf(
			"accepted on throughput alone: the subject's CPU was not available to agree (sampler fidelity %q)",
			out.Subject.Fidelity)
	}
}

// wholeSeconds drops the two buckets a run only partly filled.
//
// k6 stamps every observation with a whole second, and a run neither starts nor stops
// on a second boundary. So the first bucket holds however much of a second elapsed
// between the first request and the next tick of the clock, and the last holds whatever
// was left when the run ended: a pass that stops 200 ms into its final second
// contributes a bucket at a fifth of the real rate.
//
// To a stability test that is indistinguishable from throughput collapsing. Over sixty
// seconds it inflates the coefficient of variation by a couple of points; over a short
// pass it makes a perfectly steady run undetectable. Both are wrong, and the second is
// how this was noticed.
//
// The correction is unconditional because the condition it corrects for is: the odds of
// a run beginning and ending exactly on a second boundary are not worth writing code
// for, and keeping one real bucket is worth less than the risk of keeping two false
// ones.
func wholeSeconds(s series.Series) series.Series {
	out := series.Series{Name: s.Name, Unit: s.Unit}
	if len(s.Points) <= 2 {
		return out
	}
	out.Points = append(out.Points, s.Points[1:len(s.Points)-1]...)
	return out
}

func (r *Runner) windowSubject(out *result.Cell) {
	if !out.Subject.Present() || !out.WindowOK {
		return
	}
	cpu := out.Subject.CPU().Window(out.Window.From, out.Window.To)
	if delta, ok := cpu.Delta(); ok && out.Window.Duration() > 0 {
		out.Headline.SubjectCPUPct = delta / out.Window.Duration().Seconds() * 100
	}
	rss := out.Subject.RSS().Window(out.Window.From, out.Window.To)
	if peak, ok := rss.Max(); ok {
		out.Headline.SubjectRSSPeakMB = peak / (1 << 20)
	}
	if drift, ok := rss.Delta(); ok {
		out.Headline.SubjectRSSDriftMB = drift / (1 << 20)
	}

	if out.Runner.Present() {
		busy := out.Runner.HostBusy().Window(out.Window.From, out.Window.To)
		if delta, ok := busy.Delta(); ok && out.Window.Duration() > 0 {
			out.Headline.RunnerCPUPct = delta / out.Window.Duration().Seconds() * 100
			out.Headline.HasRunnerCPU = true
		}
	}
}

// windowServer differences the two scrapes bracketing the window.
func (r *Runner) windowServer(out *result.Cell, scrapes collect.Scrapes, flows []string) {
	if !scrapes.Present() || !out.WindowOK {
		return
	}
	start, end, bracket, err := scrapes.Window(out.Window.From, out.Window.To)
	if err != nil {
		r.cfg.Log("  server window: %v", err)
		return
	}

	s := result.Server{Present: true, Bracket: bracket}
	byOutcome := promx.CounterDeltaByLabel(start, end, "octo_flow_messages_total", "outcome")
	s.MessagesCompleted = byOutcome["completed"]
	s.MessagesFailed = byOutcome["failed"]
	s.MessagesDropped = byOutcome["dropped"]

	s.Flows = flows
	// One flow per scenario today. A scenario with several would need its histograms
	// summed rather than picked, and summing them silently would report one flow's
	// latency as the whole integration's — so this measures the first and says which.
	if len(flows) > 0 {
		if h, ok := promx.HistogramWindow(start, end, "octo_flow_duration_seconds",
			promx.Labels{"flow": flows[0], "outcome": "completed"}); ok {
			if mean, ok := h.Mean(); ok {
				s.FlowMeanSeconds, s.HasFlowMean = mean, true
			}
			s.FlowP95 = h.Quantile(0.95)
			s.FlowP99 = h.Quantile(0.99)
		} else {
			r.cfg.Log("  no octo_flow_duration_seconds for flow %q", flows[0])
		}
	}

	// Over the bracket's own span, never over the window it sits beside.
	if span := bracket.Span().Seconds(); span > 0 {
		s.ThroughputRPS = s.MessagesCompleted / span
	}
	out.Server = s
}

// flowNames reads the flows out of the config that actually ran.
//
// Not from the scenario id. The obvious convention — strip the numeric prefix from
// "001-template-page" — yields "template-page", and the flow in that scenario is called
// "page". Every server-side histogram lookup then matches nothing, and because a
// missing histogram is indistinguishable from a runtime that served no traffic, the
// cell reports a server mean of zero and carries on. The config is the only thing that
// knows what the runtime will label its metrics with.
func flowNames(rendered []byte) []string {
	var doc struct {
		Flows []struct {
			Name string `yaml:"name"`
		} `yaml:"flows"`
	}
	if err := yaml.Unmarshal(rendered, &doc); err != nil {
		return nil
	}
	var out []string
	for _, f := range doc.Flows {
		if f.Name != "" {
			out = append(out, f.Name)
		}
	}
	return out
}

func (r *Runner) fillHeadline(out *result.Cell, run loadgen.Run) {
	h := &out.Headline
	s := run.Summary

	h.OfferedRate = run.OfferedRate
	h.AchievedRPS = s.AchievedRPS()
	if h.OfferedRate > 0 {
		h.AchievedRatio = h.AchievedRPS / h.OfferedRate
	}
	h.ClientMeanMs = s.Duration.Avg
	h.ClientP50Ms = s.Duration.Med
	h.ClientP95Ms = s.Duration.P95
	h.ClientP99Ms = s.Duration.P99
	h.ClientWaitingP95Ms = s.Waiting.P95

	h.Requests = s.Requests.Count
	h.Failed = s.Failed.Fails
	if s.Failed.Present {
		h.Failed = s.Failed.Rate * s.Requests.Count
	}
	h.Dropped = s.Dropped.Count

	h.PreAllocatedVUs = run.Pool.PreAllocatedVUs
	h.ObservedMaxVUs = run.ObservedMaxVUs()
	h.PoolGrowth = loadgen.Pool{
		PreAllocatedVUs: h.PreAllocatedVUs, ObservedMaxVUs: h.ObservedMaxVUs,
	}.Growth()

	if out.Server.HasFlowMean {
		h.ServerMeanMs = out.Server.FlowMeanSeconds * 1000
		h.HasServerMean = true
		// The comparison the old lab could not make. It published a client p95 of
		// 933 ms as the runtime's latency while the runtime's own mean flow duration
		// was 0.41 ms; the two numbers existed in the same directory.
		h.ClientServerGap = h.ClientP95Ms - h.ServerMeanMs
	}

	if h.Requests > 0 && out.Headline.SubjectCPUPct > 0 && out.WindowOK {
		cpuSeconds := h.SubjectCPUPct / 100 * out.Window.Duration().Seconds()
		windowed := h.AchievedRPS * out.Window.Duration().Seconds()
		if windowed > 0 {
			h.SubjectCPUmsPerReq = cpuSeconds * 1000 / windowed
		}
	}
	h.ColdStartMs = float64(out.Ready.ColdStart) / float64(time.Millisecond)
	if out.Totals.Rusage.Available {
		h.LifetimeCPUSecs = out.Totals.Rusage.CPUSeconds()
	}
}

func (r *Runner) evidence(out *result.Cell, cell plan.Cell, peers []gate.Peer) gate.Evidence {
	e := gate.Evidence{
		CellID:   out.Slug(),
		Scenario: out.Scenario,
		Arm:      out.Arm,
		Rep:      out.Rep,
		Ordinal:  out.Ordinal,
		Window:   out.Window,
		WindowOK: out.WindowOK,
		Peers:    peers,
	}

	s := out.Measured.Summary
	e.Load = gate.Load{
		Model:             string(out.Measured.Model),
		OfferedRate:       out.Measured.OfferedRate,
		AchievedRPS:       s.AchievedRPS(),
		Requests:          int64(s.Requests.Count),
		DroppedIterations: s.Dropped.Count,
		FailedRate:        s.Failed.Rate,
		ExitCode:          out.Measured.ExitCode,
		PreAllocatedVUs:   out.Measured.Pool.PreAllocatedVUs,
		MaxVUs:            out.Measured.Pool.MaxVUs,
		ObservedMaxVUs:    out.Measured.ObservedMaxVUs(),
		VUCap:             cell.Load.VUCap,
	}
	if rps, ok := out.Measured.Series.Get("k6.rps"); ok {
		e.Load.RPS = rps
	}

	if out.Runner.Present() {
		e.Runner = gate.Runner{
			Present:    true,
			Cores:      out.Runner.Static.Cores,
			CPUPctMean: out.Headline.RunnerCPUPct,
		}
		if busy, _ := out.Runner.HostBusy().Rate(); !busy.Empty() {
			e.Runner.CPU = busy
			if peak, ok := busy.Max(); ok {
				e.Runner.CPUPctPeak = peak * 100
			}
		}
	}
	if out.Subject.Present() {
		e.Subject = gate.Subject{
			Present:    true,
			Cores:      out.Subject.Static.Cores,
			CPUPctMean: out.Headline.SubjectCPUPct,
			RSSPeak:    out.Headline.SubjectRSSPeakMB,
			RSSDrift:   out.Headline.SubjectRSSDriftMB,
		}
		if rate, _ := out.Subject.CPU().Rate(); !rate.Empty() {
			e.Subject.CPU = rate
		}
	}
	if out.Server.Present {
		e.Server = gate.Server{
			Present:           true,
			MessagesCompleted: out.Server.MessagesCompleted,
			MessagesFailed:    out.Server.MessagesFailed,
			MessagesDropped:   out.Server.MessagesDropped,
			FlowMeanSeconds:   out.Server.FlowMeanSeconds,
			HasFlowMean:       out.Server.HasFlowMean,
			IdentityVersion:   out.Identity.Version,
			IntendedVersion:   out.Identity.Intended,
			ReadyThroughout:   true,
		}
	} else {
		e.Server.IntendedVersion = out.Identity.Intended
		e.Server.IdentityVersion = out.Identity.Version
	}

	// One machine has one clock. Reporting the offset as unmeasured on a colocated
	// topology would raise a finding about a discrepancy that cannot exist, and a
	// gate that cries wolf on the local loop is a gate people learn to skip.
	if r.cfg.Hosts.Runner == r.cfg.Hosts.Subject {
		e.Clock = gate.Clock{Measured: true}
	}

	e.Ready = gate.Ready{Method: string(out.Ready.Method), ColdStart: out.Headline.ColdStartMs}
	e.Caps = gate.Caps{
		AdminPortDetected: out.Binary.Observability,
		AdminPortAnswered: out.Ready.Confirmed,
		MetricsRequested:  out.Binary.Metrics,
	}
	return e
}

// Peer summarises this cell for the cross-cell checks the next ones will run.
func Peer(c *result.Cell) gate.Peer {
	return gate.Peer{
		CellID:          c.Slug(),
		Arm:             c.Arm,
		ObservedMaxVUs:  c.Headline.ObservedMaxVUs,
		PreAllocatedVUs: c.Headline.PreAllocatedVUs,
		ReadyMethod:     string(c.Ready.Method),
	}
}

func (r *Runner) writeArtifacts(out *result.Cell, dir string, run loadgen.Run, scrapes collect.Scrapes) error {
	if err := writeFrameCSV(filepath.Join(dir, "series.csv"), run.Series); err != nil {
		return err
	}
	if out.Subject.Present() {
		if err := writeJSONFile(filepath.Join(dir, "subject-samples.json"), out.Subject); err != nil {
			return err
		}
	}
	if scrapes.Present() {
		// Every scrape, not two snapshots. Once the window is detected rather than
		// slept through, a pair captured at the edges of the load pass brackets the
		// wrong interval.
		if err := writeScrapes(filepath.Join(dir, "metrics.ndjson"), scrapes); err != nil {
			return err
		}
	}
	return out.Write(dir)
}

func loadSpec(l spec.Load) result.LoadSpec {
	source := "scenario"
	if l.Calibrate {
		source = "calibrated"
	}
	return result.LoadSpec{
		Test: l.Test, Model: string(l.Model), Rate: l.Rate, VUs: l.VUs,
		Duration: l.Duration, Warmup: l.Warmup, ExpectedLatency: l.ExpectedLatency,
		Calibrated: l.Calibrate, RateSource: source,
	}
}

func writeFrameCSV(path string, f series.Frame) error {
	names := f.Names()
	if len(names) == 0 {
		return nil
	}
	// A long format, one row per point, so a series that is absent stays absent
	// rather than becoming a column of zeroes.
	var b strings.Builder
	b.WriteString("series,unit,timestamp,value\n")
	for _, n := range names {
		s, _ := f.Get(n)
		for _, p := range s.Points {
			fmt.Fprintf(&b, "%s,%s,%d,%g\n", s.Name, s.Unit, p.At.Unix(), p.V)
		}
	}
	return writeFile(path, b.String())
}

func writeScrapes(path string, s collect.Scrapes) error {
	var b strings.Builder
	for _, sc := range s.Samples {
		fmt.Fprintf(&b, "{\"at\":%q,\"body\":%s}\n",
			sc.At.Format(time.RFC3339Nano), quoteJSON(sc.Body))
	}
	return writeFile(path, b.String())
}

func quoteJSON(s string) string {
	b, err := jsonMarshalString(s)
	if err != nil {
		return `""`
	}
	return b
}

func writeFile(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("campaign: writing %s: %w", filepath.Base(path), err)
	}
	return nil
}
