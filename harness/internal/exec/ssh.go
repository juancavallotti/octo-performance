package exec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SSHExitCode is what the ssh client itself returns when it could not establish or
// maintain the session, as distinct from a remote command that exited 255.
//
// The two are genuinely ambiguous over this protocol, which is why it is a named
// constant rather than a magic number buried in an if: a connection that dropped
// halfway through a load pass and a subject that exited 255 look identical from here,
// and the harness must say which it believes rather than pick silently.
const SSHExitCode = 255

// SSH runs commands on another machine through the system ssh client.
//
// The system client, deliberately, rather than a Go SSH library. Everything an operator
// has already arranged for reaching these hosts lives in the ssh client's world: agent
// forwarding, ~/.ssh/config, jump hosts, and — the one that matters for this lab —
// GCP's IAP tunnelling, which `gcloud compute config-ssh` writes as a ProxyCommand. A Go
// client would reimplement a subset of that and reach a subject VM with no external IP
// not at all.
//
// One connection is shared by every command through ControlMaster multiplexing, which is
// what makes 1 Hz remote sampling viable: a fresh TCP connection and key exchange per
// sample would cost tens of milliseconds and a new process on both machines, and the
// sampler would be a measurable load on the machine it is measuring.
type SSH struct {
	// Target is user@host, as ssh understands it.
	Target string
	// Options are extra -o settings and flags, applied before the target.
	Options []string
	// Binary is the ssh client, "ssh" by default.
	Binary string
	// ScpBinary is the file-transfer client, "scp" by default.
	ScpBinary string

	local *Local

	mu      sync.Mutex
	control string // the ControlPath socket, empty until dialled
	tmp     string
	closed  bool
}

// SSHConfig is what NewSSH needs.
type SSHConfig struct {
	Target  string
	KeyFile string
	// Options are appended verbatim, for the cases this struct does not anticipate.
	Options   []string
	Binary    string
	ScpBinary string
	// ConnectTimeout bounds the initial handshake.
	ConnectTimeout time.Duration
	// StrictHostKeyChecking is left to the operator's ssh config by default. It is
	// exposed because ephemeral VMs get a fresh host key every campaign, and a
	// harness that stops on a changed key at cell forty is worse than one that says
	// so at cell zero.
	StrictHostKeyChecking string
}

// NewSSH returns a runner for one machine.
//
// It does not connect. The first command dials, and the multiplexing socket persists
// from then on.
func NewSSH(cfg SSHConfig) (*SSH, error) {
	if strings.TrimSpace(cfg.Target) == "" {
		return nil, errors.New("exec: an ssh runner needs a target")
	}
	s := &SSH{
		Target:    cfg.Target,
		Binary:    or(cfg.Binary, "ssh"),
		ScpBinary: or(cfg.ScpBinary, "scp"),
		local:     NewLocal(),
	}

	tmp, err := os.MkdirTemp("", "perf-ssh-")
	if err != nil {
		return nil, fmt.Errorf("exec: %w", err)
	}
	s.tmp = tmp
	// A short path: a unix socket path is limited to about a hundred bytes, and the
	// failure when it is exceeded is an obscure bind error rather than a name that
	// says it was too long.
	s.control = filepath.Join(tmp, "c")

	timeout := cfg.ConnectTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	s.Options = []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + s.control,
		// Long enough to outlive a whole campaign's gap between cells. The socket is
		// removed by Close, so this bounds only an abandoned run.
		"-o", "ControlPersist=600",
		"-o", "ConnectTimeout=" + strconv.Itoa(int(timeout.Seconds())),
		"-o", "BatchMode=yes", // never block a headless campaign on a password prompt
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
	}
	if cfg.StrictHostKeyChecking != "" {
		s.Options = append(s.Options, "-o", "StrictHostKeyChecking="+cfg.StrictHostKeyChecking)
	}
	if cfg.KeyFile != "" {
		s.Options = append(s.Options, "-i", cfg.KeyFile, "-o", "IdentitiesOnly=yes")
	}
	s.Options = append(s.Options, cfg.Options...)
	return s, nil
}

// Name implements Runner.
func (s *SSH) Name() string { return s.Target }

