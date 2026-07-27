package agent

import (
	"context"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/juancavallotti/octo-performance/harness/internal/exec"
)

// fakeSubject answers the remote sampler's scripts out of the same /proc fixture the
// local source is tested against.
//
// It runs the script with a real /bin/sh, against a fixture tree, so the shell quoting
// and the section framing are exercised rather than mocked. Only the paths are
// rewritten — which is the one thing a fixture cannot supply, because /proc is not
// relocatable on the machine running the test.
type fakeSubject struct {
	root  string
	local *exec.Local

	mu    sync.Mutex
	calls int
}

func newFakeSubject(t *testing.T) *fakeSubject {
	t.Helper()
	abs, err := filepath.Abs("testdata/proc")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSubject{root: abs, local: exec.NewLocal()}
	t.Cleanup(func() { _ = f.local.Close() })
	return f
}

func (f *fakeSubject) Name() string { return "fake-subject" }

func (f *fakeSubject) Run(ctx context.Context, c exec.Cmd) (exec.Result, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()

	args := append([]string{}, c.Args...)
	for i, a := range args {
		args[i] = strings.ReplaceAll(a, "/proc/", f.root+"/")
	}
	// getconf and hostname exist on the test machine; the fixture supplies the rest.
	return f.local.Run(ctx, exec.Cmd{Path: c.Path, Args: args})
}

func (f *fakeSubject) Start(context.Context, exec.Cmd) (exec.Process, error) { panic("unused") }
func (f *fakeSubject) Put(context.Context, string, fs.FileMode, io.Reader) error {
	panic("unused")
}
func (f *fakeSubject) Get(context.Context, string) (io.ReadCloser, error) { panic("unused") }
func (f *fakeSubject) MkdirAll(context.Context, string, fs.FileMode) error {
	panic("unused")
}
func (f *fakeSubject) RemoveAll(context.Context, string) error { return nil }
func (f *fakeSubject) Close() error                            { return nil }

func TestARemoteSampleMatchesALocalOneOverTheSameProc(t *testing.T) {
	// The invariant that makes a split topology comparable to a colocated one at all.
	// Two parsers of the same format agree until one of them is fixed, so the two
	// sources share sampleFrom and this asserts they still do.
	local := &ProcFS{Root: "testdata/proc"}
	remote := NewRemote(newFakeSubject(t))

	got, err := remote.Sample(1234)
	if err != nil {
		t.Fatal(err)
	}
	want, err := local.Sample(1234)
	if err != nil {
		t.Fatal(err)
	}

	// The page size is the subject's, and on this fixture both sides are the same
	// machine, so RSS must agree too.
	for _, c := range []struct {
		name      string
		got, want float64
	}{
		{"user seconds", got.UserSeconds, want.UserSeconds},
		{"sys seconds", got.SysSeconds, want.SysSeconds},
		{"rss bytes", float64(got.RSSBytes), float64(want.RSSBytes)},
		{"threads", float64(got.Threads), float64(want.Threads)},
		{"voluntary switches", float64(got.VoluntaryCtxSwitches), float64(want.VoluntaryCtxSwitches)},
		{"host busy", got.HostBusySeconds, want.HostBusySeconds},
		{"load1", got.Load1, want.Load1},
	} {
		if c.got != c.want {
			t.Errorf("%s: remote %v, local %v", c.name, c.got, c.want)
		}
	}
}

func TestARemoteSampleIsOneRoundTrip(t *testing.T) {
	// At 1 Hz on the machine whose spare capacity the whole experiment depends on, six
	// separate reads cost six round trips. This is the reason the script reads
	// everything at once, and it is the kind of property that decays silently.
	f := newFakeSubject(t)
	r := NewRemote(f)

	if _, err := r.Static(); err != nil {
		t.Fatal(err)
	}
	before := f.calls
	if _, err := r.Sample(1234); err != nil {
		t.Fatal(err)
	}
	if n := f.calls - before; n != 1 {
		t.Errorf("one sample cost %d round trips", n)
	}
}

func TestStaticIsReadOnceAndCached(t *testing.T) {
	// Nothing in it changes while a cell runs. Paying a round trip per sample for a
	// constant is the sampler competing with the thing it samples.
	f := newFakeSubject(t)
	r := NewRemote(f)

	for i := 0; i < 5; i++ {
		if _, err := r.Static(); err != nil {
			t.Fatal(err)
		}
	}
	if f.calls != 1 {
		t.Errorf("static was read %d times", f.calls)
	}
}

func TestRemoteStaticReadsTheSubjectsShapeNotTheHarnessMachines(t *testing.T) {
	st, err := NewRemote(newFakeSubject(t)).Static()
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's cpuinfo, not runtime.NumCPU(). Sampling a 16-core subject from a
	// 10-core laptop and reporting ten is how a CPU percentage becomes meaningless.
	if st.Cores == 0 {
		t.Error("no core count was read from the subject")
	}
	if st.CPUModel == "" {
		t.Error("no cpu model was read from the subject")
	}
	if st.PageSize <= 0 {
		t.Error("no page size was read from the subject")
	}
}

func TestASampleOfAProcessThatIsGoneFailsRatherThanReportingZero(t *testing.T) {
	// A sampler that returns a zero sample for a dead process produces a CPU series
	// that falls to zero and a steady window that looks perfectly flat.
	r := NewRemote(newFakeSubject(t))
	if _, err := r.Sample(999999); err == nil {
		t.Error("sampling a process that does not exist succeeded")
	}
}

func TestSectionsSplitsWhatTheScriptEmits(t *testing.T) {
	in := []byte("@@stat\n1234 (octo) S 1\n@@status\nVmRSS:\t100 kB\n@@fd\n42\n")
	got := sections(in)

	if s := string(got["stat"]); s != "1234 (octo) S 1\n" {
		t.Errorf("stat = %q", s)
	}
	if s := string(got["status"]); s != "VmRSS:\t100 kB\n" {
		t.Errorf("status = %q", s)
	}
	if s := text(got["fd"]); s != "42" {
		t.Errorf("fd = %q", s)
	}
}

func TestASectionThatProducedNothingIsPresentAndEmpty(t *testing.T) {
	// An unreadable /proc/pid/status must not shift the following section's content
	// into it. Everything after the first is corroboration, and corroboration that
	// silently holds another file's bytes is worse than none.
	got := sections([]byte("@@stat\n1 (x) S\n@@status\n@@fd\n7\n"))
	if len(got["status"]) != 0 {
		t.Errorf("an empty section holds %q", got["status"])
	}
	if text(got["fd"]) != "7" {
		t.Errorf("the following section was consumed: %q", got["fd"])
	}
}

// Remote must be usable anywhere a local source is, or the campaign procedure would
// have to know which machine it is sampling.
var _ Source = (*Remote)(nil)
