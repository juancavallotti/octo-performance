package exec

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tests drive the test binary itself as the program under test, so they depend on
// no platform utility and can ask for behaviour — a specific exit status, a process
// that ignores SIGTERM, a known allocation — that no stock binary offers.

const helperEnv = "OCTO_EXEC_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "" {
		helperMain(os.Args[1:])
		return
	}
	os.Exit(m.Run())
}

func helperMain(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "helper: no command")
		os.Exit(2)
	}
	switch args[0] {
	case "exit":
		n, _ := strconv.Atoi(args[1])
		os.Exit(n)

	case "say":
		fmt.Fprint(os.Stdout, args[1])
		fmt.Fprint(os.Stderr, args[2])

	case "flood":
		n, _ := strconv.Atoi(args[1])
		chunk := bytes.Repeat([]byte("x"), 4096)
		for written := 0; written < n; written += len(chunk) {
			os.Stdout.Write(chunk)
		}

	case "echo-env":
		fmt.Fprint(os.Stdout, os.Getenv(args[1]))

	case "pwd":
		d, _ := os.Getwd()
		fmt.Fprint(os.Stdout, d)

	case "cat":
		// Line-oriented echo, which is what the agent protocol looks like.
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			fmt.Fprintln(os.Stdout, "echo:"+sc.Text())
		}

	case "sleep":
		d, _ := time.ParseDuration(args[1])
		time.Sleep(d)

	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(30 * time.Second)

	case "graceful":
		// What a well-behaved subject does: stop when asked, exit zero.
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM)
		<-ch
		os.Exit(0)

	case "burn":
		// Fixed work rather than a wall-clock deadline. A deadline loop yields
		// whatever CPU the machine happened to spare, so on a loaded runner it
		// would fail this test for reasons that have nothing to do with rusage.
		n, _ := strconv.Atoi(args[1])
		buf := bytes.Repeat([]byte("octo"), 256)
		var sum [32]byte
		for i := 0; i < n; i++ {
			sum = sha256.Sum256(buf)
			buf[i%len(buf)] = sum[0]
		}
		fmt.Fprintf(os.Stdout, "%x", sum[:4])

	case "alloc":
		mib, _ := strconv.Atoi(args[1])
		buf := make([]byte, mib<<20)
		for i := range buf { // touch every page so the RSS is real
			buf[i] = byte(i)
		}
		fmt.Fprint(os.Stdout, len(buf))
		runtime.KeepAlive(buf)

	case "spawn":
		// Start a child that would outlive us, announce its pid, then block. Used to
		// prove that stopping a subject stops what the subject started.
		child := osCommandSelf("sleep", "60s")
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		fmt.Fprintln(os.Stdout, child.Process.Pid)
		time.Sleep(30 * time.Second)

	default:
		fmt.Fprintf(os.Stderr, "helper: unknown command %q\n", args[0])
		os.Exit(2)
	}
}

// osCommandSelf re-invokes the test binary as a helper, for the case where the helper
// itself needs a child.
func osCommandSelf(args ...string) *osexec.Cmd {
	c := osexec.Command(os.Args[0], args...)
	c.Env = append(os.Environ(), helperEnv+"=1")
	return c
}

func helper(args ...string) Cmd {
	return Cmd{
		Path: os.Args[0],
		Args: args,
		Env:  map[string]string{helperEnv: "1"},
	}
}

func TestRunReportsExitStatusAsDataNotAsAnError(t *testing.T) {
	// k6 exits 99 on a threshold breach. That is a fact about the run and the gates
	// need to see it; if the runner turned it into an error the orchestrator would
	// abandon a cell that produced perfectly good evidence of saturation.
	l := NewLocal()
	res, err := l.Run(t.Context(), helper("exit", "99"))
	if err != nil {
		t.Fatalf("a command that ran and exited 99 must not be an error: %v", err)
	}
	if res.ExitCode != 99 {
		t.Errorf("ExitCode = %d, want 99", res.ExitCode)
	}
	if res.Err() == nil {
		t.Error("Result.Err() must report the non-zero status for callers that require success")
	}
}

func TestRunErrorsOnlyWhenTheCommandCouldNotRun(t *testing.T) {
	l := NewLocal()
	_, err := l.Run(t.Context(), Cmd{Path: "/nonexistent/definitely-not-a-program"})
	if err == nil {
		t.Fatal("a program that does not exist must be an error, not exit status 127")
	}
}

