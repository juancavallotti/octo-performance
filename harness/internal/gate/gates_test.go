package gate

import (
	"strings"
	"testing"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/series"
	"github.com/juancavallotti/octo-performance/harness/internal/stats"
)

var base = time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

// healthy is a cell with nothing wrong with it, used as the starting point for tests
// that break exactly one thing.
func healthy() Evidence {
	return Evidence{
		CellID: "001__v0.6.0__rep1",
		Load: Load{
			Model: "open", OfferedRate: 16000, AchievedRPS: 15990,
			Requests: 959400, DroppedIterations: 0, FailedRate: 0,
			PreAllocatedVUs: 1600, MaxVUs: 16000, ObservedMaxVUs: 1600,
		},
		Runner:   Runner{Present: true, Cores: 16, CPUPctMean: 400, CPUPctPeak: 520},
		Subject:  Subject{Present: true, Cores: 8, CPUPctMean: 300},
		Server:   Server{Present: true, MessagesCompleted: 959400, ReadyThroughout: true, IdentityVersion: "0.6.0", IntendedVersion: "0.6.0"},
		Ready:    Ready{Method: "readyz", ColdStart: 412},
		Caps:     Caps{AdminPortDetected: true, AdminPortAnswered: true, MetricsRequested: true},
		Clock:    Clock{Measured: true, OffsetMs: -3.2, UncertaintyMs: 0.9, DriftMs: 1.1},
		Window:   stats.Window{From: base, To: base.Add(45 * time.Second), CV: 0.012, Corroborated: true},
		WindowOK: true,
	}
}

func levelOf(t *testing.T, e Evidence) Verdict {
	t.Helper()
	return Evaluate(Default(Config{}), e, false)
}

func TestHealthyCellIsValid(t *testing.T) {
	v := levelOf(t, healthy())
	if v.Level != Valid {
		t.Fatalf("verdict = %s, findings %+v", v.Level, v.Findings)
	}
	if len(v.Findings) != 0 {
		t.Fatalf("unexpected findings: %+v", v.Findings)
	}
}

func TestGatesEachBreakOneThing(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*Evidence)
		want     Level
		wantGate string
	}{
		{
			name: "runner machine saturated",
			mutate: func(e *Evidence) {
				e.Runner.CPUPctMean = 1400 // 87.5% of 16 cores
			},
			want: Invalid, wantGate: "loadgen.saturation",
		},
		{
			name: "generator hit its VU ceiling",
			mutate: func(e *Evidence) {
				e.Load.VUCap = 16000
				e.Load.ObservedMaxVUs = 16000
			},
			want: Invalid, wantGate: "loadgen.saturation",
		},
		{
			name: "mild pool growth is suspect, not fatal",
			mutate: func(e *Evidence) {
				e.Load.ObservedMaxVUs = 2400 // 1.5x
			},
			want: Suspect, wantGate: "loadgen.saturation",
		},
		{
			name: "client-visible errors",
			mutate: func(e *Evidence) {
				e.Load.FailedRate = 0.02
			},
			want: Invalid, wantGate: "loadgen.errors",
		},
		{
			// The case the client cannot see: work shed after the client was told it
			// had succeeded.
			name: "runtime shed work while the client saw none",
			mutate: func(e *Evidence) {
				e.Server.MessagesDropped = 4213
			},
			want: Invalid, wantGate: "loadgen.errors",
		},
		{
			name: "runtime went unready mid-window",
			mutate: func(e *Evidence) {
				e.Server.ReadyThroughout = false
			},
			want: Suspect, wantGate: "loadgen.errors",
		},
		{
			name: "dropped iterations",
			mutate: func(e *Evidence) {
				e.Load.DroppedIterations = 313010
			},
			want: Invalid, wantGate: "loadgen.saturated",
		},
		{
			name: "achieved falls short of offered",
			mutate: func(e *Evidence) {
				e.Load.AchievedRPS = 10781
			},
			want: Invalid, wantGate: "loadgen.saturated",
		},
		{
			name: "no steady window could be found",
			mutate: func(e *Evidence) {
				e.WindowOK = false
			},
			want: Invalid, wantGate: "window.steady-state",
		},
		{
			name: "window accepted without corroboration",
			mutate: func(e *Evidence) {
				e.Window.Corroborated = false
			},
			want: Suspect, wantGate: "window.steady-state",
		},
		{
			// A stale process on the port answers 404 quickly, which reads as
			// excellent throughput.
			name: "the process running is not the one started",
			mutate: func(e *Evidence) {
				e.Server.IdentityVersion = "0.4.3"
			},
			want: Invalid, wantGate: "subject.identity",
		},
		{
			name: "capability detection was optimistically wrong",
			mutate: func(e *Evidence) {
				e.Caps.AdminPortAnswered = false
			},
			want: Invalid, wantGate: "subject.identity",
		},
		{
			name: "clock offset was never measured",
			mutate: func(e *Evidence) {
				e.Clock.Measured = false
			},
			want: Suspect, wantGate: "env.clock",
		},
		{
			name: "clock drifted enough to reorder events",
			mutate: func(e *Evidence) {
				e.Clock.DriftMs = 180
			},
			want: Invalid, wantGate: "env.clock",
		},
		{
			name: "a sibling ran on a different machine",
			mutate: func(e *Evidence) {
				e.FingerprintHash = "sha256:aaa"
				e.Peers = []Peer{{CellID: "peer", FingerprintHash: "sha256:bbb"}}
			},
			want: Invalid, wantGate: "env.fingerprint",
		},
		{
			// A build with no admin port reaches ready at a later moment than one
			// answering /readyz, so the cold starts are not the same measurement.
			name: "siblings measured readiness differently",
			mutate: func(e *Evidence) {
				e.Peers = []Peer{{CellID: "peer", ReadyMethod: "route"}}
			},
			want: Suspect, wantGate: "env.fingerprint",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := healthy()
			tc.mutate(&e)
			v := levelOf(t, e)

			if v.Level != tc.want {
				t.Fatalf("verdict = %s, want %s; findings %+v", v.Level, tc.want, v.Findings)
			}
			var found bool
			for _, f := range v.Findings {
				if f.Gate == tc.wantGate && f.Level == tc.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected %s to fire at %s; got %+v", tc.wantGate, tc.want, v.Findings)
			}
		})
	}
}

