package exec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
)

// Local runs commands on the machine the harness is running on.
//
// It is a supported topology, not a test stub. A campaign whose runner and subject
// are both local reproduces the old colocated laptop arrangement through exactly the
// code path a real campaign uses — and the saturation gate then marks those cells
// suspect, which is the correct outcome and the one the old lab never produced. That
// property is what makes the fast iteration loop worth trusting: it exercises the
// same orchestrator as the eight-hour run.
type Local struct{}

// NewLocal returns a Runner for this machine.
func NewLocal() *Local { return &Local{} }

// Name implements Runner.
func (*Local) Name() string { return "local" }

// Close implements Runner. There is no transport to release.
func (*Local) Close() error { return nil }

// Run implements Runner.
func (l *Local) Run(ctx context.Context, c Cmd) (Result, error) {
	p, err := l.Start(ctx, c)
	if err != nil {
		return Result{}, err
	}
	// Start only creates pipes for streams the caller left nil, and Run owns the
	// whole lifetime, so anything it created must be drained or the process blocks
	// on a full pipe buffer.
	lp := p.(*localProcess)
	lp.drainToCaptures()
	return p.Wait()
}

// Start implements Runner.
func (l *Local) Start(ctx context.Context, c Cmd) (Process, error) {
	if c.Path == "" {
		return nil, errors.New("exec: command has no path")
	}

	cmd := osexec.Command(c.Path, c.Args...)
	cmd.Dir = c.Dir
	if len(c.Env) > 0 {
		env := os.Environ()
		for k, v := range c.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}

	lp := &localProcess{cmd: cmd, started: time.Now(), done: make(chan struct{})}

	if c.Stdin != nil {
		cmd.Stdin = c.Stdin
	} else {
		w, err := cmd.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("exec: stdin pipe: %w", err)
		}
		lp.stdin = w
	}

	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	} else {
		r, err := cmd.StdoutPipe()
		if err != nil {
			return nil, fmt.Errorf("exec: stdout pipe: %w", err)
		}
		lp.stdout, lp.outCap = r, newCapture(c.CaptureLimit)
	}

	if c.Stderr != nil {
		cmd.Stderr = c.Stderr
	} else {
		r, err := cmd.StderrPipe()
		if err != nil {
			return nil, fmt.Errorf("exec: stderr pipe: %w", err)
		}
		lp.stderr, lp.errCap = r, newCapture(c.CaptureLimit)
	}

	// Its own process group, so stopping a subject that spawned children stops the
	// children too. The old harness discovered octo's pid with pgrep and raced with
	// its own wrapper; a process group removes the question.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("exec: starting %s: %w", c.Path, err)
	}
	lp.started = time.Now()

	// Context cancellation kills the group and is recorded, so a cell cut short by a
	// deadline is distinguishable from one whose subject exited on its own.
	if ctx.Done() != nil {
		lp.watch(ctx)
	}
	return lp, nil
}

// Put implements Runner.
func (*Local) Put(_ context.Context, dst string, mode fs.FileMode, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("exec: creating %s: %w", filepath.Dir(dst), err)
	}
	// Write beside the target and rename, so a reader never sees a half-written
	// artifact. The old lab left truncated files behind on every interrupted run.
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".put-*")
	if err != nil {
		return fmt.Errorf("exec: staging %s: %w", dst, err)
	}
	defer os.Remove(tmp.Name())

	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return fmt.Errorf("exec: writing %s: %w", dst, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("exec: closing %s: %w", dst, err)
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return fmt.Errorf("exec: chmod %s: %w", dst, err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("exec: renaming into %s: %w", dst, err)
	}
	return nil
}

// Get implements Runner.
func (*Local) Get(_ context.Context, src string) (io.ReadCloser, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, fmt.Errorf("exec: reading %s: %w", src, err)
	}
	return f, nil
}

// MkdirAll implements Runner.
func (*Local) MkdirAll(_ context.Context, dir string, mode fs.FileMode) error {
	if err := os.MkdirAll(dir, mode); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return nil
}

// RemoveAll implements Runner.
func (*Local) RemoveAll(_ context.Context, dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return nil
}

// localProcess is a running os/exec command.
type localProcess struct {
	cmd     *osexec.Cmd
	started time.Time

	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	outCap *capture
	errCap *capture

	drain sync.WaitGroup

	once   sync.Once
	done   chan struct{} // closed once Wait has produced a result
	result Result
	err    error

	mu       sync.Mutex
	canceled bool
}

func (p *localProcess) Stdin() io.WriteCloser {
	if p.stdin == nil {
		return nil
	}
	return p.stdin
}

