package exec

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"sync"
	"time"
)

// DefaultCaptureLimit bounds retained output per stream when a Cmd does not say.
// Large enough for any help text, argv dump or k6 summary the harness asks for;
// small enough that a process looping on stderr cannot exhaust memory.
const DefaultCaptureLimit = 1 << 20 // 1 MiB

// Runner executes commands and transfers files on one machine.
//
// Implementations must be safe for concurrent use: the orchestrator samples a host
// while a load pass is running on it.
type Runner interface {
	// Name identifies the machine in logs and in the fingerprint, e.g. "local" or
	// "perf@10.0.0.4".
	Name() string

	// Run executes c to completion. The returned error is non-nil only when the
	// command could not be run; a command that ran and failed reports its status in
	// Result.ExitCode.
	Run(ctx context.Context, c Cmd) (Result, error)

	// Start launches c and returns without waiting. Streams that c leaves nil are
	// available as pipes on the returned Process.
	Start(ctx context.Context, c Cmd) (Process, error)

	// Put writes r to dst, creating parent directories as needed.
	Put(ctx context.Context, dst string, mode fs.FileMode, r io.Reader) error

	// Get opens src for reading. The caller closes it.
	Get(ctx context.Context, src string) (io.ReadCloser, error)

	// MkdirAll creates dir and any missing parents.
	MkdirAll(ctx context.Context, dir string, mode fs.FileMode) error

	// RemoveAll deletes dir and everything under it.
	RemoveAll(ctx context.Context, dir string) error

	// Close releases the transport. Running processes are not affected.
	Close() error
}

// Cmd is one program invocation.
type Cmd struct {
	// Path is the program to run. A path without a separator is looked up on PATH.
	Path string

	// Args is argv after the program name.
	//
	// This is a slice and not a string on purpose. Assembling octo's flags as text
	// is what made the old harness fragile, and it is also how --metrics reached a
	// build that could not parse it.
	Args []string

	// Dir is the working directory. Empty means the runner's default.
	Dir string

	// Env are variables added to the runner's environment, not a replacement for it.
	Env map[string]string

	// Stdin, when set, is written to the process. Start ignores this in favour of a
	// pipe when it is nil.
	Stdin io.Reader

	// Stdout and Stderr, when set, receive the streams and nothing is captured into
	// Result. Use them for anything long-lived — octo's log belongs in a file, not
	// in the orchestrator's heap.
	Stdout io.Writer
	Stderr io.Writer

	// CaptureLimit bounds retained output per stream. Zero means
	// DefaultCaptureLimit; negative means capture nothing.
	CaptureLimit int
}

// String renders the command the way a shell would show it, for logs and for the
// archived argv. It is a rendering only: nothing in this package parses it back.
func (c Cmd) String() string {
	out := c.Path
	for _, a := range c.Args {
		out += " " + a
	}
	return out
}

// Result is what a finished command produced.
type Result struct {
	// ExitCode is the process status. It is -1 when the process was terminated by a
	// signal.
	ExitCode int

	// Stdout and Stderr hold captured output, empty when the Cmd redirected the
	// stream or disabled capture.
	Stdout []byte
	Stderr []byte

	// Truncated reports that output exceeded the capture limit and was cut.
	Truncated bool

	// Signaled reports termination by a signal, with Signal naming it.
	Signaled bool
	Signal   string

	// Duration is wall clock from launch to exit.
	Duration time.Duration

	// Rusage is whole-lifetime resource usage, filled in where the transport can
	// supply it. Being the parent process is what makes this exact, which is the
	// main reason perf-agent supervises octo rather than discovering its pid.
	Rusage Rusage
}

// Err reports a non-zero exit as an error, for the callers that require success.
// Most callers should read ExitCode instead: a status is usually a measurement.
func (r Result) Err() error {
	if r.Signaled {
		return fmt.Errorf("killed by SIG%s", r.Signal)
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("exit status %d", r.ExitCode)
	}
	return nil
}

// Rusage is whole-lifetime CPU and memory for one process.
//
// This is not a sample. It covers the process from exec to exit, which is what makes
// it usable as a cost denominator: CPU-seconds per request over a whole load pass
// cannot be assembled from 1 Hz samples without inventing the gaps.
type Rusage struct {
	Available   bool
	UserSeconds float64
	SysSeconds  float64
	// MaxRSSBytes is the peak resident set, normalised to bytes. The kernel unit
	// differs between Linux and Darwin, which is a mistake worth exactly one test.
	MaxRSSBytes int64
	// VoluntaryCtxSwitches and InvoluntaryCtxSwitches distinguish a process that
	// blocked from one that was preempted — the difference between waiting on
	// something and being starved of a core.
	VoluntaryCtxSwitches   int64
	InvoluntaryCtxSwitches int64
}

// CPUSeconds is total processor time, user plus system.
func (r Rusage) CPUSeconds() float64 { return r.UserSeconds + r.SysSeconds }

// Process is a command that is still running.
type Process interface {
	// Stdin is the process's standard input, non-nil only when Cmd.Stdin was nil.
	Stdin() io.WriteCloser
	// Stdout is the process's standard output, non-nil only when Cmd.Stdout was nil.
	Stdout() io.Reader
	// Stderr is the process's standard error, non-nil only when Cmd.Stderr was nil.
	Stderr() io.Reader

	// PID is the process id, or zero on a transport that cannot report one.
	PID() int

	// Signal delivers sig.
	Signal(sig Signal) error

	// Wait blocks until the process exits and returns what it produced. It is safe
	// to call more than once and returns the same result.
	Wait() (Result, error)
}

// Signal is the subset of signals the harness sends. Naming them abstractly keeps
// the SSH transport honest: it forwards a signal request rather than pretending to
// hold an os.Process.
type Signal string

const (
	// SIGINT asks a process to stop the way a terminal would.
	SIGINT Signal = "INT"
	// SIGTERM is the graceful stop the harness sends first.
	SIGTERM Signal = "TERM"
	// SIGKILL is what it sends when graceful did not work.
	SIGKILL Signal = "KILL"
)

// Stop asks p to exit gracefully and escalates to SIGKILL after grace.
//
// A subject that has to be killed did not shut down cleanly, and its whole-lifetime
// Rusage is correspondingly less trustworthy — so the escalation is reported rather
// than hidden.
func Stop(p Process, grace time.Duration) (res Result, killed bool, err error) {
	if err := p.Signal(SIGTERM); err != nil {
		return Result{}, false, fmt.Errorf("exec: signalling: %w", err)
	}

	type outcome struct {
		res Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		r, e := p.Wait()
		done <- outcome{r, e}
	}()

	timer := time.NewTimer(grace)
	defer timer.Stop()

	select {
	case o := <-done:
		return o.res, false, o.err
	case <-timer.C:
		_ = p.Signal(SIGKILL)
		o := <-done
		return o.res, true, o.err
	}
}

// capture is a bounded, concurrency-safe sink for one output stream.
type capture struct {
	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool
}

func newCapture(limit int) *capture {
	if limit == 0 {
		limit = DefaultCaptureLimit
	}
	return &capture{limit: limit}
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.limit < 0 {
		return len(p), nil // discarded on purpose; report the full write
	}
	room := c.limit - len(c.buf)
	if room <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		c.buf = append(c.buf, p[:room]...)
		c.truncated = true
		return len(p), nil
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

func (c *capture) bytes() ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]byte, len(c.buf))
	copy(out, c.buf)
	return out, c.truncated
}
