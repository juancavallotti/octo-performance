package gate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/juancavallotti/octo-performance/internal/loadgen"
)

// The corpus is written in the old harness's summary.json shape. These loaders are
// deliberately local to the test: the corpus must never constrain the new result
// schema, and the new schema must never be able to silently reshape the corpus.
type legacySummary struct {
	Test        string  `json:"test"`
	LoadModel   string  `json:"loadModel"`
	OfferedRate float64 `json:"offeredRate"`
	Duration    float64 `json:"durationSeconds"`
	Metrics     struct {
		HTTPReqs struct {
			Count float64 `json:"count"`
			Rate  float64 `json:"rate"`
		} `json:"http_reqs"`
		HTTPReqFailed struct {
			Rate float64 `json:"rate"`
		} `json:"http_req_failed"`
		Dropped struct {
			Count float64 `json:"count"`
		} `json:"dropped_iterations"`
		VUsMax struct {
			Max float64 `json:"max"`
		} `json:"vus_max"`
	} `json:"metrics"`
}

func loadSpecimen(t *testing.T, name string) legacySummary {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "corpus", name, "summary.json"))
	if err != nil {
		t.Fatalf("read corpus specimen %s: %v", name, err)
	}
	var s legacySummary
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("parse corpus specimen %s: %v", name, err)
	}
	return s
}

// evidenceFor builds Evidence from a corpus specimen.
//
// PreAllocatedVUs is reconstructed rather than read, because the old harness did not
// record it — which is the whole of learning L2. The reconstruction uses the same
// Little's-law sizing the new harness applies, with scenario 001's declared
// EXPECTED_LATENCY_MS of 25 ms at the offered 16,000 req/s, and it lands on 1,600:
// exactly the vus_max the healthy runs report, which is what confirms the arithmetic.
func evidenceFor(t *testing.T, name string) Evidence {
	t.Helper()
	s := loadSpecimen(t, name)

	pool := loadgen.SizePool(int(s.OfferedRate), 25*time.Millisecond, 0)
	pool.ObservedMaxVUs = int(s.Metrics.VUsMax.Max)

	return Evidence{
		CellID:   name,
		Scenario: "001-template-page",
		Load: Load{
			Model:             s.LoadModel,
			OfferedRate:       s.OfferedRate,
			AchievedRPS:       s.Metrics.HTTPReqs.Rate,
			Requests:          int64(s.Metrics.HTTPReqs.Count),
			DroppedIterations: s.Metrics.Dropped.Count,
			FailedRate:        s.Metrics.HTTPReqFailed.Rate,
			PreAllocatedVUs:   pool.PreAllocatedVUs,
			MaxVUs:            pool.MaxVUs,
			ObservedMaxVUs:    pool.ObservedMaxVUs,
		},
	}
}

// corpusGates is the subset the corpus can actually exercise.
//
// The old harness captured no k6 time series, never detected a measurement window, and
// never measured the clock offset between machines — which is precisely why those gates
// exist. Evaluating them here would mean inventing the evidence, and a fixture that
// fabricates the thing under test proves nothing. They are covered by their own unit
// tests against synthetic series instead.
func corpusGates(cfg Config) []Gate {
	return []Gate{
		GeneratorSaturation{cfg},
		ErrorRate{cfg},
		RateGap{cfg},
		Identity{},
		Fingerprint{},
	}
}

func TestPoolSizingReproducesTheObservedFloor(t *testing.T) {
	// Little's law at scenario 001's parameters: 16000 x 0.025 x 4 headroom = 1600.
	p := loadgen.SizePool(16000, 25*time.Millisecond, 0)
	if p.PreAllocatedVUs != 1600 {
		t.Fatalf("preAllocatedVUs = %d, want 1600 — the floor the healthy runs sat at",
			p.PreAllocatedVUs)
	}
}