func (p *localProcess) Stdout() io.Reader {
	if p.stdout == nil {
		return nil
	}
	return p.stdout
}

func (p *localProcess) Stderr() io.Reader {
	if p.stderr == nil {
		return nil
	}
	return p.stderr
}

func (p *localProcess) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// drainToCaptures reads the pipes Start created into the capture buffers. Run calls
// it; a caller that took the pipes itself does not.
func (p *localProcess) drainToCaptures() {
	if p.stdout != nil {
		p.drain.Add(1)
		go func() {
			defer p.drain.Done()
			_, _ = io.Copy(p.outCap, p.stdout)
		}()
	}
	if p.stderr != nil {
		p.drain.Add(1)
		go func() {
			defer p.drain.Done()
			_, _ = io.Copy(p.errCap, p.stderr)
		}()
	}
}

// watch kills the process group when ctx is done. It exits when Wait closes done, so
// the watcher never outlives the process it watches.
func (p *localProcess) watch(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			p.mu.Lock()
			p.canceled = true
			p.mu.Unlock()
			_ = p.Signal(SIGKILL)
		case <-p.done:
		}
	}()
}

// Signal implements Process. It targets the whole process group.
func (p *localProcess) Signal(sig Signal) error {
	if p.cmd.Process == nil {
		return errors.New("exec: process not started")
	}
	var s syscall.Signal
	switch sig {
	case SIGINT:
		s = syscall.SIGINT
	case SIGTERM:
		s = syscall.SIGTERM
	case SIGKILL:
		s = syscall.SIGKILL
	default:
		return fmt.Errorf("exec: unknown signal %q", sig)
	}
	// Negative pid means the group. Falling back to the process alone covers the
	// window before the group exists.
	if err := syscall.Kill(-p.cmd.Process.Pid, s); err != nil {
		if err := p.cmd.Process.Signal(s); err != nil {
			return fmt.Errorf("exec: signalling %d: %w", p.cmd.Process.Pid, err)
		}
	}
	return nil
}

// Wait implements Process.
func (p *localProcess) Wait() (Result, error) {
	p.once.Do(func() {
		p.drain.Wait()
		waitErr := p.cmd.Wait()
		res := Result{Duration: time.Since(p.started)}

		if p.outCap != nil {
			res.Stdout, res.Truncated = p.outCap.bytes()
		}
		if p.errCap != nil {
			b, t := p.errCap.bytes()
			res.Stderr = b
			res.Truncated = res.Truncated || t
		}

		if st := p.cmd.ProcessState; st != nil {
			res.ExitCode = st.ExitCode()
			if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				res.Signaled = true
				res.Signal = ws.Signal().String()
			}
			res.Rusage = rusageFrom(st)
		}

		p.mu.Lock()
		canceled := p.canceled
		p.mu.Unlock()

		switch {
		case canceled:
			// A killed process is not a failed command, it is an interrupted
			// measurement, and the cell has to be able to say which it was.
			p.err = fmt.Errorf("exec: %s: killed by context cancellation", p.cmd.Path)
		case waitErr != nil:
			var ee *osexec.ExitError
			if !errors.As(waitErr, &ee) {
				// Not an exit status: the command genuinely could not be run.
				p.err = fmt.Errorf("exec: %s: %w", p.cmd.Path, waitErr)
			}
		}
		p.result = res
		close(p.done)
	})
	return p.result, p.err
}

// rusageFrom normalises the kernel's resource usage into the harness's units.
func rusageFrom(st *os.ProcessState) Rusage {
	ru, ok := st.SysUsage().(*syscall.Rusage)
	if !ok {
		return Rusage{}
	}
	return Rusage{
		Available:              true,
		UserSeconds:            st.UserTime().Seconds(),
		SysSeconds:             st.SystemTime().Seconds(),
		MaxRSSBytes:            maxRSSBytes(int64(ru.Maxrss)),
		VoluntaryCtxSwitches:   int64(ru.Nvcsw),
		InvoluntaryCtxSwitches: int64(ru.Nivcsw),
	}
}

// maxRSSBytes converts ru_maxrss to bytes.
//
// Linux reports kilobytes and Darwin reports bytes, a difference that silently
// changes every memory number by 1024x. TestMaxRSSIsPlausible allocates a known
// amount and asserts the result is in the right order of magnitude, which is the only
// way a unit test can catch this.
func maxRSSBytes(maxrss int64) int64 {
	if runtime.GOOS == "darwin" {
		return maxrss
	}
	return maxrss * 1024
}
