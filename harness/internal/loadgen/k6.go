package loadgen

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/exec"
	"github.com/juancavallotti/octo-performance/harness/internal/series"
	"github.com/juancavallotti/octo-performance/harness/internal/spec"
)

//go:embed script.js
var scriptSource string

// Script is the load script, and ScriptSHA256 identifies it. Both are archived per
// cell: what was offered is as much a part of a result as what came back.
var (
	Script       = scriptSource
	ScriptSHA256 = func() string {
		sum := sha256.Sum256([]byte(scriptSource))
		return hex.EncodeToString(sum[:])
	}()
)

// Phase names what a load pass was for. It is recorded because a warm-up and a
// measured pass produce identical artifacts and only their labels distinguish them.
type Phase string

const (
	// Smoke is a correctness check before any load counts.
	Smoke Phase = "smoke"
	// Warmup is discarded from the result but kept on disk.
	Warmup Phase = "warmup"
	// Measured is the pass a number comes from.
	Measured Phase = "measured"
	// Capacity is a ramp looking for the knee.
	Capacity Phase = "capacity"
)

// Stage is one step of a capacity ramp.
type Stage struct {
	Target   int           `json:"target"`
	Duration time.Duration `json:"duration"`
}

// Request is one load pass.
type Request struct {
	Phase Phase
	URL   string

	Method      string
	Body        string
	ContentType string

	Model    spec.Model
	Rate     int
	VUs      int
	Duration time.Duration

	// Pool is the virtual-user allocation, computed by SizePool and recorded. It is
	// an input here rather than something the script works out, because the one
	// number that explains a collapsed run has to exist in a file afterwards.
	Pool Pool

	// Stages and StartRate describe a capacity ramp, and displace Rate when set.
	Stages    []Stage
	StartRate int

	// OutDir receives the summary, the log and the aggregated series.
	OutDir string
	Env    map[string]string
}

// Run is what one load pass produced.
type Run struct {
	Phase Phase `json:"phase"`

	// Model is recorded at the source rather than inferred later, so an open-model
	// and a closed-model number can never end up in the same table.
	Model       spec.Model `json:"model"`
	OfferedRate float64    `json:"offeredRate"`
	Pool        Pool       `json:"pool"`

	Started time.Time     `json:"started"`
	Ended   time.Time     `json:"ended"`
	Elapsed time.Duration `json:"elapsed"`

	// ExitCode is data. k6 exits 99 when a threshold is breached, which describes
	// the run rather than the harness, and the old lab wrote that 99 to a file that
	// nothing ever read.
	ExitCode int `json:"exitCode"`

	Summary Summary      `json:"summary"`
	Series  series.Frame `json:"-"`

	// RawRows is how many observations k6 emitted, and UnparsedRows how many did not
	// parse. One unparsed row is the CSV header.
	RawRows      int64 `json:"rawRows"`
	UnparsedRows int64 `json:"unparsedRows"`

	Argv         []string `json:"argv"`
	K6Version    string   `json:"k6Version"`
	ScriptSHA256 string   `json:"scriptSha256"`
}

// ObservedMaxVUs is how far the pool actually grew.
func (r Run) ObservedMaxVUs() int {
	if r.Summary.VUsMax.Present {
		return int(r.Summary.VUsMax.Max)
	}
	return r.Pool.ObservedMaxVUs
}

// K6 drives the k6 binary.
type K6 struct {
	// Binary is the k6 executable, "k6" by default.
	Binary string
	runner exec.Runner

	versionOnce sync.Once
	version     string
}

// NewK6 returns a driver that runs k6 through r.
func NewK6(r exec.Runner, binary string) *K6 {
	if binary == "" {
		binary = "k6"
	}
	return &K6{Binary: binary, runner: r}
}

// Name implements the generator contract.
func (k *K6) Name() string { return "k6" }

// Version asks the binary what it is. The output shape of k6 is not a stable API, so
// which version produced a series is part of the series' provenance.
func (k *K6) Version(ctx context.Context) (string, error) {
	var err error
	k.versionOnce.Do(func() {
		var res exec.Result
		res, err = k.runner.Run(ctx, exec.Cmd{Path: k.Binary, Args: []string{"version"}})
		if err != nil {
			return
		}
		k.version = strings.TrimSpace(string(res.Stdout) + string(res.Stderr))
	})
	return k.version, err
}

