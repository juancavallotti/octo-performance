package stats

import (
	"fmt"
	"math"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/series"
)

// Window is the interval a cell's numbers actually describe.
type Window struct {
	From, To time.Time
	// Discarded is how much of the load pass was cut off the front as warm-up.
	Discarded time.Duration
	// CV and SlopePctPerMin are the accepted window's own stability figures, kept so
	// a reader can see how flat "flat" was.
	CV             float64
	SlopePctPerMin float64
	// Corroborated records whether a second series was checked. A window accepted on
	// throughput alone is weaker evidence than one where the subject's CPU agreed.
	Corroborated bool
}

// Duration of the accepted window.
func (w Window) Duration() time.Duration { return w.To.Sub(w.From) }

// SteadyConfig tunes detection. Use [DefaultSteadyConfig].
type SteadyConfig struct {
	// Bucket is the bin width the input series is aggregated to before analysis.
	Bucket time.Duration
	// MinDuration is the shortest window that may be accepted. A window can always
	// be made flat by making it short enough, so this is what stops the search from
	// finding stability in the last two seconds of a pass.
	MinDuration time.Duration
	// MaxCV is the largest coefficient of variation a window may have.
	MaxCV float64
	// MaxSlopePctPerMin bounds the trend, as a percentage of the window's own mean
	// per minute. CV alone would accept a smooth ramp.
	MaxSlopePctPerMin float64
	// Corroborate is a second series that must also be flat over the same interval,
	// normally the subject's CPU *rate*. Pass a rate series, not a cumulative
	// counter: a counter always trends upward and would never be flat.
	//
	// It exists because throughput can plateau while the subject is still warming —
	// the offered rate caps what k6 can deliver, so a saturated server and a settled
	// one look identical from the client alone.
	Corroborate series.Series
	// CorroborateMaxSlopePctPerMin bounds the corroborating series' trend. Looser
	// than the primary bound by default: CPU is noisier than request counts.
	CorroborateMaxSlopePctPerMin float64
}

// DefaultSteadyConfig is what campaigns use for a 60 s measured pass.
func DefaultSteadyConfig() SteadyConfig {
	return SteadyConfig{
		Bucket:                       time.Second,
		MinDuration:                  30 * time.Second,
		MaxCV:                        0.05,
		MaxSlopePctPerMin:            2.0,
		CorroborateMaxSlopePctPerMin: 5.0,
	}
}

// DriftPctAcrossWindow is what MaxSlopePctPerMin is really trying to bound: how much
// the fitted trend moves the metric from one end of the measured window to the other,
// as a percentage of its own mean.
const DriftPctAcrossWindow = 2.0

// SteadyConfigFor scales detection to the length of the pass being measured.
//
// A slope expressed per minute, applied to a window shorter than a minute, is an
// extrapolation — and it tightens as the window shrinks. Two adjacent seconds at 400
// and 402 requests differ by half a percent, which is nothing; extrapolated to a minute
// it is a thirty percent trend, and a fixed two-percent-per-minute bound rejects it.
// The consequence is a detector that silently finds no window at all on any pass much
// shorter than sixty seconds, which is the wrong failure: it looks like an unsteady
// subject and is really an unstated assumption about duration.
//
// So the bound is stated as drift across the window and converted, which leaves a
// sixty-second pass at exactly the two percent per minute it always used.
func SteadyConfigFor(pass time.Duration) SteadyConfig {
	cfg := DefaultSteadyConfig()
	if pass <= 0 {
		return cfg
	}

	cfg.MinDuration = pass / 2
	if cfg.MinDuration > 30*time.Second {
		cfg.MinDuration = 30 * time.Second
	}
	if cfg.MinDuration < 2*time.Second {
		cfg.MinDuration = 2 * time.Second
	}

	// The shortest window detection may accept is what the bound has to hold over,
	// because that is the window most vulnerable to the extrapolation above.
	over := cfg.MinDuration.Seconds()
	if over < 1 {
		over = 1
	}
	cfg.MaxSlopePctPerMin = DriftPctAcrossWindow * 60 / over
	cfg.CorroborateMaxSlopePctPerMin = cfg.MaxSlopePctPerMin * 2.5
	return cfg
}