// Ping opens the connection and reports what is on the far side. Called once, at
// campaign start, so an unreachable subject costs a second rather than a cell.
func (s *SSH) Ping(ctx context.Context) (string, error) {
	res, err := s.Run(ctx, Cmd{Path: "uname", Args: []string{"-sr"}})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("exec: %s: uname exited %d: %s",
			s.Target, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

// argv builds the local ssh invocation for a remote command.
func (s *SSH) argv(remote string) []string {
	out := append([]string{}, s.Options...)
	return append(out, s.Target, remote)
}

// remoteCommand renders c as one shell command for the far side.
//
// This is where the harness's own rule — argv is a slice, never a string — meets a
// protocol that only carries a string. There is no way around it: the SSH exec channel
// takes a command line and the login shell splits it. So every element is quoted here,
// once, by [quote], and the quoting is tested against the values that would break it
// rather than assumed.
func (s *SSH) remoteCommand(c Cmd) string {
	var b strings.Builder
	if c.Dir != "" {
		// Failing to change directory must not run the command somewhere else. Without
		// the &&, a mistyped staging path silently runs octo in the login directory,
		// where a relative config path resolves to a different file or to none.
		b.WriteString("cd " + quote(c.Dir) + " && ")
	}
	if len(c.Env) > 0 {
		b.WriteString("env")
		for _, k := range sortedKeys(c.Env) {
			b.WriteString(" " + quote(k+"="+c.Env[k]))
		}
		b.WriteString(" ")
	}
	// exec so the remote shell is replaced: the pid the harness signals is then the
	// program's own, not a shell that may or may not forward the signal.
	b.WriteString("exec " + quote(c.Path))
	for _, a := range c.Args {
		b.WriteString(" " + quote(a))
	}
	return b.String()
}

// Run implements Runner.
func (s *SSH) Run(ctx context.Context, c Cmd) (Result, error) {
	if err := s.check(); err != nil {
		return Result{}, err
	}
	local := Cmd{
		Path:         s.Binary,
		Args:         s.argv(s.remoteCommand(c)),
		Stdin:        c.Stdin,
		Stdout:       c.Stdout,
		Stderr:       c.Stderr,
		CaptureLimit: c.CaptureLimit,
	}
	res, err := s.local.Run(ctx, local)
	if err != nil {
		return res, fmt.Errorf("exec: %s: %w", s.Target, err)
	}
	// Whole-lifetime usage over this transport is the ssh client's, not the remote
	// program's, and reporting one as the other would put the cost of a load pass at
	// a few milliseconds of local socket handling. The subject's cost comes from the
	// /proc sampler instead, which measures the right process on the right machine.
	res.Rusage = Rusage{}
	return res, nil
}

// Start implements Runner.
//
// The remote pid is captured by having the far side write it before exec'ing, because
// nothing else on this transport can report it: ssh knows about a session, not about a
// process. That pid is what [SSH.signal] targets and what the sampler reads /proc for.
func (s *SSH) Start(ctx context.Context, c Cmd) (Process, error) {
	if err := s.check(); err != nil {
		return nil, err
	}

	pidPath := fmt.Sprintf("/tmp/perf-%d-%d.pid", os.Getpid(), time.Now().UnixNano())
	remote := "echo $$ > " + quote(pidPath) + "; " + s.remoteCommand(c)

	local := Cmd{
		Path:   s.Binary,
		Args:   s.argv(remote),
		Stdin:  c.Stdin,
		Stdout: c.Stdout,
		Stderr: c.Stderr,
	}
	p, err := s.local.Start(ctx, local)
	if err != nil {
		return nil, fmt.Errorf("exec: %s: %w", s.Target, err)
	}

	rp := &sshProcess{ssh: s, session: p, pidPath: pidPath}
	rp.pid = s.awaitPID(ctx, pidPath)
	return rp, nil
}

// awaitPID reads the pid the far side wrote. It polls because the write races the
// session's own start-up, and it gives up rather than blocking a campaign: a process
// with no pid is still running and still measurable through its ports, it simply cannot
// be sampled or signalled precisely, and that is recorded rather than fatal.
func (s *SSH) awaitPID(ctx context.Context, path string) int {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		res, err := s.Run(ctx, Cmd{Path: "cat", Args: []string{path}})
		if err == nil && res.ExitCode == 0 {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(res.Stdout))); err == nil && pid > 0 {
				return pid
			}
		}
		if ctx.Err() != nil {
			return 0
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0
}

// Put implements Runner.
func (s *SSH) Put(ctx context.Context, dst string, mode fs.FileMode, r io.Reader) error {
	if err := s.check(); err != nil {
		return err
	}
	if err := s.MkdirAll(ctx, filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	// Streamed through the shell rather than written locally and scp'd. The scp round
	// trip needs a real file on this side, and the harness's own artifacts — a rendered
	// config, a staged binary — often exist only as bytes in memory.
	remote := fmt.Sprintf("cat > %s && chmod %o %s", quote(dst), mode.Perm(), quote(dst))
	res, err := s.local.Run(ctx, Cmd{
		Path:  s.Binary,
		Args:  s.argv(remote),
		Stdin: r,
	})
	if err != nil {
		return fmt.Errorf("exec: %s: writing %s: %w", s.Target, dst, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exec: %s: writing %s: exit %d: %s",
			s.Target, dst, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

// Get implements Runner.
func (s *SSH) Get(ctx context.Context, src string) (io.ReadCloser, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	res, err := s.Run(ctx, Cmd{
		Path: "cat", Args: []string{src},
		// A fetched artifact is an octo log or a metrics dump, not a data stream. The
		// bound is generous and explicit rather than absent.
		CaptureLimit: 64 << 20,
	})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("exec: %s: reading %s: exit %d: %s",
			s.Target, src, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	if res.Truncated {
		return nil, fmt.Errorf("exec: %s: %s exceeded the transfer limit", s.Target, src)
	}
	return io.NopCloser(strings.NewReader(string(res.Stdout))), nil
}

// MkdirAll implements Runner.
func (s *SSH) MkdirAll(ctx context.Context, dir string, mode fs.FileMode) error {
	if err := s.check(); err != nil {
		return err
	}
	res, err := s.Run(ctx, Cmd{Path: "mkdir", Args: []string{"-p", dir}})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exec: %s: mkdir %s: exit %d: %s",
			s.Target, dir, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

// RemoveAll implements Runner.
func (s *SSH) RemoveAll(ctx context.Context, dir string) error {
	if err := s.check(); err != nil {
		return err
	}
	// Refuses to be handed something that would take the machine with it. This runs
	// against a host the operator does not have open in a terminal, so the blast
	// radius of a bad staging path is the whole campaign and possibly the VM.
	clean := filepath.Clean(dir)
	if clean == "/" || clean == "." || clean == "" || !strings.HasPrefix(clean, "/") {
		return fmt.Errorf("exec: %s: refusing to remove %q", s.Target, dir)
	}
	res, err := s.Run(ctx, Cmd{Path: "rm", Args: []string{"-rf", clean}})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exec: %s: rm %s: exit %d", s.Target, clean, res.ExitCode)
	}
	return nil
}

// Close tears down the multiplexed connection.
func (s *SSH) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	tmp := s.tmp
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	args := append(append([]string{}, s.Options...), "-O", "exit", s.Target)
	_, _ = s.local.Run(ctx, Cmd{Path: s.Binary, Args: args})

	if tmp != "" {
		_ = os.RemoveAll(tmp)
	}
	return s.local.Close()
}

func (s *SSH) check() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("exec: %s: runner is closed", s.Target)
	}
	return nil
}

// signal delivers sig to a remote pid.
func (s *SSH) signal(pid int, sig Signal) error {
	if pid <= 0 {
		return fmt.Errorf("exec: %s: no remote pid to signal", s.Target)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := s.Run(ctx, Cmd{Path: "kill", Args: []string{"-" + string(sig), strconv.Itoa(pid)}})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		// A pid that is already gone is not a failure to signal. It is the outcome
		// the caller wanted, arrived at without them.
		if strings.Contains(string(res.Stderr), "No such process") {
			return nil
		}
		return fmt.Errorf("exec: %s: kill -%s %d: exit %d: %s",
			s.Target, sig, pid, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

// sshProcess is a remote program, seen through one ssh session.
type sshProcess struct {
	ssh     *SSH
	session Process
	pid     int
	pidPath string

	once sync.Once
	res  Result
	err  error
}

func (p *sshProcess) Stdin() io.WriteCloser { return p.session.Stdin() }
func (p *sshProcess) Stdout() io.Reader     { return p.session.Stdout() }
func (p *sshProcess) Stderr() io.Reader     { return p.session.Stderr() }

// PID is the remote pid, or zero when it could not be read. Zero is meaningful: the
// process is running and reachable over its ports, but it cannot be sampled.
func (p *sshProcess) PID() int { return p.pid }

func (p *sshProcess) Signal(sig Signal) error { return p.ssh.signal(p.pid, sig) }

// Wait blocks until the remote program exits.
func (p *sshProcess) Wait() (Result, error) {
	p.once.Do(func() {
		p.res, p.err = p.session.Wait()
		// Local usage over a remote command describes the ssh client, and reporting it
		// as the subject's would put a sixty-second load pass at a few milliseconds of
		// CPU. The /proc sampler measures the right process on the right machine.
		p.res.Rusage = Rusage{}

		if p.res.ExitCode == SSHExitCode {
			// Genuinely ambiguous over this protocol, so it is named rather than
			// silently treated as one of the two.
			p.err = errors.Join(p.err, fmt.Errorf(
				"exec: %s: exit 255 — either the remote program exited 255 or the ssh "+
					"connection failed; %s", p.ssh.Target, strings.TrimSpace(string(p.res.Stderr))))
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = p.ssh.Run(ctx, Cmd{Path: "rm", Args: []string{"-f", p.pidPath}})
	})
	return p.res, p.err
}

// quote renders s as a single POSIX shell word.
//
// Single quotes, because inside them the shell interprets nothing at all — no variable
// expansion, no backslash escapes, no command substitution. The only character that
// needs handling is the closing quote itself, which is done by ending the quoted run,
// emitting an escaped quote, and starting a new one.
//
// This function is the entire trust boundary between the harness's argv slices and a
// protocol that carries a string, so it is deliberately the dumbest possible correct
// implementation rather than a clever one that skips quoting for "safe-looking" values.
func quote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sortedKeys orders the environment so the rendered command is stable. It reaches the
// archived argv, and a command line that reorders between two identical cells is a
// diff nobody can read.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
