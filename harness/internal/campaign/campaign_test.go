package campaign

import (
	"fmt"
	"net"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/agent"
	"github.com/juancavallotti/octo-performance/harness/internal/exec"
	"github.com/juancavallotti/octo-performance/harness/internal/fake"
	"github.com/juancavallotti/octo-performance/harness/internal/gate"
	"github.com/juancavallotti/octo-performance/harness/internal/loadgen"
	"github.com/juancavallotti/octo-performance/harness/internal/plan"
	"github.com/juancavallotti/octo-performance/harness/internal/result"
	"github.com/juancavallotti/octo-performance/harness/internal/series"
	"github.com/juancavallotti/octo-performance/harness/internal/spec"
)

// The whole cell procedure, against a real process, with a real load generator, in a
// few seconds. This is the test that makes it possible to change the orchestration
// without an eight-hour campaign to find out whether it still works — which is the
// single largest thing the old lab did not have.

func requireK6(t *testing.T) {
	t.Helper()
	if _, err := osexec.LookPath("k6"); err != nil {
		t.Skip("k6 is not installed: the cell procedure is NOT being verified end to end by this run")
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

func loadScenario(t *testing.T) *spec.Scenario {
	t.Helper()
	sc, err := spec.LoadScenario(filepath.Join("testdata", "scenarios", "900-page"))
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// harness builds a campaign runner pointed at a fake subject.
//
// caps decides which octo the fake impersonates: "metrics" is 0.5.0 and later, "none"
// is everything before it. Both are exercised, because half of what this lab compares
// has no admin port and a procedure that only works against the newer one would be
// useless for the comparison it exists to make.
func harness(t *testing.T, caps string, latency string, capacity int) (*Runner, plan.Cell, string) {
	t.Helper()
	requireK6(t)

	bin, err := fake.OctoBinary()
	if err != nil {
		t.Fatal(err)
	}

	sc := loadScenario(t)
	workload, admin := freePort(t), freePort(t)
	dir := t.TempDir()

	arm := spec.Arm{
		Name:   "fake-0.6.0",
		Binary: spec.BinaryRef{Path: bin},
		Config: spec.ConfigArm{Mode: spec.Baseline},
		Env: map[string]string{
			"FAKEOCTO_CAPS":     caps,
			"FAKEOCTO_VERSION":  "0.6.0",
			"FAKEOCTO_ADDR":     fmt.Sprintf("127.0.0.1:%d", workload),
			"FAKEOCTO_LATENCY":  latency,
			"FAKEOCTO_CAPACITY": fmt.Sprint(capacity),
		},
	}

	cell := plan.Cell{
		ID:       plan.CellID{Campaign: "local", Scenario: sc.ID, Arm: arm.Name, Rep: 1},
		Ordinal:  0,
		Scenario: sc,
		Arm:      arm,
		Load:     sc.Load,
	}

	// The environment has to reach the probe as well as the run: capabilities are
	// established by executing the artifact, and this artifact decides what it is
	// from its environment.
	for k, v := range arm.Env {
		t.Setenv(k, v)
	}

	r := New(Config{
		Hosts:          Hosts{Runner: exec.NewLocal(), Subject: exec.NewLocal()},
		Endpoints:      Endpoints{Host: "127.0.0.1", WorkloadPort: workload, AdminPort: admin},
		Dir:            dir,
		LoadGen:        loadgen.NewK6(exec.NewLocal(), ""),
		SubjectSource:  agent.LocalSource(),
		RunnerSource:   agent.LocalSource(),
		Gates:          gate.Default(gate.Config{}),
		ObserveOnly:    true,
		SampleInterval: 200 * time.Millisecond,
		StopGrace:      5 * time.Second,
		Log:            func(f string, a ...any) { t.Logf(f, a...) },
	})
	return r, cell, dir
}

func TestRunCellProducesADefensibleResult(t *testing.T) {
	r, cell, dir := harness(t, "metrics", "300us", 512)

	out, err := r.RunCell(t.Context(), cell, nil)
	if err != nil {
		t.Fatal(err)
	}

	// The artifact was asked what it accepts, and what ran is recorded verbatim.
	if !out.Binary.Observability || !out.Binary.Metrics {
		t.Errorf("capabilities = %+v", out.Binary)
	}
	if !strings.Contains(strings.Join(out.Argv, " "), "--metrics") {
		t.Errorf("argv = %v, want --metrics on a build that accepts it", out.Argv)
	}
	if len(out.Withheld) != 0 {
		t.Errorf("withheld = %v on a build that accepts everything", out.Withheld)
	}

	// Every path the subject was handed is absolute. It resolves them against its
	// own working directory, which is never the harness's, so a relative one means a
	// different file — and the runtime is started inside the cell directory, so it
	// breaks even when both are the same machine.
	for i, a := range out.Argv {
		if a == "--config" && i+1 < len(out.Argv) && !filepath.IsAbs(out.Argv[i+1]) {
			t.Errorf("--config %q is relative", out.Argv[i+1])
		}
	}

	// Readiness was detected, by a named method, and the admin port was confirmed
	// rather than assumed.
	if out.Ready.Method != "readyz" || !out.Ready.Confirmed {
		t.Errorf("ready = %+v", out.Ready)
	}

	// The process was asked what it is, not just the file on disk.
	if out.Identity.Source != "metrics" || !out.Identity.Agrees() {
		t.Errorf("identity = %+v", out.Identity)
	}

	// The window was chosen from the data, and the naive fixed-offset window is kept
	// beside it so detection can be audited.
	if !out.WindowOK {
		t.Fatalf("no steady window found: %s", out.WindowNote)
	}
	if out.Window.Duration() <= 0 {
		t.Errorf("window = %v", out.Window)
	}

	// Client and server latency, side by side. This is the comparison that would
	// have caught a published p95 of 933 ms against a server mean of 0.41 ms.
	h := out.Headline
	if h.ClientP95Ms <= 0 {
		t.Errorf("ClientP95Ms = %v", h.ClientP95Ms)
	}
	if !h.HasServerMean {
		t.Error("the runtime's own view of its latency is missing from a cell that scraped it")
	}
	if h.ServerMeanMs <= 0 || h.ServerMeanMs > h.ClientP95Ms {
		t.Errorf("server mean %vms against client p95 %vms — implausible", h.ServerMeanMs, h.ClientP95Ms)
	}

	// The generator kept up.
	if h.AchievedRatio < 0.9 {
		t.Errorf("achieved %.0f of an offered %.0f", h.AchievedRPS, h.OfferedRate)
	}
	if h.PreAllocatedVUs == 0 || h.ObservedMaxVUs == 0 {
		t.Error("the pool the harness allocated and what it grew to must both be recorded")
	}

	// The subject was sampled and its cost attributed per request.
	if !out.Subject.Present() {
		t.Error("the subject was never sampled")
	}
	if h.SubjectCPUPct <= 0 {
		t.Errorf("SubjectCPUPct = %v", h.SubjectCPUPct)
	}
	if h.SubjectCPUmsPerReq <= 0 {
		t.Errorf("SubjectCPUmsPerRequest = %v; cost per request has no denominator", h.SubjectCPUmsPerReq)
	}

	if out.Totals.Killed {
		t.Error("the subject had to be killed rather than stopping on request")
	}
	if !out.Totals.Rusage.Available {
		t.Error("whole-lifetime cost is missing")
	}

	// It is on disk, and it reads back.
	cellDir := filepath.Join(dir, "cells", out.Slug())
	back, err := result.ReadCell(cellDir)
	if err != nil {
		t.Fatal(err)
	}
	if back.Headline.AchievedRPS != out.Headline.AchievedRPS {
		t.Error("the cell did not survive the round trip")
	}
	for _, want := range []string{
		"cell.json", "verdict.json", "config.render.json", "octo/integration.yaml",
		"argv.json", "help.txt", "octo.log", "series.csv", "metrics.ndjson",
	} {
		if _, err := os.Stat(filepath.Join(cellDir, want)); err != nil {
			t.Errorf("missing artifact %s", want)
		}
	}

	// The baseline arm stripped its tunables rather than writing defaults, so a
	// changed default in a future runtime is caught rather than masked.
	config, err := os.ReadFile(filepath.Join(cellDir, "octo", "integration.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, knob := range []string{"workers:", "buffer:", "pool:"} {
		if strings.Contains(string(config), knob) {
			t.Errorf("the baseline config still declares %s", knob)
		}
	}
}

func TestRunCellWorksAgainstABuildWithNoAdminPort(t *testing.T) {
	// Every octo before 0.5.0. Half of the comparison this lab exists for, and a
	// procedure that quietly required the admin port would make it unrunnable.
	r, cell, dir := harness(t, "none", "300us", 512)

	out, err := r.RunCell(t.Context(), cell, nil)
	if err != nil {
		t.Fatal(err)
	}

	if out.Ready.Method != "route" {
		t.Errorf("Method = %q, want the route fallback", out.Ready.Method)
	}
	if out.Ready.Confirmed {
		t.Error("Confirmed set with no admin port to confirm")
	}
	if len(out.Withheld) == 0 {
		t.Error("flags the artifact could not accept must be recorded, not silently dropped")
	}

	// No server-side view is available, and that absence is stated rather than
	// filled in with a zero.
	if out.Server.Present {
		t.Error("server metrics reported for a build that serves none")
	}
	if out.Headline.HasServerMean {
		t.Error("HasServerMean set with nothing to have measured it")
	}
	if out.Headline.ServerMeanMs != 0 {
		t.Errorf("ServerMeanMs = %v out of nowhere", out.Headline.ServerMeanMs)
	}

	// The client-side result is still complete.
	if out.Headline.AchievedRPS <= 0 || out.Headline.ClientP95Ms <= 0 {
		t.Errorf("headline = %+v", out.Headline)
	}
	if _, err := result.ReadCell(filepath.Join(dir, "cells", out.Slug())); err != nil {
		t.Fatal(err)
	}
}

func TestRunCellRecordsTheCollapseRatherThanPublishingIt(t *testing.T) {
	// A subject that cannot serve the offered rate. Every number the cell produces
	// is real; what makes it usable is that the verdict says not to use it.
	r, cell, _ := harness(t, "metrics", "10ms", 2)

	out, err := r.RunCell(t.Context(), cell, nil)
	if err != nil {
		t.Fatal(err)
	}

	if out.Verdict.Level == gate.Valid {
		t.Fatalf("a cell that achieved %.0f of an offered %.0f was called valid",
			out.Headline.AchievedRPS, out.Headline.OfferedRate)
	}
	t.Logf("verdict: %s", out.Verdict)

	// The findings have to name what went wrong, with the observed value, or a
	// reader has a label instead of evidence.
	var named []string
	for _, f := range out.Verdict.Findings {
		named = append(named, f.Gate)
		if f.Summary == "" {
			t.Errorf("finding from %s has no summary", f.Gate)
		}
	}
	if len(named) == 0 {
		t.Fatal("no findings on a collapsed cell")
	}
	t.Logf("gates that fired: %v", named)

	// Gates ran in observe mode, so nothing escalated past Suspect — a badly chosen
	// threshold costs a re-read, never an afternoon on a machine you are paying for.
	if out.Verdict.Level > gate.Suspect {
		t.Errorf("verdict = %s under observe mode", out.Verdict.Level)
	}
}

func TestPeerCarriesWhatTheCrossCellCheckNeeds(t *testing.T) {
	c := &result.Cell{
		Scenario: "900-page", Arm: "0.6.0", Rep: 2,
		Headline: result.Headline{ObservedMaxVUs: 7113, PreAllocatedVUs: 1600},
	}
	c.Ready.Method = "readyz"

	p := Peer(c)
	if p.ObservedMaxVUs != 7113 || p.PreAllocatedVUs != 1600 {
		t.Errorf("peer = %+v; the pool numbers are what the cross-cell check compares", p)
	}
	if p.ReadyMethod != "readyz" {
		t.Errorf("ReadyMethod = %q; two arms established ready by different methods have not measured the same thing", p.ReadyMethod)
	}
}

func TestDefaultResolverRefusesAnArmThatNamesNothing(t *testing.T) {
	resolve := DefaultResolver("/opt/octo-versions")

	if _, err := resolve(spec.BinaryRef{}); err == nil {
		t.Fatal("an arm with no binary reference resolved to something")
	}
	got, err := resolve(spec.BinaryRef{Version: "0.6.0"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "/opt/octo-versions/octo-0.6.0"; got != want {
		t.Errorf("resolved to %q, want %q", got, want)
	}
	if got, _ := resolve(spec.BinaryRef{Path: "/usr/local/bin/octo"}); got != "/usr/local/bin/octo" {
		t.Errorf("an explicit path was rewritten to %q", got)
	}
}

func TestWholeSecondsDropsTheBucketsARunOnlyPartlyFilled(t *testing.T) {
	// k6 stamps observations with a whole second and a run starts and stops mid-
	// second, so the first and last bucket hold a fraction of a second's traffic. On
	// a four-second pass that is half the data and it makes a perfectly steady run
	// look like one that collapsed at both ends.
	base := time.Unix(1785200000, 0)
	s := series.Series{Name: "k6.rps", Points: []series.Point{
		{At: base, V: 142}, // partial: the run began mid-second
		{At: base.Add(1 * time.Second), V: 400},
		{At: base.Add(2 * time.Second), V: 401},
		{At: base.Add(3 * time.Second), V: 400},
		{At: base.Add(4 * time.Second), V: 58}, // partial: the run ended mid-second
	}}

	got := wholeSeconds(s)
	if got.Len() != 3 {
		t.Fatalf("kept %d points, want the 3 whole seconds", got.Len())
	}
	for _, p := range got.Points {
		if p.V < 300 {
			t.Errorf("a partial bucket survived: %v at %v", p.V, p.At.Sub(base))
		}
	}

	// Trimming must not turn a short pass into a confident claim about nothing.
	short := series.Series{Points: []series.Point{{At: base, V: 1}, {At: base.Add(time.Second), V: 2}}}
	if !wholeSeconds(short).Empty() {
		t.Error("a two-bucket pass produced whole seconds it cannot have had")
	}
}

func TestFlowNamesComeFromTheConfigNotTheScenarioId(t *testing.T) {
	// Scenario 001 is "001-template-page" and its flow is called "page". Stripping
	// the numeric prefix yields "template-page", which matches no metric — and a
	// histogram that matches nothing is indistinguishable from a runtime that served
	// no traffic, so the cell would have reported a server mean of zero and carried
	// on. It did, until this was noticed against the real binaries.
	got := flowNames([]byte(`
service:
  name: perf-template-page
flows:
  - name: page
    workers: 8
  - name: audit
`))
	if len(got) != 2 || got[0] != "page" || got[1] != "audit" {
		t.Errorf("flowNames = %v, want [page audit]", got)
	}
	if flowNames([]byte("not: yaml: at: all: [")) != nil {
		t.Error("an unparseable config must yield no flows rather than a guess")
	}
	if flowNames([]byte("service:\n  name: x\n")) != nil {
		t.Error("a config with no flows must yield none")
	}
}

func TestSubjectPathsAreAbsolute(t *testing.T) {
	// A subject resolves paths against its own working directory, which is never the
	// harness's. A relative --out therefore hands the runtime a path that means
	// something different on the other side — and because the runtime is started
	// with its working directory set to the cell, it breaks even when both are the
	// same machine. It failed only when someone passed a relative --out, which is
	// exactly the case a local run makes easy and a remote run makes certain.
	cfg := Config{Dir: filepath.Join("relative", "out")}
	cfg.withDefaults()

	if !filepath.IsAbs(cfg.Dir) {
		t.Errorf("Dir = %q, want an absolute path", cfg.Dir)
	}
	if cfg.SubjectDir != cfg.Dir {
		t.Errorf("SubjectDir = %q, want it to default to Dir on a colocated topology", cfg.SubjectDir)
	}

	// A remote subject stages somewhere on its own filesystem, and that path is not
	// derived from the runner's.
	remote := Config{Dir: "out", SubjectDir: "/var/tmp/perf"}
	remote.withDefaults()
	if remote.SubjectDir != "/var/tmp/perf" {
		t.Errorf("SubjectDir = %q, want it left alone when set", remote.SubjectDir)
	}
}
