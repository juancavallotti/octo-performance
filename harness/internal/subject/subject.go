package subject

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/exec"
)

// The admin routes the runtime serves from 0.5.0 onwards. There are exactly three,
// and naming them here rather than in string literals at each call site is the point:
// an invented fourth would be a probe that never answers and a cell that never starts.
const (
	// HealthzPath answers 200 while the process is alive.
	HealthzPath = "/healthz"
	// ReadyzPath answers 200 once every connector and flow has started, and 503 with
	// a reason otherwise. It goes false the moment a shutdown signal arrives, which
	// is why readiness is polled for start-up and never used to detect a stop.
	ReadyzPath = "/readyz"
	// MetricsPath is Prometheus exposition, served only with --metrics.
	MetricsPath = "/metrics"
)

// ReadyMethod is how a cell decided the subject was up.
//
// It is recorded because the two methods do not mean the same thing. /readyz answers
// once every connector and flow has started; a route probe answers as soon as the HTTP
// listener binds, which is earlier and says nothing about the flows behind it. Two
// arms established as ready by different methods have not measured the same interval,
// so a cold-start comparison across them is not a comparison.
type ReadyMethod string

const (
	// ByReadyz used the admin port.
	ByReadyz ReadyMethod = "readyz"
	// ByRoute polled the workload route, which is all a pre-0.5.0 build offers.
	ByRoute ReadyMethod = "route"
)

// Endpoints is where a running subject answers, as seen from the runner.
type Endpoints struct {
	// Base is the workload, e.g. "http://10.0.0.5:8080".
	Base string
	// Admin is the admin port, e.g. "http://10.0.0.5:39999". Empty when the artifact
	// has none.
	Admin string
}

// StartRequest is everything needed to start one arm.
type StartRequest struct {
	Caps Caps

	// ConfigPath is the rendered integration on the subject host.
	ConfigPath string
	// WorkDir is the cell's staging directory on the subject host.
	WorkDir string

	// AdminAddr is the admin listen address, e.g. ":39999". Ignored when the
	// artifact has no admin port.
	AdminAddr string
	// Metrics asks for Prometheus exposition. It is a request, not a guarantee: an
	// artifact that cannot serve it has the flag withheld and the fact recorded.
	Metrics bool
	// MetricsBlocks names blocks to time individually. Off unless a campaign says
	// otherwise, because a watched block pays for its own timing on the flow's
	// goroutine.
	MetricsBlocks string

	Env        map[string]string
	ExtraFlags []string

	// Endpoints is where the harness will reach it.
	Endpoints Endpoints

	// Log receives the runtime's stdout and stderr. It belongs in a file: a subject
	// under load can produce more than the orchestrator should hold.
	Log io.Writer
}

// Argv assembles the command line, withholding flags the artifact does not accept.
//
// Withheld is not an error and not silent. Comparing an arm that served metrics
// against one that could not is comparing two different things, so the campaign
// records what was withheld and the report says so.
func Argv(req StartRequest) (args []string, withheld []string) {
	args = []string{"run", "--config", req.ConfigPath}

	if req.Caps.Observability && req.AdminAddr != "" {
		args = append(args, "--observability-addr", req.AdminAddr)
	} else if req.AdminAddr != "" {
		withheld = append(withheld, "--observability-addr")
	}

	if req.Metrics {
		if req.Caps.Metrics {
			args = append(args, "--metrics")
		} else {
			withheld = append(withheld, "--metrics")
		}
	}

	if req.MetricsBlocks != "" {
		if req.Caps.MetricsBlocks {
			args = append(args, "--metrics-blocks", req.MetricsBlocks)
		} else {
			withheld = append(withheld, "--metrics-blocks")
		}
	}

	args = append(args, req.ExtraFlags...)
	return args, withheld
}

// Handle is a running subject.
type Handle struct {
	Caps      Caps
	Argv      []string
	Withheld  []string
	Endpoints Endpoints
	StartedAt time.Time

	proc   exec.Process
	runner exec.Runner
	client *http.Client
}

// PID is the subject's process id where the transport can report one.
func (h *Handle) PID() int {
	if h.proc == nil {
		return 0
	}
	return h.proc.PID()
}

// Start launches the subject. It does not wait for readiness.
func Start(ctx context.Context, r exec.Runner, req StartRequest) (*Handle, error) {
	if req.ConfigPath == "" {
		return nil, errors.New("subject: no config to run")
	}
	if req.Caps.Binary == "" {
		return nil, errors.New("subject: capabilities were never probed; ask the artifact before starting it")
	}

	args, withheld := Argv(req)
	cmd := exec.Cmd{
		Path:   req.Caps.Binary,
		Args:   args,
		Dir:    req.WorkDir,
		Env:    req.Env,
		Stdout: req.Log,
		Stderr: req.Log,
	}

	p, err := r.Start(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("subject: starting %s: %w", req.Caps.Version, err)
	}
	return &Handle{
		Caps:      req.Caps,
		Argv:      append([]string{req.Caps.Binary}, args...),
		Withheld:  withheld,
		Endpoints: req.Endpoints,
		StartedAt: time.Now(),
		proc:      p,
		runner:    r,
		client:    &http.Client{Timeout: 2 * time.Second},
	}, nil
}

