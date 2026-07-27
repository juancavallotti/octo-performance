// Package collect holds the runner-side collectors that run alongside a load pass.
//
// The rule they exist to enforce: sampling starts before the window opens and stops
// after it closes, and the window is chosen afterwards. A collector that starts when
// the measurement starts cannot answer whether the measurement was steady, which is
// how a fixed ten-second warm-up came to be trusted for the whole life of the old lab
// without anything ever checking it.
package collect

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/promx"
)

// Scrape is one Prometheus exposition, with the moment it was read.
type Scrape struct {
	At   time.Time `json:"at"`
	Body string    `json:"body"`
}

// Scrapes is everything a scraper collected over one cell.
//
// Every scrape is kept. The old lab wrote metrics-start.prom and metrics-end.prom
// bracketing the whole load pass, which was defensible only while the measured window
// was the whole load pass. Once the window is detected rather than slept through,
// those two files bracket the wrong interval and the counters differenced between them
// describe a period that includes the warm-up.
type Scrapes struct {
	Interval string   `json:"interval"`
	Samples  []Scrape `json:"samples"`
	Errors   []string `json:"errors,omitempty"`
}

// Present reports whether anything was collected.
func (s Scrapes) Present() bool { return len(s.Samples) > 0 }

// Bracket is the pair of scrapes enclosing a window, and by how much.
//
// The two are never the same interval. Scrapes land where the tick put them and the
// window is chosen afterwards, so the counters differenced across a bracket describe
// slightly more than the window asked for. Recording the difference is what keeps that
// from mattering: a server-side rate is the delta over Span, the interval it was
// actually measured across, and never over the window it is being reported beside.
type Bracket struct {
	Start, End Scrape    `json:"-"`
	From, To   time.Time `json:"-"`

	// StartAt and EndAt are when the bracketing scrapes were taken.
	StartAt time.Time `json:"startAt"`
	EndAt   time.Time `json:"endAt"`
	// WindowFrom and WindowTo are what was asked for.
	WindowFrom time.Time `json:"windowFrom"`
	WindowTo   time.Time `json:"windowTo"`
}

// Span is the interval the counters were actually differenced across.
func (b Bracket) Span() time.Duration { return b.EndAt.Sub(b.StartAt) }

// Window is the interval that was asked for.
func (b Bracket) Window() time.Duration { return b.WindowTo.Sub(b.WindowFrom) }

// Overhang is how much wider the bracket is than the window.
func (b Bracket) Overhang() time.Duration { return b.Span() - b.Window() }

// OverhangRatio is the overhang as a share of the window. At a one-second scrape
// interval over a thirty-second window it is under seven percent. Much above that and
// the delta is describing a materially different interval — most likely one that
// reaches back into the warm-up.
func (b Bracket) OverhangRatio() float64 {
	w := b.Window()
	if w <= 0 {
		return 0
	}
	return b.Overhang().Seconds() / w.Seconds()
}

func (b Bracket) String() string {
	return fmt.Sprintf("%s across %s for a %s window (+%.0f%%)",
		b.StartAt.Format("15:04:05.000"), b.Span().Round(time.Millisecond),
		b.Window().Round(time.Millisecond), b.OverhangRatio()*100)
}

// Bracketing returns the last scrape at or before from and the first at or after to.
//
// It fails rather than approximating. A pair that does not enclose the window produces
// a counter delta for some other interval, and nothing downstream could tell that from
// a correct one.
func (s Scrapes) Bracketing(from, to time.Time) (Bracket, error) {
	if len(s.Samples) < 2 {
		return Bracket{}, fmt.Errorf("collect: %d scrapes cannot bracket a window", len(s.Samples))
	}

	startIdx, endIdx := -1, -1
	for i, sc := range s.Samples {
		if !sc.At.After(from) {
			startIdx = i // the latest one still at or before the start
		}
		if endIdx < 0 && !sc.At.Before(to) {
			endIdx = i
		}
	}
	if startIdx < 0 {
		return Bracket{}, fmt.Errorf(
			"collect: no scrape at or before the window start (%s); the first was at %s",
			from.Format(time.RFC3339Nano), s.Samples[0].At.Format(time.RFC3339Nano))
	}
	if endIdx < 0 {
		return Bracket{}, fmt.Errorf(
			"collect: no scrape at or after the window end (%s); the last was at %s",
			to.Format(time.RFC3339Nano), s.Samples[len(s.Samples)-1].At.Format(time.RFC3339Nano))
	}
	if endIdx <= startIdx {
		return Bracket{}, fmt.Errorf(
			"collect: the window falls between two consecutive scrapes; nothing to difference")
	}

	a, b := s.Samples[startIdx], s.Samples[endIdx]
	return Bracket{
		Start: a, End: b,
		StartAt: a.At, EndAt: b.At,
		WindowFrom: from, WindowTo: to,
	}, nil
}

// Window parses the two scrapes bracketing an interval.
func (s Scrapes) Window(from, to time.Time) (start, end promx.Exposition, b Bracket, err error) {
	b, err = s.Bracketing(from, to)
	if err != nil {
		return nil, nil, b, err
	}
	start, err = promx.ParseBytes([]byte(b.Start.Body))
	if err != nil {
		return nil, nil, b, fmt.Errorf("collect: parsing the opening scrape: %w", err)
	}
	end, err = promx.ParseBytes([]byte(b.End.Body))
	if err != nil {
		return nil, nil, b, fmt.Errorf("collect: parsing the closing scrape: %w", err)
	}
	return start, end, b, nil
}

// PromScraper reads an exposition endpoint at a fixed interval.
type PromScraper struct {
	scrape   func(context.Context) (string, error)
	interval time.Duration

	mu      sync.Mutex
	samples []Scrape
	errs    []string

	cancel  context.CancelFunc
	stopped chan struct{}
	running bool
}

// NewPromScraper returns a scraper. An interval of zero means one second.
func NewPromScraper(scrape func(context.Context) (string, error), interval time.Duration) *PromScraper {
	if interval <= 0 {
		interval = time.Second
	}
	return &PromScraper{scrape: scrape, interval: interval}
}

// Name implements the collector contract.
func (*PromScraper) Name() string { return "prometheus" }

// Start begins scraping, taking one immediately so a short cell is never left with
// nothing to difference.
func (p *PromScraper) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return fmt.Errorf("collect: scraper already started")
	}
	p.running = true
	p.mu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.stopped = make(chan struct{})

	p.tick(ctx)
	go func() {
		defer close(p.stopped)
		t := time.NewTicker(p.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.tick(ctx)
			}
		}
	}()
	return nil
}

func (p *PromScraper) tick(ctx context.Context) {
	body, err := p.scrape(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		if len(p.errs) < 16 {
			p.errs = append(p.errs, err.Error())
		}
		return
	}
	p.samples = append(p.samples, Scrape{At: time.Now(), Body: body})
}

// Stop ends scraping and returns everything collected.
func (p *PromScraper) Stop() Scrapes {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return Scrapes{Interval: p.interval.String()}
	}
	p.running = false
	p.mu.Unlock()

	p.cancel()
	<-p.stopped

	p.mu.Lock()
	defer p.mu.Unlock()
	out := Scrapes{
		Interval: p.interval.String(),
		Samples:  append([]Scrape(nil), p.samples...),
	}
	if len(p.errs) > 0 {
		out.Errors = append([]string(nil), p.errs...)
	}
	return out
}
