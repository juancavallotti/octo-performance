package spec_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/juancavallotti/octo-performance/harness/internal/payload"
	"github.com/juancavallotti/octo-performance/harness/internal/spec"
)

// scenariosDir is the checked-in scenario tree, from this package's directory.
const scenariosDir = "../../../scenarios"

// TestEveryShippedScenarioLoads is the guard against the failure this repository
// actually had: a scenario that exists as a directory, is named in a campaign, and turns
// out not to be runnable — discovered at the point a campaign reaches it, hours in.
//
// It is a test rather than a lint because a scenario that parses is not the same as a
// scenario that is complete, and Validate is where "complete" is defined.
func TestEveryShippedScenarioLoads(t *testing.T) {
	entries, err := os.ReadDir(scenariosDir)
	if err != nil {
		t.Fatal(err)
	}

	loaded := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(scenariosDir, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "scenario.yaml")); err != nil {
			t.Errorf("%s has no scenario.yaml, so no campaign can name it", e.Name())
			continue
		}
		s, err := spec.LoadScenario(dir)
		if err != nil {
			t.Errorf("%s: %v", e.Name(), err)
			continue
		}
		loaded++

		// The integration is what actually runs. A scenario pointing at a file that
		// is not there fails at the point the subject is started, which is after the
		// cell has already cost a cooldown and a warm-up.
		if _, err := os.Stat(s.IntegrationPath()); err != nil {
			t.Errorf("%s: %v", e.Name(), err)
		}
		// Both sides of the load specification have to be whole, and the load is only
		// whole once merged — but a scenario that names neither a rate nor calibration
		// cannot be rescued by any campaign, so that much is checkable here.
		if s.Load.Rate == 0 && !s.Load.Calibrate {
			t.Errorf("%s: names neither a rate nor calibrate: true", e.Name())
		}
		if s.Load.Duration == 0 {
			t.Errorf("%s: names no duration", e.Name())
		}
		for _, path := range []string{s.Deps.Setup, s.Deps.Teardown} {
			if path == "" {
				continue
			}
			full := filepath.Join(dir, path)
			info, err := os.Stat(full)
			if err != nil {
				t.Errorf("%s: %v", e.Name(), err)
				continue
			}
			if info.Mode().Perm()&0o111 == 0 {
				t.Errorf("%s: %s is not executable", e.Name(), path)
			}
		}
	}

	// All seven, not "however many happened to parse". A campaign that silently
	// measures less than it claimed is how the old results index came to list 34 rows
	// against 40 directories.
	if loaded != 7 {
		t.Errorf("%d scenarios loaded, want 7", loaded)
	}
}

func TestEveryScenarioWithABodyCanBuildIt(t *testing.T) {
	// A generator that cannot produce what it was asked for is a specification error,
	// and it must surface here rather than as a subject that came up and received an
	// empty POST for sixty seconds.
	entries, err := os.ReadDir(scenariosDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s, err := spec.LoadScenario(filepath.Join(scenariosDir, e.Name()))
		if err != nil {
			continue // reported by TestEveryShippedScenarioLoads
		}
		if !s.Request.HasBody() {
			if s.Request.Verb() != "GET" {
				t.Errorf("%s: %s with no body", e.Name(), s.Request.Verb())
			}
			continue
		}
		body, err := payload.Build(s.Request.Payload)
		if err != nil {
			t.Errorf("%s: %v", e.Name(), err)
			continue
		}
		if body.Size == 0 && s.Request.Body == "" {
			t.Errorf("%s: declares a body and produced no bytes", e.Name())
		}
	}
}

// The pairing is a claim the two scenarios make about each other, so it is asserted
// against the specs rather than left in the prose of two README files.
func TestScenarios006And007OfferTheSameClientSizing(t *testing.T) {
	six, err := spec.LoadScenario(filepath.Join(scenariosDir, "006-json-transform"))
	if err != nil {
		t.Fatal(err)
	}
	seven, err := spec.LoadScenario(filepath.Join(scenariosDir, "007-csv-transform"))
	if err != nil {
		t.Fatal(err)
	}
	if six.Load.ExpectedLatency != seven.Load.ExpectedLatency {
		t.Errorf("the two scenarios size their VU pools differently (%s vs %s), "+
			"so they do not offer load through the same client",
			six.Load.ExpectedLatency, seven.Load.ExpectedLatency)
	}
	if six.Load.Duration != seven.Load.Duration {
		t.Errorf("different durations: %s vs %s", six.Load.Duration, seven.Load.Duration)
	}
}