// Throughput falling while the generator's CPU climbs is the feedback loop itself,
// and it is visible only in the correlation between two series.
func TestNegativeCorrelationBetweenThroughputAndRunnerCPU(t *testing.T) {
	e := healthy()
	var rps, cpu series.Series
	for i := 0; i < 30; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		rps.Points = append(rps.Points, series.Point{At: at, V: 16000 - float64(i)*300})
		cpu.Points = append(cpu.Points, series.Point{At: at, V: 400 + float64(i)*20})
	}
	e.Load.RPS = rps
	e.Runner.CPU = cpu

	v := Evaluate([]Gate{GeneratorSaturation{Config{}}}, e, false)
	if v.Level != Invalid {
		t.Fatalf("verdict = %s; a throughput/CPU anticorrelation is the feedback loop", v.Level)
	}
}

func TestStableSeriesDoesNotTripTheCorrelationCheck(t *testing.T) {
	e := healthy()
	var rps, cpu series.Series
	for i := 0; i < 30; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		v := 16000.0
		if i%2 == 0 {
			v = 15990
		}
		rps.Points = append(rps.Points, series.Point{At: at, V: v})
		cpu.Points = append(cpu.Points, series.Point{At: at, V: 400})
	}
	e.Load.RPS = rps
	e.Runner.CPU = cpu

	if v := Evaluate([]Gate{GeneratorSaturation{Config{}}}, e, false); v.Level != Valid {
		t.Fatalf("verdict = %s on a stable pass: %+v", v.Level, v.Findings)
	}
}

func TestVerdictOrdersFindingsWorstFirst(t *testing.T) {
	e := healthy()
	e.Load.DroppedIterations = 1000 // invalid
	e.Window.Corroborated = false   // suspect
	e.Load.ObservedMaxVUs = 2400    // suspect

	v := levelOf(t, e)
	if v.Findings[0].Level != Invalid {
		t.Fatalf("findings not ordered worst-first: %+v", v.Findings)
	}
	for i := 1; i < len(v.Findings); i++ {
		if v.Findings[i].Level > v.Findings[i-1].Level {
			t.Fatalf("findings out of order at %d: %+v", i, v.Findings)
		}
	}
}

func TestVerdictReasonsListOnlyTheDecidingFindings(t *testing.T) {
	e := healthy()
	e.Load.DroppedIterations = 1000
	e.Window.Corroborated = false

	v := levelOf(t, e)
	if len(v.Reasons()) != 1 {
		t.Fatalf("reasons = %v, want only the invalid one", v.Reasons())
	}
}

func TestLevelRoundTripsThroughText(t *testing.T) {
	for _, l := range []Level{Valid, Suspect, Invalid} {
		b, err := l.MarshalText()
		if err != nil {
			t.Fatal(err)
		}
		var got Level
		if err := got.UnmarshalText(b); err != nil {
			t.Fatal(err)
		}
		if got != l {
			t.Fatalf("%s round-tripped to %s", l, got)
		}
	}
	var l Level
	if err := l.UnmarshalText([]byte("sideways")); err == nil {
		t.Fatal("an unknown level must not decode")
	}
}

func TestExcludedOnlyForInvalid(t *testing.T) {
	if (Verdict{Level: Suspect}).Excluded() {
		t.Fatal("suspect cells stay in the aggregate")
	}
	if !(Verdict{Level: Invalid}).Excluded() {
		t.Fatal("invalid cells must be excluded")
	}
}

func TestThePeerPoolCheckOnlyComparesTheSameScenario(t *testing.T) {
	// Different workloads legitimately need different generator capacity: 001 offers a
	// bare GET at sub-millisecond latency, 005 holds every request for 70 ms. Comparing
	// across scenarios fires on every cell of every campaign, and a gate that always
	// fires is a gate nobody reads — which is worse than not having it, because the
	// incident it exists to catch then arrives inside the noise it generates.
	e := Evidence{
		Scenario: "001-template-page",
		Load:     Load{ObservedMaxVUs: 20, PreAllocatedVUs: 20, MaxVUs: 200, OfferedRate: 200, AchievedRPS: 200},
		Peers: []Peer{
			{CellID: "005__a__rep1", Scenario: "005-http-proxy", ObservedMaxVUs: 200},
		},
	}
	for _, f := range (GeneratorSaturation{}).Check(e) {
		if strings.Contains(f.Summary, "differs") {
			t.Errorf("a different scenario's pool was compared: %s", f.Summary)
		}
	}

	// The same scenario still fires: this is the 1,600-against-7,113 incident.
	e.Peers = []Peer{
		{CellID: "001__a__rep1", Scenario: "001-template-page", ObservedMaxVUs: 200},
	}
	found := false
	for _, f := range (GeneratorSaturation{}).Check(e) {
		if strings.Contains(f.Summary, "differs") {
			found = true
			if f.Level != Invalid {
				t.Errorf("a tenfold pool difference within one scenario is %s", f.Level)
			}
		}
	}
	if !found {
		t.Error("a tenfold pool difference within one scenario was not reported")
	}
}