// Run performs one load pass.
//
// The per-observation CSV goes through a pipe and is folded into one-second buckets as
// it arrives; it is never written to disk. What lands in OutDir is the summary, k6's
// own log, and the aggregated series.
func (k *K6) Run(ctx context.Context, req Request) (Run, error) {
	if req.URL == "" {
		return Run{}, fmt.Errorf("loadgen: no URL to load")
	}
	if err := os.MkdirAll(req.OutDir, 0o755); err != nil {
		return Run{}, fmt.Errorf("loadgen: %w", err)
	}

	scriptPath := filepath.Join(req.OutDir, "script.js")
	if err := os.WriteFile(scriptPath, []byte(Script), 0o644); err != nil {
		return Run{}, fmt.Errorf("loadgen: staging script: %w", err)
	}
	summaryPath := filepath.Join(req.OutDir, "summary.json")

	fifoPath := filepath.Join(req.OutDir, "series.fifo")
	os.Remove(fifoPath)
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		return Run{}, fmt.Errorf("loadgen: creating the series pipe: %w", err)
	}
	defer os.Remove(fifoPath)

	env, err := environment(req, summaryPath)
	if err != nil {
		return Run{}, err
	}
	for key, v := range req.Env {
		env[key] = v
	}

	logFile, err := os.Create(filepath.Join(req.OutDir, "k6.log"))
	if err != nil {
		return Run{}, fmt.Errorf("loadgen: %w", err)
	}
	defer logFile.Close()

	args := []string{"run", "--out", "csv=" + fifoPath, "--quiet", "--log-output=stderr", scriptPath}
	cmd := exec.Cmd{Path: k.Binary, Args: args, Env: env, Stdout: logFile, Stderr: logFile}

	// The reader has to be waiting before k6 starts: opening a FIFO for reading
	// blocks until a writer arrives, and opening it for writing blocks until a
	// reader does. Whichever side goes second unblocks the first.
	agg := NewAggregator()
	readDone := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(fifoPath, os.O_RDONLY, 0)
		if err != nil {
			readDone <- fmt.Errorf("loadgen: opening the series pipe: %w", err)
			return
		}
		defer f.Close()
		readDone <- agg.Consume(f)
	}()

	started := time.Now()
	res, err := k.runner.Run(ctx, cmd)
	ended := time.Now()
	if err != nil {
		return Run{}, fmt.Errorf("loadgen: running k6: %w", err)
	}

	if err := <-readDone; err != nil {
		return Run{}, err
	}

	version, _ := k.Version(ctx)
	rows, unparsed := agg.Rows()

	run := Run{
		Phase:        req.Phase,
		Model:        model(req),
		OfferedRate:  float64(req.Rate),
		Pool:         req.Pool,
		Started:      started,
		Ended:        ended,
		Elapsed:      ended.Sub(started),
		ExitCode:     res.ExitCode,
		Series:       agg.Frame(),
		RawRows:      rows,
		UnparsedRows: unparsed,
		Argv:         append([]string{k.Binary}, args...),
		K6Version:    version,
		ScriptSHA256: ScriptSHA256,
	}
	run.Pool.ObservedMaxVUs = agg.ObservedMaxVUs()

	b, err := os.ReadFile(summaryPath)
	if err != nil {
		// No summary and a non-zero exit means k6 failed to start at all — a bad
		// flag, a missing binary, an unreachable URL. The log says which.
		return run, fmt.Errorf("loadgen: k6 exited %d and wrote no summary; see %s: %w",
			res.ExitCode, filepath.Join(req.OutDir, "k6.log"), err)
	}
	summary, err := ParseSummary(b)
	if err != nil {
		return run, err
	}
	run.Summary = summary
	if summary.VUsMax.Present {
		run.Pool.ObservedMaxVUs = int(summary.VUsMax.Max)
	}
	return run, nil
}

func model(req Request) spec.Model {
	if req.Model == spec.Closed {
		return spec.Closed
	}
	return spec.Open
}

// environment builds the variables the script reads.
func environment(req Request, summaryPath string) (map[string]string, error) {
	env := map[string]string{
		"PERF_URL":      req.URL,
		"PERF_MODEL":    string(model(req)),
		"PERF_DURATION": durationText(req.Duration),
		"PERF_PRE_VUS":  strconv.Itoa(req.Pool.PreAllocatedVUs),
		"PERF_MAX_VUS":  strconv.Itoa(req.Pool.MaxVUs),
		"PERF_SUMMARY":  summaryPath,
	}
	if req.Method != "" {
		env["PERF_METHOD"] = req.Method
	}
	if req.Body != "" {
		env["PERF_BODY"] = req.Body
	}
	if req.ContentType != "" {
		env["PERF_CONTENT_TYPE"] = req.ContentType
	}

	switch model(req) {
	case spec.Closed:
		if req.VUs <= 0 {
			return nil, fmt.Errorf("loadgen: a closed-model pass needs vus")
		}
		env["PERF_VUS"] = strconv.Itoa(req.VUs)
	default:
		if len(req.Stages) > 0 {
			stages := make([]map[string]any, len(req.Stages))
			for i, s := range req.Stages {
				stages[i] = map[string]any{"target": s.Target, "duration": durationText(s.Duration)}
			}
			b, err := json.Marshal(stages)
			if err != nil {
				return nil, fmt.Errorf("loadgen: encoding stages: %w", err)
			}
			env["PERF_STAGES"] = string(b)
			env["PERF_START_RATE"] = strconv.Itoa(req.StartRate)
			break
		}
		if req.Rate <= 0 {
			return nil, fmt.Errorf("loadgen: an open-model pass needs a rate")
		}
		env["PERF_RATE"] = strconv.Itoa(req.Rate)
	}
	return env, nil
}

// durationText renders a duration the way k6 expects it. Go's own formatting produces
// things like "1m0s", which k6 parses, and "1.5s", which it does not.
func durationText(d time.Duration) string {
	if d <= 0 {
		return "30s"
	}
	return strconv.FormatInt(int64(d/time.Millisecond), 10) + "ms"
}
