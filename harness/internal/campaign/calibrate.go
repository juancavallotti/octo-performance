package campaign

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/loadgen"
	"github.com/juancavallotti/octo-performance/harness/internal/payload"
	"github.com/juancavallotti/octo-performance/harness/internal/plan"
	"github.com/juancavallotti/octo-performance/harness/internal/series"
	"github.com/juancavallotti/octo-performance/harness/internal/spec"
	"github.com/juancavallotti/octo-performance/harness/internal/stats"
	"github.com/juancavallotti/octo-performance/harness/internal/subject"
)

// Calibration is what a capacity ramp established for one scenario.
//
// It is archived with the campaign and printed in the report, because the rate every
// arm ran at is a decision the campaign made and a reader has to be able to check it.
// The old lab's rate was a shell variable with a comment above it explaining a
// measurement taken on a different day, on a machine that no longer behaved that way.
type Calibration struct {
	Scenario string `json:"scenario"`
	// Arm is the reference the ramp ran against. Every arm of the scenario then runs
	// at the same rate, so an arm that cannot hold it produces a saturation finding
	// rather than a quietly lower number.
	Arm     string    `json:"arm"`
	Version string    `json:"version,omitempty"`
	RanAt   time.Time `json:"ranAt"`

	Capacity spec.Capacity `json:"capacity"`
	Knee     stats.Knee    `json:"knee"`
	Fraction float64       `json:"fraction"`
	// Rate is what the campaign will offer. Zero means the ramp failed to establish
	// one, and the campaign falls back to the scenario's declared rate — saying so.
	Rate int    `json:"rate"`
	Note string `json:"note"`
}

// Calibrate runs a capacity ramp for one scenario and chooses the rate every arm of it
// will be offered.
//
// It starts and stops its own subject. The alternative — calibrating inside the first
// cell — would give that cell a different history from every other cell in the campaign,
// which is precisely the confounder the interleaved ordering exists to remove.
func (r *Runner) Calibrate(ctx context.Context, cell plan.Cell, dir string) (*Calibration, error) {
	cfg := r.cfg
	sc := cell.Scenario
	cap := sc.Capacity.WithDefaults()

	out := &Calibration{
		Scenario: sc.ID,
		Arm:      cell.Arm.Name,
		RanAt:    time.Now(),
		Capacity: cap,
		Fraction: cell.Load.CalibrateFraction,
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("campaign: %w", err)
	}
	subjectDir := filepath.Join(cfg.SubjectDir, "calibration", sc.ID)

	cfg.Log("calibrating %s against arm %s: %d rungs from %d to %d req/s, %s each",
		sc.ID, cell.Arm.Name, cap.Steps, cap.StartRate, cap.PeakRate, cap.Dwell)

	body, err := payload.Build(sc.Request.Payload)
	if err != nil {
		return nil, err
	}
	rendered, err := r.renderConfig(ctx, cell, dir, subjectDir)
	if err != nil {
		return nil, err
	}
	binary, err := cfg.ResolveBinary(cell.Arm.Binary)
	if err != nil {
		return nil, err
	}
	caps, err := r.prober.Probe(ctx, cfg.Hosts.Subject, binary)
	if err != nil {
		return nil, err
	}
	out.Version = caps.Version

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
		Env:        mergeEnv(r.deps[sc.ID].SubjectEnv(), cell.Arm.Env),
		ExtraFlags: cell.Arm.Flags,
		Log:        logFile,
		Endpoints:  subject.Endpoints{Base: cfg.Endpoints.base()},
	}
	if caps.Observability {
		req.Endpoints.Admin = cfg.Endpoints.admin()
	}

	h, err := subject.Start(ctx, cfg.Hosts.Subject, req)
	if err != nil {
		return nil, err
	}
	defer h.Stop(cfg.StopGrace)

	if _, err := h.AwaitReady(ctx, sc.ReadyRoute, cfg.ReadyTimeout); err != nil {
		return nil, err
	}

	targets, stages := stats.RampStages(cap.StartRate, cap.PeakRate, cap.Steps, cap.Dwell, cap.Transition)
	lreq := loadgen.Request{
		Phase:       loadgen.Capacity,
		URL:         r.url(sc),
		Method:      sc.Request.Verb(),
		Body:        sc.Request.Body,
		BodyBytes:   body.Bytes,
		ContentType: sc.Request.ContentType,
		Model:       spec.Open,
		StartRate:   cap.StartRate,
		Duration:    cap.Duration(),
		// Sized for the top of the ramp, not for the rung being offered. A pool that
		// grows during the probe is a generator scaling to keep up, and the rung at
		// which that started would be indistinguishable from the server's knee.
		Pool:   loadgen.SizePool(cap.PeakRate, cell.Load.ExpectedLatency, cell.Load.VUCap),
		OutDir: filepath.Join(dir, "k6-capacity"),
	}
	for _, s := range stages {
		lreq.Stages = append(lreq.Stages, loadgen.Stage{Target: s.Target, Duration: s.Duration})
	}

	run, err := cfg.LoadGen.Run(ctx, lreq)
	if err != nil {
		return nil, fmt.Errorf("campaign: capacity ramp: %w", err)
	}

	get := func(name string) series.Series {
		s, _ := run.Series.Get(name)
		return s
	}
	steps := stats.StepWindows(run.Started, targets, cap.Dwell, cap.Transition)
	out.Knee = stats.FindKnee(steps,
		get("k6.rps"), get("k6.dropped"), get("k6.failed"), get("k6.latency.mean"),
		stats.DefaultKnee())

	out.Rate = out.Knee.Chosen(out.Fraction)
	switch {
	case out.Rate > 0 && out.Knee.Found:
		out.Note = fmt.Sprintf("%d req/s, %.0f%% of a measured knee at %.0f. %s",
			out.Rate, out.Fraction*100, out.Knee.Rate, out.Knee.Note)
	case out.Rate > 0:
		// The ramp never folded over, so what it produced is a lower bound. Saying
		// "the knee is 40,000" here would turn a bound into a measurement, and the
		// campaign would run at half of a ceiling that was never located.
		out.Note = fmt.Sprintf("%d req/s, %.0f%% of the highest rate the ramp held. %s",
			out.Rate, out.Fraction*100, out.Knee.Note)
	default:
		out.Note = "the ramp established no rate. " + out.Knee.Note
	}
	cfg.Log("  %s", out.Note)

	if err := writeJSONFile(filepath.Join(dir, "calibration.json"), out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Runner) url(sc *spec.Scenario) string {
	return trimSlash(r.cfg.Endpoints.base()) + sc.Route
}
