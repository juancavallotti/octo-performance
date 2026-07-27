package subject

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/exec"
	"github.com/juancavallotti/octo-performance/harness/internal/fake"
)

// These tests drive a real process through the real Runner. The subject is fake, the
// mechanism is not: ports are bound, readiness is polled over HTTP, the process is
// signalled, and its whole-lifetime resource usage is read back. A double that skipped
// any of that would leave the parts most likely to be wrong untested.

func octoBin(t *testing.T) string {
	t.Helper()
	bin, err := fake.OctoBinary()
	if err != nil {
		t.Fatal(err)
	}
	return bin
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

func writeConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("flows:\n  - name: page\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProbeAsksTheArtifactAndCachesByDigest(t *testing.T) {
	bin := octoBin(t)
	r := exec.NewLocal()
	p := NewProber()

	// The same binary presenting itself two different ways. The digest is what
	// identifies an artifact, so the second probe is a cache hit and returns the
	// first answer — which is correct: it is the same file.
	t.Setenv("FAKEOCTO_CAPS", "metrics")
	t.Setenv("FAKEOCTO_VERSION", "0.6.0")

	c, err := p.Probe(t.Context(), r, bin)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Observability || !c.Metrics {
		t.Fatalf("caps = %+v, want an admin port and metrics", c)
	}
	if c.Version != "0.6.0" {
		t.Errorf("Version = %q", c.Version)
	}
	if len(c.SHA256) != 64 {
		t.Errorf("SHA256 = %q, want a full digest", c.SHA256)
	}
	if c.HelpSHA256 == "" || c.Help == "" {
		t.Error("the raw help must be archived so the inference can be re-checked")
	}

	c2, err := p.Probe(t.Context(), r, bin)
	if err != nil {
		t.Fatal(err)
	}
	if !c2.ProbedAt.Equal(c.ProbedAt) {
		t.Error("the second probe of an identical artifact re-ran instead of hitting the cache")
	}
}

func TestProbeDetectsABuildWithNoAdminPort(t *testing.T) {
	// This is every octo before 0.5.0, and it is half of the comparison the lab
	// exists to make. Nothing may assume the admin port is there.
	t.Setenv("FAKEOCTO_CAPS", "none")
	t.Setenv("FAKEOCTO_VERSION", "0.4.3")

	c, err := NewProber().Probe(t.Context(), exec.NewLocal(), octoBin(t))
	if err != nil {
		t.Fatal(err)
	}
	if c.Observability || c.Metrics {
		t.Errorf("caps = %+v, want neither on a pre-0.5.0 build", c)
	}
	if c.Version != "0.4.3" {
		t.Errorf("Version = %q", c.Version)
	}
}

func TestProbeRefusesAnArtifactThatCannotStateItsVersion(t *testing.T) {
	// The old lab published cells stamped vunknown-dev. A number that cannot be
	// attributed to an artifact is not a measurement of anything, so this fails at
	// the probe rather than at the report.
	t.Setenv("FAKEOCTO_VERSION", "not-a-version")

	_, err := NewProber().Probe(t.Context(), exec.NewLocal(), octoBin(t))
	if err == nil {
		t.Fatal("an artifact with an unparseable version was accepted")
	}
	if !strings.Contains(err.Error(), "does not state a version") {
		t.Errorf("error = %v", err)
	}
}

// startFake probes and starts a fake subject, returning a ready-to-await handle.
func startFake(t *testing.T, caps, version string, extraEnv map[string]string) *Handle {
	t.Helper()

	port, admin := freePort(t), freePort(t)
	env := map[string]string{
		"FAKEOCTO_CAPS":    caps,
		"FAKEOCTO_VERSION": version,
		"FAKEOCTO_ADDR":    fmt.Sprintf("127.0.0.1:%d", port),
	}
	for k, v := range extraEnv {
		env[k] = v
	}
	for k, v := range env {
		t.Setenv(k, v)
	}

	r := exec.NewLocal()
	c, err := NewProber().Probe(t.Context(), r, octoBin(t))
	if err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(t.TempDir(), "octo.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logFile.Close() })

	req := StartRequest{
		Caps:       c,
		ConfigPath: writeConfig(t),
		AdminAddr:  fmt.Sprintf(":%d", admin),
		Metrics:    true,
		Env:        env,
		Log:        logFile,
		Endpoints: Endpoints{
			Base:  fmt.Sprintf("http://127.0.0.1:%d", port),
			Admin: fmt.Sprintf("http://127.0.0.1:%d", admin),
		},
	}
	if !c.Observability {
		req.Endpoints.Admin = ""
	}

	h, err := Start(t.Context(), r, req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Stop(2 * time.Second) })
	return h
}

func TestAwaitReadyUsesTheAdminPortAndRecordsThatItDid(t *testing.T) {
	h := startFake(t, "metrics", "0.6.0", map[string]string{"FAKEOCTO_COLD_START": "250ms"})

	rd, err := h.AwaitReady(t.Context(), "/page", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if rd.Method != ByReadyz {
		t.Errorf("Method = %q, want %q", rd.Method, ByReadyz)
	}
	if !rd.Confirmed {
		t.Error("a detected admin port must be positively confirmed, not assumed")
	}
	// The runtime was told to take 250ms. A readiness probe that returned earlier
	// would be answering a different question than the one it claims to.
	if rd.ColdStart < 250*time.Millisecond {
		t.Errorf("ColdStart = %v, but the runtime does not answer /readyz for 250ms", rd.ColdStart)
	}
	if rd.ColdStart > 5*time.Second {
		t.Errorf("ColdStart = %v, implausibly long", rd.ColdStart)
	}
	if rd.Attempts < 2 {
		t.Errorf("Attempts = %d; readiness was not actually polled", rd.Attempts)
	}
}

func TestAwaitReadyFallsBackToTheRouteAndSaysSo(t *testing.T) {
	// A pre-0.5.0 build can only be probed on its workload route, which answers as
	// soon as the listener binds and says nothing about the flows behind it. Two
	// arms established as ready by different methods have not measured the same
	// interval, so the method travels with the number.
	h := startFake(t, "none", "0.4.3", map[string]string{"FAKEOCTO_COLD_START": "150ms"})

	rd, err := h.AwaitReady(t.Context(), "/page", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if rd.Method != ByRoute {
		t.Errorf("Method = %q, want %q", rd.Method, ByRoute)
	}
	if rd.Confirmed {
		t.Error("Confirmed must stay false when there was no admin port to confirm")
	}
}

func TestAwaitReadyRejectsAnAdminPortThatWasDetectedButDoesNotAnswer(t *testing.T) {
	// Detection is a grep of someone else's help text. Being wrong in the optimistic
	// direction is the dangerous direction: without confirmation this cell would run
	// to completion and publish a number.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ReadyzPath {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	h := &Handle{
		Caps:      Caps{Version: "0.6.0", Observability: true},
		Endpoints: Endpoints{Base: srv.URL, Admin: srv.URL},
		StartedAt: time.Now(),
		client:    &http.Client{Timeout: time.Second},
	}

	_, err := h.AwaitReady(t.Context(), "/page", 2*time.Second)
	if err == nil {
		t.Fatal("a cell whose admin port does not answer /healthz was allowed to proceed")
	}
	if !strings.Contains(err.Error(), HealthzPath) {
		t.Errorf("error = %v, want it to name the probe that failed", err)
	}
}

func TestAwaitReadyTimesOutWithTheReasonTheProbeFailed(t *testing.T) {
	h := &Handle{
		Caps:      Caps{Version: "0.6.0"},
		Endpoints: Endpoints{Base: "http://127.0.0.1:1"},
		StartedAt: time.Now(),
		client:    &http.Client{Timeout: 100 * time.Millisecond},
	}
	rd, err := h.AwaitReady(t.Context(), "/page", 300*time.Millisecond)
	if err == nil {
		t.Fatal("a subject that never came up was reported ready")
	}
	if rd.LastError == "" {
		t.Error("the reason the last probe failed must be kept")
	}
}

func TestIdentifyAsksTheRunningProcessNotTheFileOnDisk(t *testing.T) {
	h := startFake(t, "metrics", "0.6.0", nil)
	if _, err := h.AwaitReady(t.Context(), "/page", 10*time.Second); err != nil {
		t.Fatal(err)
	}

	id, err := h.Identify(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if id.Source != "metrics" {
		t.Errorf("Source = %q, want the running process to have been asked", id.Source)
	}
	if id.Version != "0.6.0" || id.Intended != "0.6.0" {
		t.Errorf("Identity = %+v", id)
	}
	if !id.Agrees() {
		t.Error("the process that was started disagreed with itself")
	}
}

func TestIdentifyFallsBackToTheArtifactWhenThereIsNoMetricsEndpoint(t *testing.T) {
	h := startFake(t, "none", "0.4.3", nil)
	if _, err := h.AwaitReady(t.Context(), "/page", 10*time.Second); err != nil {
		t.Fatal(err)
	}

	id, err := h.Identify(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if id.Source != "artifact" {
		t.Errorf("Source = %q; a build with no admin port cannot be asked", id.Source)
	}
	if id.Version != "" {
		t.Errorf("Version = %q, want empty: nothing asked the process", id.Version)
	}
	// An identity that could not be established must not read as a disagreement.
	if !id.Agrees() {
		t.Error("an unaskable arm was reported as disagreeing")
	}
}

func TestIdentityDisagreementIsDetectable(t *testing.T) {
	id := Identity{Version: "0.5.0", Intended: "0.6.0", Source: "metrics"}
	if id.Agrees() {
		t.Error("a process running a different build than the harness started was accepted")
	}
}

func TestStopIsGracefulAndReturnsWholeLifetimeCost(t *testing.T) {
	h := startFake(t, "metrics", "0.6.0", nil)
	if _, err := h.AwaitReady(t.Context(), "/page", 10*time.Second); err != nil {
		t.Fatal(err)
	}

	totals, err := h.Stop(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if totals.Killed {
		t.Error("the subject had to be killed; it should have stopped on SIGTERM")
	}
	if totals.ExitCode != 0 {
		t.Errorf("ExitCode = %d", totals.ExitCode)
	}
	if !totals.Rusage.Available {
		t.Fatal("no rusage; cost per request has no denominator")
	}
	if totals.Uptime <= 0 {
		t.Error("Uptime not recorded")
	}
}

func TestStartRefusesAnArtifactThatWasNeverProbed(t *testing.T) {
	_, err := Start(t.Context(), exec.NewLocal(), StartRequest{ConfigPath: "/tmp/x.yaml"})
	if err == nil {
		t.Fatal("started a subject without asking the artifact what it accepts")
	}
}

func TestAssertPortsFreeCatchesAStaleListener(t *testing.T) {
	// A stale process on the workload port answers 404 quickly, and a load pass
	// against it records excellent throughput for a runtime that is not running.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if err := AssertPortsFree(l.Addr().String()); err == nil {
		t.Fatal("an occupied port was accepted")
	}
	if err := AssertPortsFree(fmt.Sprintf("127.0.0.1:%d", freePort(t))); err != nil {
		t.Errorf("a free port was rejected: %v", err)
	}
}

func TestScrapeReturnsTheRuntimesOwnExposition(t *testing.T) {
	h := startFake(t, "metrics", "0.6.0", nil)
	if _, err := h.AwaitReady(t.Context(), "/page", 10*time.Second); err != nil {
		t.Fatal(err)
	}

	// Give the runtime something to have counted.
	for i := 0; i < 5; i++ {
		resp, err := http.Get(h.Endpoints.Base + "/page")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	body, err := h.Scrape(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"octo_build_info", "octo_flow_messages_total", "octo_flow_duration_seconds_bucket",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition is missing %s", want)
		}
	}
}

func TestScrapeIsAnErrorWhenThereIsNoAdminPort(t *testing.T) {
	h := &Handle{Caps: Caps{Version: "0.4.3"}}
	if _, err := h.Scrape(context.Background()); err == nil {
		t.Fatal("scraping a build with no admin port succeeded")
	}
}