func TestRunCapturesBothStreams(t *testing.T) {
	l := NewLocal()
	res, err := l.Run(t.Context(), helper("say", "on-stdout", "on-stderr"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Stdout); got != "on-stdout" {
		t.Errorf("Stdout = %q", got)
	}
	if got := string(res.Stderr); got != "on-stderr" {
		t.Errorf("Stderr = %q", got)
	}
	if res.Truncated {
		t.Error("Truncated set for output well under the limit")
	}
}

func TestCaptureIsBoundedAndSaysSo(t *testing.T) {
	l := NewLocal()
	c := helper("flood", "200000")
	c.CaptureLimit = 4096

	res, err := l.Run(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) != 4096 {
		t.Errorf("captured %d bytes, want the 4096 limit", len(res.Stdout))
	}
	if !res.Truncated {
		t.Error("truncation must be recorded; silently short output reads as a short run")
	}
}

func TestArgsAreArgvAndNotAShellString(t *testing.T) {
	// If anything ever interpolated Args into a shell, this argument would be
	// mangled or would execute. It must arrive at the process byte for byte.
	hostile := `a b; echo pwned > /tmp/x & $(whoami) "quoted" 'single' \backslash`
	l := NewLocal()
	res, err := l.Run(t.Context(), helper("say", hostile, ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Stdout); got != hostile {
		t.Errorf("argument was transformed on the way to the process:\n got %q\nwant %q", got, hostile)
	}
}

func TestEnvAddsToTheEnvironmentRatherThanReplacingIt(t *testing.T) {
	t.Setenv("OCTO_TEST_INHERITED", "inherited")

	l := NewLocal()
	c := helper("echo-env", "OCTO_TEST_INHERITED")
	c.Env["OCTO_TEST_EXTRA"] = "extra"

	res, err := l.Run(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Stdout); got != "inherited" {
		t.Errorf("inherited variable = %q, want %q; Env replaced the environment instead of adding to it", got, "inherited")
	}

	c = helper("echo-env", "OCTO_TEST_EXTRA")
	c.Env["OCTO_TEST_EXTRA"] = "extra"
	res, err = l.Run(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Stdout); got != "extra" {
		t.Errorf("added variable = %q, want %q", got, "extra")
	}
}

func TestDirSetsTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	l := NewLocal()
	c := helper("pwd")
	c.Dir = dir

	res, err := l.Run(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	// macOS resolves /var to /private/var, so compare the resolved forms.
	want, _ := filepath.EvalSymlinks(dir)
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(string(res.Stdout)))
	if got != want {
		t.Errorf("working directory = %q, want %q", got, want)
	}
}

func TestStartGivesPipesForTheStreamsTheCallerLeftNil(t *testing.T) {
	// This is the shape the agent protocol needs: write a line, read a line, on a
	// process that stays up.
	l := NewLocal()
	p, err := l.Start(t.Context(), helper("cat"))
	if err != nil {
		t.Fatal(err)
	}

	in, out := p.Stdin(), p.Stdout()
	if in == nil || out == nil {
		t.Fatal("Start must expose pipes for streams the Cmd left nil")
	}

	sc := bufio.NewScanner(out)
	for _, line := range []string{"hello", "world"} {
		if _, err := io.WriteString(in, line+"\n"); err != nil {
			t.Fatal(err)
		}
		if !sc.Scan() {
			t.Fatalf("no reply to %q", line)
		}
		if got, want := sc.Text(), "echo:"+line; got != want {
			t.Errorf("reply = %q, want %q", got, want)
		}
	}

	in.Close()
	for sc.Scan() { // drain to EOF before waiting, as os/exec requires
	}
	if _, err := p.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestStartHonoursAWriterInsteadOfMakingAPipe(t *testing.T) {
	// octo's log goes to a file, not into the orchestrator's heap.
	var buf bytes.Buffer
	c := helper("say", "to-the-writer", "")
	c.Stdout = &buf

	l := NewLocal()
	p, err := l.Start(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if p.Stdout() != nil {
		t.Error("Stdout() must be nil when the Cmd supplied a writer")
	}
	res, err := p.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if buf.String() != "to-the-writer" {
		t.Errorf("writer got %q", buf.String())
	}
	if len(res.Stdout) != 0 {
		t.Error("a redirected stream must not also be captured")
	}
}

func TestStopEscalatesToKillAndReportsThatItDid(t *testing.T) {
	l := NewLocal()
	p, err := l.Start(t.Context(), helper("ignore-term"))
	if err != nil {
		t.Fatal(err)
	}
	// Give the helper time to install its handler, otherwise the default action
	// would kill it and the escalation path would go untested.
	time.Sleep(300 * time.Millisecond)

	res, killed, err := Stop(p, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !killed {
		t.Fatal("a subject that ignores SIGTERM must be reported as killed, not as a clean stop")
	}
	if !res.Signaled || res.Signal != "killed" {
		t.Errorf("Signaled=%v Signal=%q, want a recorded SIGKILL", res.Signaled, res.Signal)
	}
}

func TestStopReturnsCleanlyWhenTheProcessExitsOnTime(t *testing.T) {
	l := NewLocal()
	p, err := l.Start(t.Context(), helper("graceful"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // let the handler install

	res, killed, err := Stop(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if killed {
		t.Error("a process that stopped on request must not be reported as killed")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d", res.ExitCode)
	}
}

func TestContextCancellationKillsTheProcessAndIsDistinguishableFromAnExit(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	l := NewLocal()
	p, err := l.Start(ctx, helper("sleep", "30s"))
	if err != nil {
		t.Fatal(err)
	}

	cancel()
	_, err = p.Wait()
	if err == nil {
		t.Fatal("a cell cut short by a deadline must not look like a subject that exited on its own")
	}
	if !strings.Contains(err.Error(), "context cancellation") {
		t.Errorf("error = %v, want it to name the cancellation", err)
	}
}

func TestStoppingASubjectStopsWhatTheSubjectStarted(t *testing.T) {
	// The old harness discovered octo's pid with pgrep against its own time(1)
	// wrapper and raced with it. Running each command in its own process group
	// removes the question: signalling the group reaches the children.
	l := NewLocal()
	p, err := l.Start(t.Context(), helper("spawn"))
	if err != nil {
		t.Fatal(err)
	}

	sc := bufio.NewScanner(p.Stdout())
	if !sc.Scan() {
		t.Fatal("helper did not announce its child")
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(sc.Text()))
	if err != nil {
		t.Fatalf("child pid: %v", err)
	}

	if _, _, err := Stop(p, 2*time.Second); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPID, 0); err != nil {
			return // gone, as required
		}
		time.Sleep(20 * time.Millisecond)
	}
	syscall.Kill(childPID, syscall.SIGKILL)
	t.Fatalf("child %d survived its parent being stopped", childPID)
}

func TestRusageIsWholeLifetimeCPU(t *testing.T) {
	l := NewLocal()
	res, err := l.Run(t.Context(), helper("burn", "200000"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Rusage.Available {
		t.Fatal("Rusage unavailable; cost per request has no denominator without it")
	}
	// 200k SHA-256 rounds is fixed work, so the processor time it costs does not
	// depend on how loaded the machine is. Anything near zero means the counter is
	// not being read at all.
	if cpu := res.Rusage.CPUSeconds(); cpu < 0.05 {
		t.Errorf("CPUSeconds = %v after 200k hash rounds; the counter is not being read", cpu)
	}
}

func TestMaxRSSIsPlausible(t *testing.T) {
	// ru_maxrss is kilobytes on Linux and bytes on Darwin. Getting that wrong scales
	// every memory number by 1024 and still looks like a number, so the only defence
	// is to allocate a known amount and check the order of magnitude.
	const allocMiB = 64

	l := NewLocal()
	res, err := l.Run(t.Context(), helper("alloc", strconv.Itoa(allocMiB)))
	if err != nil {
		t.Fatal(err)
	}
	got := res.Rusage.MaxRSSBytes
	const lo, hi = 32 << 20, 4 << 30
	if got < lo || got > hi {
		t.Errorf("MaxRSSBytes = %d (%.1f MiB) after allocating %d MiB; outside [%d, %d] — check the ru_maxrss unit for %s",
			got, float64(got)/(1<<20), allocMiB, lo, hi, runtime.GOOS)
	}
}

func TestPutIsAtomicAndSetsTheMode(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "nested", "config.yaml")

	l := NewLocal()
	if err := l.Put(t.Context(), dst, 0o600, strings.NewReader("flows: []\n")); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "flows: []\n" {
		t.Errorf("content = %q", b)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}

	// No staging files left behind: an interrupted campaign must not leave debris
	// that a later reader mistakes for an artifact.
	entries, _ := os.ReadDir(filepath.Dir(dst))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".put-") {
			t.Errorf("staging file %q survived", e.Name())
		}
	}
}

func TestGetReadsBackWhatPutWrote(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "argv.json")

	l := NewLocal()
	if err := l.Put(t.Context(), dst, 0o644, strings.NewReader(`["octo","run"]`)); err != nil {
		t.Fatal(err)
	}
	rc, err := l.Get(t.Context(), dst)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `["octo","run"]` {
		t.Errorf("read back %q", b)
	}
}

func TestMkdirAllAndRemoveAll(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "c")
	l := NewLocal()
	if err := l.MkdirAll(t.Context(), dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	if err := l.RemoveAll(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("directory survived RemoveAll: %v", err)
	}
}

func TestCmdStringIsForHumans(t *testing.T) {
	c := Cmd{Path: "/opt/octo", Args: []string{"run", "--config", "x.yaml"}}
	if got, want := c.String(), "/opt/octo run --config x.yaml"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
