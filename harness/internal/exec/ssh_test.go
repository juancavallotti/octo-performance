package exec

import (
	"strings"
	"testing"
)

func newTestSSH(t *testing.T) *SSH {
	t.Helper()
	s, err := NewSSH(SSHConfig{Target: "perf@subject"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAnSSHRunnerNeedsATarget(t *testing.T) {
	if _, err := NewSSH(SSHConfig{}); err == nil {
		t.Error("an ssh runner with no target was accepted")
	}
}

// quote is the entire trust boundary between the harness's argv slices and a protocol
// that carries only a string. Everything it is handed comes from a spec file, a scenario
// path or a rendered config, so the cases below are the ones a real campaign would
// eventually produce — not adversarial input, just ordinary input that a naive quoter
// gets wrong.
func TestQuoteProducesOneShellWordWhateverIsInIt(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain", "octo", `'octo'`},
		{"empty", "", `''`},
		{"space", "/opt/my perf/octo", `'/opt/my perf/octo'`},
		{"single quote", "it's", `'it'\''s'`},
		{"double quote", `say "hi"`, `'say "hi"'`},
		{"dollar", "$HOME/octo", `'$HOME/octo'`},
		{"backtick", "a`whoami`b", "'a`whoami`b'"},
		{"semicolon", "a; rm -rf /", `'a; rm -rf /'`},
		{"newline", "a\nb", "'a\nb'"},
		{"backslash", `a\b`, `'a\b'`},
		{"glob", "cells/*", `'cells/*'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := quote(tc.in); got != tc.want {
				t.Errorf("quote(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestQuotingSurvivesARoundTripThroughARealShell(t *testing.T) {
	// The table above asserts a shape. This asserts the property that actually matters:
	// whatever went in comes out of /bin/sh as exactly one unchanged argument.
	l := NewLocal()
	defer l.Close()

	for _, in := range []string{
		"octo", "/opt/my perf/octo", "it's", `say "hi"`, "$HOME", "a`whoami`b",
		"a; rm -rf /", `a\b`, "cells/*", "--config=/x/y z/integration.yaml",
		"PG_DSN=postgres://u:p@h:5432/db?sslmode=disable",
	} {
		res, err := l.Run(t.Context(), Cmd{
			Path: "/bin/sh",
			Args: []string{"-c", "printf %s " + quote(in)},
		})
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("%q: exit %d: %s", in, res.ExitCode, res.Stderr)
		}
		if got := string(res.Stdout); got != in {
			t.Errorf("round trip changed %q into %q", in, got)
		}
	}
}

func TestARemoteCommandChangesDirectoryBeforeItRuns(t *testing.T) {
	// Without the &&, a staging path that does not exist runs octo in the login
	// directory instead — where a relative config path resolves to a different file or
	// to none, and the failure arrives as "resource: not found" with no hint that the
	// working directory was wrong.
	got := newTestSSH(t).remoteCommand(Cmd{
		Path: "/opt/octo", Args: []string{"run", "--config", "octo/integration.yaml"},
		Dir: "/srv/perf/cells/001",
	})
	if !strings.HasPrefix(got, `cd '/srv/perf/cells/001' && `) {
		t.Errorf("command does not cd first: %s", got)
	}
	if !strings.Contains(got, "&&") {
		t.Error("a failed cd would not stop the command")
	}
}

func TestARemoteCommandReplacesTheShellSoTheSignalReachesTheProgram(t *testing.T) {
	// Without exec, the pid written by the far side is a shell's, and SIGTERM goes to
	// the shell rather than to octo. The shell exits, the harness believes it stopped
	// the subject, and the subject keeps holding port 8080 — which is exactly the
	// stale-process failure the port assertion exists to catch, caused by the harness.
	got := newTestSSH(t).remoteCommand(Cmd{Path: "/opt/octo", Args: []string{"run"}})
	if !strings.Contains(got, "exec '/opt/octo'") {
		t.Errorf("the remote shell is not replaced: %s", got)
	}
}

func TestARemoteCommandExecsEnvRatherThanAskingEnvForExec(t *testing.T) {
	// `env K=V exec prog` asks env to run a program called "exec". It is a shell
	// builtin, so no such binary exists and the command dies with 127 before the
	// program is ever reached — reporting the builtin's name, not the program's:
	//
	//   003-postgres-crud setup exited 127: env: 'exec': No such file or directory
	//
	// Found on the first split-topology campaign, on the first cell that needed a
	// dependency. Invisible everywhere else: only the deps path sets Env at all, and
	// exec.Local assigns cmd.Env directly rather than building a shell command, so
	// every local smoke run over all seven scenarios passed.
	got := newTestSSH(t).remoteCommand(Cmd{
		Path: "/srv/perf/scenarios/003-postgres-crud/setup.sh",
		Dir:  "/srv/perf/scenarios/003-postgres-crud",
		Env:  map[string]string{"PGPORT": "5432"},
	})
	if strings.Contains(got, "env") && !strings.Contains(got, "exec env") {
		t.Errorf("env is invoked without exec in front of it: %s", got)
	}
	if i, j := strings.Index(got, "exec "), strings.Index(got, "env "); i < 0 || j < 0 || i > j {
		t.Errorf("exec must precede env: %s", got)
	}
	// And the program still ends up as the last word, so env has something to run.
	if !strings.HasSuffix(got, `'/srv/perf/scenarios/003-postgres-crud/setup.sh'`) {
		t.Errorf("the program is not the command env execs: %s", got)
	}
}

func TestARemoteCommandCarriesTheEnvironmentInAStableOrder(t *testing.T) {
	// The rendered command reaches the archived argv. One that reorders between two
	// identical cells produces a diff nobody can read, and map iteration order in Go is
	// deliberately not stable.
	s := newTestSSH(t)
	c := Cmd{
		Path: "/opt/octo",
		Env: map[string]string{
			"PG_DSN":      "postgres://u:p@h/db?sslmode=disable",
			"BACKEND_URL": "http://10.0.0.9:9090",
			"HTTP_PORT":   "8080",
		},
	}
	first := s.remoteCommand(c)
	for i := 0; i < 20; i++ {
		if got := s.remoteCommand(c); got != first {
			t.Fatalf("the rendered command is not stable:\n%s\n%s", first, got)
		}
	}
	// The query string's ? and & must not reach the shell unquoted.
	if !strings.Contains(first, `'PG_DSN=postgres://u:p@h/db?sslmode=disable'`) {
		t.Errorf("the dsn was not quoted as one word: %s", first)
	}
	if strings.Index(first, "BACKEND_URL") > strings.Index(first, "HTTP_PORT") {
		t.Errorf("environment is not sorted: %s", first)
	}
}

func TestTheSSHInvocationMultiplexesOneConnection(t *testing.T) {
	// A fresh connection per command would put a full TCP handshake and key exchange in
	// front of every 1 Hz sample, which is the sampler becoming a measurable load on
	// the machine it is measuring.
	argv := strings.Join(newTestSSH(t).argv("uname -sr"), " ")
	for _, want := range []string{"ControlMaster=auto", "ControlPath=", "ControlPersist="} {
		if !strings.Contains(argv, want) {
			t.Errorf("the ssh invocation is missing %s: %s", want, argv)
		}
	}
	// A headless campaign must never sit on a password prompt at cell forty.
	if !strings.Contains(argv, "BatchMode=yes") {
		t.Errorf("batch mode is not set: %s", argv)
	}
	// The target has to come before the command, or ssh reads the command as a host.
	if i, j := strings.Index(argv, "perf@subject"), strings.Index(argv, "uname -sr"); i > j {
		t.Errorf("target and command are the wrong way round: %s", argv)
	}
}

func TestRemoveAllRefusesToTakeTheMachineWithIt(t *testing.T) {
	// This runs against a host nobody has open in a terminal. The blast radius of a
	// staging path that resolved to "/" is the campaign and probably the VM.
	s := newTestSSH(t)
	for _, dir := range []string{"/", "", ".", "relative/path", "/.."} {
		if err := s.RemoveAll(t.Context(), dir); err == nil {
			t.Errorf("RemoveAll(%q) was accepted", dir)
		}
	}
}

func TestAClosedRunnerRefusesWork(t *testing.T) {
	s := newTestSSH(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(t.Context(), Cmd{Path: "true"}); err == nil {
		t.Error("a closed runner ran a command")
	}
	if err := s.Close(); err != nil {
		t.Errorf("closing twice: %v", err)
	}
}

// The transport must satisfy the same interface as the local one, or the campaign
// procedure would need to know which machine it is talking to — which is the coupling
// the interface exists to prevent.
var _ Runner = (*SSH)(nil)