// Ready is how and how quickly a subject came up.
type Ready struct {
	Method    ReadyMethod   `json:"method"`
	ColdStart time.Duration `json:"coldStart"`
	// Confirmed reports that a detected admin port actually answered. Detection is
	// an inference from help text; this is the observation.
	Confirmed bool `json:"adminPortConfirmed"`
	// Attempts is how many probes it took, which distinguishes a slow start from a
	// probe interval that is too coarse to measure one.
	Attempts int `json:"attempts"`
	// LastError is why the final failing probe failed, kept for the cell that never
	// came up.
	LastError string `json:"lastError,omitempty"`
}

// probeInterval bounds how precisely cold start can be measured. Cold starts run in
// the hundreds of milliseconds, so 10 ms keeps the quantisation well under the noise
// between repetitions rather than contributing to it.
const probeInterval = 10 * time.Millisecond

// AwaitReady polls until the subject is serving, and records which question it asked.
//
// When the artifact claims an admin port, that claim is confirmed before anything else
// happens: /healthz must answer. A detected-but-absent admin port would otherwise fall
// through to route polling and produce a cell that looks ordinary while measuring a
// different startup boundary than its sibling.
func (h *Handle) AwaitReady(ctx context.Context, route string, timeout time.Duration) (Ready, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	rd := Ready{Method: ByRoute}
	probe := strings.TrimRight(h.Endpoints.Base, "/") + route
	if h.Caps.Observability && h.Endpoints.Admin != "" {
		rd.Method = ByReadyz
		probe = strings.TrimRight(h.Endpoints.Admin, "/") + ReadyzPath
	}

	t := time.NewTicker(probeInterval)
	defer t.Stop()

	for {
		rd.Attempts++
		if err := h.get(ctx, probe); err == nil {
			rd.ColdStart = time.Since(h.StartedAt)
			break
		} else {
			rd.LastError = err.Error()
		}

		select {
		case <-ctx.Done():
			return rd, fmt.Errorf("subject: %s did not become ready within %s (%d probes to %s, last: %s)",
				h.Caps.Version, timeout, rd.Attempts, probe, rd.LastError)
		case <-t.C:
		}
	}

	if rd.Method == ByReadyz {
		// Positive confirmation. /readyz answered, so the port is real; asking
		// /healthz as well costs one request and closes the case where a foreign
		// service happens to answer 200 on that port.
		if err := h.get(ctx, strings.TrimRight(h.Endpoints.Admin, "/")+HealthzPath); err != nil {
			return rd, fmt.Errorf(
				"subject: %s advertises an admin port but %s did not answer (%w); "+
					"detection from help text was wrong in the optimistic direction",
				h.Caps.Version, HealthzPath, err)
		}
		rd.Confirmed = true
	}
	return rd, nil
}

// get performs one probe. Any 2xx counts; anything else is a reason to keep waiting.
func (h *Handle) get(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// Scrape reads the runtime's Prometheus exposition. It is empty when the artifact
// cannot serve it, which is a fact about the arm rather than a failure.
func (h *Handle) Scrape(ctx context.Context) (string, error) {
	if h.Endpoints.Admin == "" {
		return "", errors.New("subject: no admin port to scrape")
	}
	url := strings.TrimRight(h.Endpoints.Admin, "/") + MetricsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("subject: scraping %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("subject: scraping %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("subject: reading exposition: %w", err)
	}
	return string(b), nil
}

// Totals is what a stopped subject cost over its whole life.
type Totals struct {
	ExitCode int           `json:"exitCode"`
	Signaled bool          `json:"signaled,omitempty"`
	Signal   string        `json:"signal,omitempty"`
	Killed   bool          `json:"killed,omitempty"`
	Uptime   time.Duration `json:"uptime"`
	Rusage   exec.Rusage   `json:"rusage"`
}

// Stop shuts the subject down and returns its whole-lifetime cost.
//
// The escalation is reported: a runtime that had to be killed did not drain, so its
// totals include work it abandoned and its final metrics scrape may be missing
// counters that a clean shutdown would have flushed.
func (h *Handle) Stop(grace time.Duration) (Totals, error) {
	if h.proc == nil {
		return Totals{}, errors.New("subject: not started")
	}
	res, killed, err := exec.Stop(h.proc, grace)
	t := Totals{
		ExitCode: res.ExitCode,
		Signaled: res.Signaled,
		Signal:   res.Signal,
		Killed:   killed,
		Uptime:   time.Since(h.StartedAt),
		Rusage:   res.Rusage,
	}
	if err != nil {
		return t, fmt.Errorf("subject: stopping %s: %w", h.Caps.Version, err)
	}
	return t, nil
}

// AssertPortsFree refuses to start when something is already listening.
//
// A stale process on the workload port answers 404 quickly, and a load pass against it
// records excellent throughput for a runtime that is not running. That failure is
// silent in every artifact a run produces, which is what makes it worth one syscall
// per port beforehand.
func AssertPortsFree(addrs ...string) error {
	var errs []error
	for _, addr := range addrs {
		if addr == "" {
			continue
		}
		conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
		if err != nil {
			continue // nothing listening, which is what we want
		}
		conn.Close()
		errs = append(errs, fmt.Errorf(
			"something is already listening on %s; a stale process answers fast and reads as excellent throughput", addr))
	}
	return errors.Join(errs...)
}
