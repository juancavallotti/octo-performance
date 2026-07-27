package loadgen

import (
	"fmt"
	"net"
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/exec"
	"github.com/juancavallotti/octo-performance/harness/internal/fake"
	"github.com/juancavallotti/octo-performance/harness/internal/spec"
)

// These run the real k6 against a real fake runtime through the real driver. k6's
// output shape is not a stable API — the summary object and the CSV columns have both
// moved between releases — so a parser tested only against committed fixtures is
// tested against a snapshot of a contract nobody promised. This is what notices when
// it changes.

func requireK6(t *testing.T) {
	t.Helper()
	if _, err := osexec.LookPath("k6"); err != nil {
		t.Skip("k6 is not installed: the summary parser, the CSV aggregator and the FIFO " +
			"streaming path are NOT being verified against a real k6 by this run")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startSubject brings up a fake runtime and returns its base URL.
func startSubject(t *testing.T, latency string, capacity int) string {
	t.Helper()

	bin, err := fake.OctoBinary()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("flows:\n  - name: page\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	logFile, err := os.Create(filepath.Join(dir, "octo.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logFile.Close() })

	p, err := exec.NewLocal().Start(t.Context(), exec.Cmd{
		Path: bin,
		Args: []string{"run", "--config", cfg},
		Env: map[string]string{
			"FAKEOCTO_CAPS":     "none",
			"FAKEOCTO_ADDR":     fmt.Sprintf("127.0.0.1:%d", port),
			"FAKEOCTO_LATENCY":  latency,
			"FAKEOCTO_CAPACITY": fmt.Sprint(capacity),
		},
		Stdout: logFile,
		Stderr: logFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { exec.Stop(p, 2*time.Second) })

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/page")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the fake runtime never came up")
	return ""
}

func TestK6DrivesARealLoadPassAndStreamsItsSeries(t *testing.T) {
	requireK6(t)

	base := startSubject(t, "300us", 256)
	k := NewK6(exec.NewLocal(), "")

	const rate = 500
	req := Request{
		Phase:    Measured,
		URL:      base + "/page",
		Model:    spec.Open,
		Rate:     rate,
		Duration: 3 * time.Second,
		Pool:     SizePool(rate, 20*time.Millisecond, 0),
		OutDir:   filepath.Join(t.TempDir(), "k6"),
	}

	run, err := k.Run(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}

	if run.ExitCode != 0 {
		t.Errorf("ExitCode = %d", run.ExitCode)
	}
	if run.Model != spec.Open {
		t.Errorf("Model = %q; it is recorded at the source, never inferred", run.Model)
	}
	if run.OfferedRate != rate {
		t.Errorf("OfferedRate = %v, want %d", run.OfferedRate, rate)
	}

	// The generator kept up, so achieved should be within a few percent of offered.
	if got := run.Summary.AchievedRPS(); got < rate*0.9 || got > rate*1.1 {
		t.Errorf("AchievedRPS = %v against an offered %d", got, rate)
	}
	if run.Summary.Dropped.Present {
		t.Errorf("dropped %v iterations on a load the subject can serve", run.Summary.Dropped.Count)
	}

	// The pool the harness computed reached the script, and what it grew to came
	// back. In the old lab neither number existed in any file.
	if run.Pool.PreAllocatedVUs != req.Pool.PreAllocatedVUs {
		t.Errorf("PreAllocatedVUs = %d, want the %d the harness allocated",
			run.Pool.PreAllocatedVUs, req.Pool.PreAllocatedVUs)
	}
	if run.Pool.ObservedMaxVUs == 0 {
		t.Error("ObservedMaxVUs was never read back")
	}

	// The series exists, which the old lab's summary-only capture could not offer,
	// and it was folded down on the way rather than written out in full.
	rps, ok := run.Series.Get("k6.rps")
	if !ok {
		t.Fatal("no rps series")
	}
	if rps.Len() < 2 {
		t.Fatalf("rps series has %d points over a 3s pass", rps.Len())
	}
	if run.RawRows < int64(rate) {
		t.Errorf("RawRows = %d; the CSV stream produced almost nothing", run.RawRows)
	}
	if run.UnparsedRows != 1 {
		t.Errorf("UnparsedRows = %d, want exactly 1 (the header) — anything more means k6's CSV moved",
			run.UnparsedRows)
	}

	// Nothing wrote the per-observation rows to disk. That is the whole point: at
	// campaign rates they would be two gigabytes per cell.
	if _, err := os.Stat(filepath.Join(req.OutDir, "series.fifo")); !os.IsNotExist(err) {
		t.Error("the series pipe survived the run")
	}
	entries, _ := os.ReadDir(req.OutDir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		if fi, err := e.Info(); err == nil && fi.Size() > 4<<20 {
			t.Errorf("%s is %d bytes; the raw rows were kept", e.Name(), fi.Size())
		}
	}
	t.Logf("cell artifacts: %v", names)

	if run.K6Version == "" {
		t.Error("k6's version is part of the series' provenance and was not recorded")
	}
	if run.ScriptSHA256 != ScriptSHA256 {
		t.Error("the script digest was not recorded")
	}
}

func TestK6CapturesTheSignatureOfACollapsedRun(t *testing.T) {
	requireK6(t)

	// A subject that cannot serve anything like the offered rate, with a pool too
	// small to cover the resulting latency. This is the 2026-07-26 failure in
	// miniature, and every number a gate needs to reject it has to survive the trip.
	base := startSubject(t, "8ms", 4)
	k := NewK6(exec.NewLocal(), "")

	const rate = 3000
	run, err := k.Run(t.Context(), Request{
		Phase:    Measured,
		URL:      base + "/page",
		Model:    spec.Open,
		Rate:     rate,
		Duration: 3 * time.Second,
		Pool:     Pool{PreAllocatedVUs: 40, MaxVUs: 200},
		OutDir:   filepath.Join(t.TempDir(), "k6"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if !run.Summary.Dropped.Present || run.Summary.Dropped.Count == 0 {
		t.Error("no dropped iterations; the offered rate was not actually offered")
	}
	if got := run.Summary.AchievedRPS(); got > rate/2 {
		t.Errorf("AchievedRPS = %v; the subject was supposed to be overwhelmed", got)
	}
	if run.Pool.ObservedMaxVUs < run.Pool.MaxVUs {
		t.Errorf("ObservedMaxVUs = %d of a %d ceiling; the pool was supposed to run into it",
			run.Pool.ObservedMaxVUs, run.Pool.MaxVUs)
	}
	if got := run.Pool.Growth(); got < 2 {
		t.Errorf("Growth = %v, want the pool to have scaled well past its allocation", got)
	}

	// The run overran what it was asked for, because the executor could not finish
	// on time. That gap is evidence in its own right.
	if run.Summary.TestRunDuration <= 3*time.Second {
		t.Errorf("TestRunDuration = %v, want the overrun to be visible", run.Summary.TestRunDuration)
	}
}

func TestK6ReportsAMissingBinaryRatherThanAnEmptyResult(t *testing.T) {
	k := NewK6(exec.NewLocal(), "definitely-not-k6")
	_, err := k.Run(t.Context(), Request{
		URL: "http://127.0.0.1:1/page", Model: spec.Open, Rate: 10,
		Duration: time.Second, Pool: SizePool(10, time.Millisecond, 0),
		OutDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("a missing generator produced a result")
	}
}
