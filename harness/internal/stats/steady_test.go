package stats

import (
	"math"
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

// quantised builds a rate series that can only take multiples of q, which is what
// differencing a tick counter actually produces. The pattern repeats rather than being
// drawn at random so the test says the same thing on every run.
func quantised(name string, n int, q float64, levels ...int) series.Series {
	s := series.Series{Name: name}
	for i := 0; i < n; i++ {
		s.Points = append(s.Points, series.Point{
			At: rampOrigin.Add(time.Duration(i) * time.Second),
			V:  float64(levels[i%len(levels)]) * q,
		})
	}
	return s
}

func TestACorroboratorTooCoarseToSeeATrendDoesNotVetoTheWindow(t *testing.T) {
	// The second half of the lesson above, and the one that cost a second CI failure.
	//
	// The first fix handled a corroborator with no answer at all. This is a
	// corroborator that answers — confidently, with a number — and the number is its
	// own granularity. Process CPU is counted in clock ticks, so a rate differenced
	// out of it moves in steps of one tick per sample: at 100 Hz sampled every 200 ms,
	// steps of 0.05 of a core. Against a process using an eighth of a core that step
	// is nearly 40% of the signal, and a least-squares line through ten such samples
	// was measured on Linux swinging from +1088 %/min to −2342 %/min — sign included —
	// across six runs of one unchanged workload whose throughput was constant to four
	// decimal places.
	//
	// So the veto has to be conditioned on resolution rather than on the fitted slope.
	// A test on the slope, or on its significance, is a coin toss: the same constant
	// signal produces a steep line on one draw and a flat one on the next, and a
	// detector built on it fails intermittently — which is worse than failing always,
	// because it gets retried until it passes.
	rps := steadySeries("k6.rps", 20, 400)

	// Four distinct levels around an eighth of a core, arranged so the fit through
	// them trends hard enough to breach the bound several times over.
	co := quantised("cpu", 20, 0.05, 1, 2, 1, 3, 2, 1, 2, 3, 3, 4)

	cfg := SteadyConfigFor(20 * time.Second)
	cfg.Corroborate = co
	cfg.CorroborateResolution = 0.05

	// It really would have vetoed: without the resolution, this is a failed window.
	blind := cfg
	blind.CorroborateResolution = 0
	if _, ok := DetectSteady(rps, blind); ok {
		t.Fatal("the fixture no longer breaches the trend bound, so it tests nothing")
	}

	w, ok := DetectSteady(rps, cfg)
	if !ok {
		t.Fatal("a window was rejected on a corroborator too coarse to have an opinion")
	}
	if w.Corroborated {
		t.Error("the window claims corroboration from an instrument that could not resolve one")
	}
	if w.CorroborationNote == "" {
		t.Error("the detector abstained without saying why; an instrument that cannot measure must say so in its own voice")
	}
}

func TestResolutionIsJudgedAgainstTheSignalNotTheSlope(t *testing.T) {
	// The guard must not become a blanket excuse. The same 0.05 quantum against a
	// subject burning two cores is 2.5% of the signal, not 40%, and there the veto is
	// exactly what the corroborator is for: a runtime still warming under real load.
	rps := steadySeries("k6.rps", 20, 400)

	climbing := series.Series{Name: "cpu"}
	for i := 0; i < 20; i++ {
		// Two cores, climbing — and still quantised to 0.05, as the real one is.
		climbing.Points = append(climbing.Points, series.Point{
			At: rampOrigin.Add(time.Duration(i) * time.Second),
			V:  math.Round((2.0+float64(i)*0.05)/0.05) * 0.05,
		})
	}

	cfg := SteadyConfigFor(20 * time.Second)
	cfg.Corroborate = climbing
	cfg.CorroborateResolution = 0.05
	if _, ok := DetectSteady(rps, cfg); ok {
		t.Error("a window was accepted while a well-resolved corroborator showed the subject still climbing")
	}

	// And flat at two cores is a corroboration, not an abstention.
	cfg.Corroborate = quantised("cpu", 20, 0.05, 40, 40, 41, 40, 39, 40)
	w, ok := DetectSteady(rps, cfg)
	if !ok {
		t.Fatal("a flat, well-resolved corroborator rejected the window")
	}
	if !w.Corroborated {
		t.Errorf("a corroborator resolving its signal to 2.5%% abstained: %s", w.CorroborationNote)
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