// DetectSteady finds the earliest suffix of a load pass that is flat enough to measure.
//
// It walks candidate start points forward, and accepts the first whose remaining span
// satisfies every criterion: long enough, low enough CV, small enough trend, and — when
// a corroborating series was supplied — flat there too. Taking the earliest rather than
// the flattest keeps as much of the pass as the data supports, so the measured window
// is not quietly shrunk to whatever looked calmest.
//
// Ok is false when no window qualifies. That is a finding for the caller to gate on,
// not a condition to paper over: there is deliberately no fallback to a fixed window,
// because falling back silently is how the previous harness measured the wrong
// interval for its entire life.
func DetectSteady(rps series.Series, cfg SteadyConfig) (Window, bool) {
	binned := rps
	if cfg.Bucket > 0 {
		binned = rps.Bucket(cfg.Bucket)
	}
	if binned.Len() < 3 {
		return Window{}, false
	}

	_, last, _ := binned.Span()

	for i := 0; i < binned.Len()-2; i++ {
		start := binned.Points[i].At
		if last.Sub(start) < cfg.MinDuration {
			// Every later candidate is shorter still.
			break
		}
		suffix := binned.Window(start, last)

		cv, ok := suffix.CV()
		if !ok || cv > cfg.MaxCV {
			continue
		}
		slopePct, ok := slopePctPerMin(suffix)
		if !ok || math.Abs(slopePct) > cfg.MaxSlopePctPerMin {
			continue
		}

		w := Window{
			From:           start,
			To:             last,
			Discarded:      start.Sub(binned.Points[0].At),
			CV:             cv,
			SlopePctPerMin: slopePct,
		}

		if !cfg.Corroborate.Empty() {
			co := cfg.Corroborate.Window(start, last)
			coSlope, ok := slopePctPerMin(co)
			switch {
			case !ok:
				// The corroborating series cannot answer: too few points to fit a
				// trend, or a mean of about zero, which is what a lightly loaded
				// process differentiated from a coarse clock actually looks like —
				// mostly quantisation steps around nothing.
				//
				// That is "cannot say", and it is not "not flat". Rejecting the
				// window here would be rejecting it for want of evidence rather
				// than on evidence, and the result would be a detector that finds
				// no window at all on any subject cheap enough not to register —
				// reported to the operator as an unsteady runtime.
				//
				// So the window is accepted with Corroborated left false, which is
				// a recorded state the cell carries and the report prints. The
				// weaker claim is made explicitly instead of a stronger one being
				// refused silently.
			case math.Abs(coSlope) > cfg.CorroborateMaxSlopePctPerMin:
				// Throughput has settled but the subject has not. Keep looking.
				continue
			default:
				w.Corroborated = true
			}
		}
		return w, true
	}
	return Window{}, false
}

// FixedWindow is the naive alternative: discard a fixed prefix and measure the rest.
//
// It is recorded alongside the detected window in every cell so the effect of
// detection can be audited across a whole campaign rather than taken on trust.
func FixedWindow(s series.Series, discard time.Duration) (Window, bool) {
	first, last, ok := s.Span()
	if !ok {
		return Window{}, false
	}
	from := first.Add(discard)
	if !from.Before(last) {
		return Window{}, false
	}
	w := Window{From: from, To: last, Discarded: discard}
	sub := s.Window(from, last)
	if cv, ok := sub.CV(); ok {
		w.CV = cv
	}
	if sp, ok := slopePctPerMin(sub); ok {
		w.SlopePctPerMin = sp
	}
	return w, true
}

// slopePctPerMin expresses a series' trend as a percentage of its own mean per minute,
// which makes the threshold comparable across metrics with different units and scales.
func slopePctPerMin(s series.Series) (float64, bool) {
	perSec, _, ok := s.Slope()
	if !ok {
		return 0, false
	}
	mean, ok := s.Mean()
	if !ok || math.Abs(mean) < 1e-12 {
		return 0, false
	}
	return perSec * 60 / math.Abs(mean) * 100, true
}

// Describe renders a window for a log line or a report caption.
func (w Window) String() string {
	s := fmt.Sprintf("%s window, %s discarded, CV %.3f, slope %+.2f%%/min",
		w.Duration().Round(time.Second), w.Discarded.Round(time.Second), w.CV, w.SlopePctPerMin)
	if w.Corroborated {
		s += ", corroborated"
	}
	return s
}
