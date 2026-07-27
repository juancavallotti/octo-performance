package stats

import (
	"testing"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/series"
)

// steadySeries builds a flat series of n one-second points at v.
func steadySeries(name string, n int, v float64) series.Series {
	s := series.Series{Name: name}
	for i := 0; i < n; i++ {
		s.Points = append(s.Points, series.Point{At: rampOrigin.Add(time.Duration(i) * time.Second), V: v})
	}
	return s
}

func TestACorroboratorThatCannotAnswerDoesNotVetoTheWindow(t *testing.T) {
	// The distinction this whole package turns on: "cannot say" is not "not flat".
	//
	// A lightly loaded process differentiated from a 100 Hz clock produces a rate
	// series of mostly zeros — quantisation steps around nothing — and no trend can be
	// fitted to it. Treating that as a failed corroboration means the detector finds no
	// window at all on any subject cheap enough not to register, and reports it to the
	// operator as an unsteady runtime. That is the most misleading answer available:
	// it names the subject for a shortcoming of the instrument.
	//
	// Caught by CI on Linux and invisible on macOS, where there is no /proc and the
	// corroboration path is never taken at all.
	rps := steadySeries("k6.rps", 20, 400)
	cfg := SteadyConfigFor(20 * time.Second)

	for _, tc := range []struct {
		name string
		co   series.Series
	}{
		{"all zeros", steadySeries("cpu", 20, 0)},
		{"one point", series.Series{Name: "cpu", Points: []series.Point{{At: rampOrigin, V: 0.3}}}},
		{"empty after windowing", series.Series{Name: "cpu", Points: []series.Point{
			{At: rampOrigin.Add(-time.Hour), V: 0.3},
			{At: rampOrigin.Add(-time.Hour).Add(time.Second), V: 0.3},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := cfg
			c.Corroborate = tc.co
			w, ok := DetectSteady(rps, c)
			if !ok {
				t.Fatal("a window was rejected because the corroborator could not answer")
			}
			// And it must say so. An uncorroborated window is weaker evidence than a
			// corroborated one, and the difference has to survive into the report.
			if w.Corroborated {
				t.Error("the window claims corroboration it did not get")
			}
		})
	}
}

func TestACorroboratorThatCanAnswerStillVetoes(t *testing.T) {
	// The check has to keep working, or the fix above would have quietly deleted it.
	// Throughput plateaus because the offered rate caps it, so a subject still warming
	// looks identical to a settled one from the client alone — that is the entire
	// reason a second series is consulted.
	rps := steadySeries("k6.rps", 20, 400)
	cfg := SteadyConfigFor(20 * time.Second)

	// CPU climbing hard across the whole pass: the subject has not settled.
	climbing := series.Series{Name: "cpu"}
	for i := 0; i < 20; i++ {
		climbing.Points = append(climbing.Points, series.Point{
			At: rampOrigin.Add(time.Duration(i) * time.Second),
			V:  0.10 + float64(i)*0.05,
		})
	}
	cfg.Corroborate = climbing
	if _, ok := DetectSteady(rps, cfg); ok {
		t.Error("a window was accepted while the subject's CPU was still climbing")
	}

	// Flat and non-zero: corroborated, and it says so.
	cfg.Corroborate = steadySeries("cpu", 20, 0.42)
	w, ok := DetectSteady(rps, cfg)
	if !ok {
		t.Fatal("a flat corroborator rejected the window")
	}
	if !w.Corroborated {
		t.Error("a flat, measurable corroborator did not mark the window corroborated")
	}
}