// The acceptance test for the entire gate suite. A gate suite that passes the
// collapsed specimen is broken whatever else it does.
func TestCorpusVerdicts(t *testing.T) {
	tests := []struct {
		specimen string
		want     Level
		wantGate string
		note     string
	}{
		{
			specimen: "clean",
			want:     Valid,
			note:     "15,999 req/s at 0.86 ms p95, nothing dropped, pool at its 1,600 floor",
		},
		{
			specimen: "collapsed",
			want:     Invalid,
			wantGate: "loadgen.saturation",
			note:     "the same binary a day later: 6,938 req/s, 7,113 VUs, 535,026 iterations dropped",
		},
		{
			specimen: "metricson",
			want:     Invalid,
			wantGate: "loadgen.saturation",
			note:     "9,897 req/s with a 933 ms client p95 against a 0.41 ms runtime mean",
		},
	}

	gates := corpusGates(Config{})
	for _, tc := range tests {
		t.Run(tc.specimen, func(t *testing.T) {
			e := evidenceFor(t, tc.specimen)
			v := Evaluate(gates, e, false)

			if v.Level != tc.want {
				t.Fatalf("%s\nverdict = %s, want %s\nfindings: %+v", tc.note, v.Level, tc.want, v.Findings)
			}
			if tc.wantGate != "" {
				var found bool
				for _, f := range v.Findings {
					if f.Gate == tc.wantGate && f.Level == Invalid {
						found = true
					}
				}
				if !found {
					t.Fatalf("expected %s to fire at invalid; got %+v", tc.wantGate, v.Findings)
				}
			}
			t.Logf("%s -> %s (%s)", tc.specimen, v.Level, tc.note)
			for _, f := range v.Findings {
				t.Logf("    [%s] %s: %s", f.Level, f.Gate, f.Summary)
			}
		})
	}
}

// The discriminator between the two specimens is the VU pool, and it is the only thing
// that differs in kind. This asserts the gate is reading that signal, not accidentally
// passing on the throughput number.
func TestTheDiscriminatorIsTheVUPool(t *testing.T) {
	clean := evidenceFor(t, "clean")
	collapsed := evidenceFor(t, "collapsed")

	if clean.Load.Growth() > 1.01 {
		t.Fatalf("healthy specimen shows pool growth %.2fx", clean.Load.Growth())
	}
	if collapsed.Load.Growth() < 4 {
		t.Fatalf("collapsed specimen shows pool growth %.2fx, expected over 4x", collapsed.Load.Growth())
	}

	g := GeneratorSaturation{Config{}}
	if f := g.Check(clean); len(f) != 0 {
		t.Fatalf("saturation gate fired on the healthy specimen: %+v", f)
	}
	if f := g.Check(collapsed); len(f) == 0 {
		t.Fatal("saturation gate silent on the collapsed specimen")
	}
}

// Even with the absolute thresholds set uselessly loose, the two cells compared against
// each other must still be rejected — which is why the peer check exists.
func TestPeerComparisonCatchesItWhenThresholdsAreTooLoose(t *testing.T) {
	clean := evidenceFor(t, "clean")
	collapsed := evidenceFor(t, "collapsed")

	collapsed.Peers = []Peer{{
		CellID:          clean.CellID,
		ObservedMaxVUs:  clean.Load.ObservedMaxVUs,
		PreAllocatedVUs: clean.Load.PreAllocatedVUs,
	}}

	loose := Config{
		PoolGrowthSuspect:    100,
		PoolGrowthInvalid:    100,
		RunnerCPUCeilingPct:  100,
		MaxDroppedIterations: 1e9,
		AchievedVsOfferedMin: 0.01,
	}
	v := Evaluate([]Gate{GeneratorSaturation{loose}}, collapsed, false)
	if v.Level != Invalid {
		t.Fatalf("peer comparison did not catch a 4.4x pool disagreement: %+v", v.Findings)
	}
}

// Observe mode still records everything; it only declines to escalate.
func TestObserveModeRecordsWithoutExcluding(t *testing.T) {
	collapsed := evidenceFor(t, "collapsed")
	v := Evaluate(corpusGates(Config{}), collapsed, true)

	if v.Level != Suspect {
		t.Fatalf("observe mode produced %s, want suspect", v.Level)
	}
	if v.Excluded() {
		t.Fatal("observe mode must not exclude a cell")
	}
	if len(v.Findings) == 0 {
		t.Fatal("observe mode must still record the findings")
	}
	for _, f := range v.Findings {
		if f.Level > Suspect {
			t.Fatalf("finding %q escalated past suspect in observe mode", f.Gate)
		}
		if f.Evidence == nil && f.Gate == "loadgen.saturation" {
			t.Fatal("evidence must be recorded so thresholds can be calibrated from it")
		}
	}
}
