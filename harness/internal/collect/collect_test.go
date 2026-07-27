package collect

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func at(base time.Time, sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }

func scrapesAt(base time.Time, secs ...int) Scrapes {
	s := Scrapes{Interval: "1s"}
	for _, sec := range secs {
		s.Samples = append(s.Samples, Scrape{
			At:   at(base, sec),
			Body: "octo_flow_messages_total{flow=\"page\",outcome=\"completed\"} " + itoa(sec*100) + "\n",
		})
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestBracketingPicksTheScrapesEnclosingTheWindow(t *testing.T) {
	// The window is detected after the fact, so it lands somewhere inside a run that
	// was scraped throughout. The right pair is the last scrape at or before the
	// start and the first at or after the end — anything else differences counters
	// over an interval that is not the one being reported.
	base := time.Now().Truncate(time.Second)
	s := scrapesAt(base, 0, 1, 2, 3, 4, 5, 6)

	b, err := s.Bracketing(at(base, 2).Add(500*time.Millisecond), at(base, 4).Add(500*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if !b.StartAt.Equal(at(base, 2)) {
		t.Errorf("start = %v, want the scrape at +2s", b.StartAt.Sub(base))
	}
	if !b.EndAt.Equal(at(base, 5)) {
		t.Errorf("end = %v, want the scrape at +5s", b.EndAt.Sub(base))
	}
	// The bracket is wider than the window, always, and by how much is recorded —
	// because a server-side rate is the delta over Span, not over the window it is
	// reported beside.
	if got, want := b.Span(), 3*time.Second; got != want {
		t.Errorf("Span = %v, want %v", got, want)
	}
	if got, want := b.Overhang(), time.Second; got != want {
		t.Errorf("Overhang = %v, want %v", got, want)
	}
}

func TestBracketingIsExactWhenTheWindowLandsOnScrapes(t *testing.T) {
	base := time.Now().Truncate(time.Second)
	s := scrapesAt(base, 0, 1, 2, 3, 4)

	b, err := s.Bracketing(at(base, 1), at(base, 3))
	if err != nil {
		t.Fatal(err)
	}
	if !b.StartAt.Equal(at(base, 1)) || !b.EndAt.Equal(at(base, 3)) {
		t.Errorf("bracket = [%v, %v], want exactly [+1s, +3s]", b.StartAt.Sub(base), b.EndAt.Sub(base))
	}
	if b.Overhang() != 0 {
		t.Errorf("Overhang = %v, want none when the window lands on scrapes", b.Overhang())
	}
}

func TestBracketingFailsRatherThanApproximating(t *testing.T) {
	// A pair that does not enclose the window yields a delta for some other
	// interval, and nothing downstream could tell that from a correct one.
	base := time.Now().Truncate(time.Second)

	t.Run("nothing before the start", func(t *testing.T) {
		s := scrapesAt(base, 5, 6, 7)
		_, err := s.Bracketing(at(base, 1), at(base, 6))
		if err == nil {
			t.Fatal("a window starting before the first scrape was bracketed")
		}
		if !strings.Contains(err.Error(), "at or before") {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("nothing after the end", func(t *testing.T) {
		s := scrapesAt(base, 0, 1, 2)
		_, err := s.Bracketing(at(base, 1), at(base, 9))
		if err == nil {
			t.Fatal("a window ending after the last scrape was bracketed")
		}
		if !strings.Contains(err.Error(), "at or after") {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("a bracket far wider than the window is measured, not hidden", func(t *testing.T) {
		// Scrapes ten seconds apart do enclose a one-second window, and the delta
		// between them is real — but it covers ten seconds of work, not one. The
		// pair is returned and the overhang says so, because dividing that delta by
		// the window would overstate the server's rate tenfold.
		s := scrapesAt(base, 0, 10)
		b, err := s.Bracketing(at(base, 2), at(base, 3))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := b.Span(), 10*time.Second; got != want {
			t.Errorf("Span = %v, want %v", got, want)
		}
		if got := b.OverhangRatio(); got < 8 {
			t.Errorf("OverhangRatio = %v, want the ninefold overhang to be visible", got)
		}
	})

	t.Run("too few scrapes", func(t *testing.T) {
		if _, err := (Scrapes{}).Bracketing(base, base); err == nil {
			t.Fatal("an empty collection bracketed a window")
		}
	})
}

func TestWindowParsesBothEndsAndReportsWhenTheyWereTaken(t *testing.T) {
	base := time.Now().Truncate(time.Second)
	s := scrapesAt(base, 0, 1, 2, 3)

	start, end, taken, err := s.Window(at(base, 1), at(base, 3))
	if err != nil {
		t.Fatal(err)
	}
	// The counter must have advanced between the two, and by the amount the fixture
	// says: this is what every server-side rate is derived from.
	from, ok := start.Total("octo_flow_messages_total", nil)
	if !ok {
		t.Fatal("opening scrape did not parse")
	}
	to, _ := end.Total("octo_flow_messages_total", nil)
	if to-from != 200 {
		t.Errorf("counter delta = %v, want 200", to-from)
	}
	// When the scrapes were taken is recorded, because it is not when the window
	// was: the delta covers slightly more than the window and a reader has to be
	// able to see by how much.
	if !taken.StartAt.Equal(at(base, 1)) || !taken.EndAt.Equal(at(base, 3)) {
		t.Errorf("taken = %v", taken)
	}
}

func TestScraperKeepsEveryScrape(t *testing.T) {
	n := 0
	p := NewPromScraper(func(context.Context) (string, error) {
		n++
		return "octo_ready 1\n", nil
	}, 5*time.Millisecond)

	if err := p.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	got := p.Stop()

	if len(got.Samples) < 4 {
		t.Fatalf("kept %d scrapes over 60ms at 5ms; two snapshots are not a time series", len(got.Samples))
	}
	for i := 1; i < len(got.Samples); i++ {
		if !got.Samples[i].At.After(got.Samples[i-1].At) {
			t.Fatalf("scrape %d is not after %d", i, i-1)
		}
	}
}

func TestScraperTakesOneImmediately(t *testing.T) {
	p := NewPromScraper(func(context.Context) (string, error) { return "octo_ready 1\n", nil }, time.Hour)
	if err := p.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := p.Stop(); len(got.Samples) != 1 {
		t.Errorf("got %d scrapes; a cell shorter than the interval must still have one", len(got.Samples))
	}
}

func TestScraperRecordsFailures(t *testing.T) {
	// An arm with no admin port produces these on every tick. The absence of
	// scrapes has to be explained by something in the artifact, not inferred.
	p := NewPromScraper(func(context.Context) (string, error) {
		return "", errors.New("connection refused")
	}, 5*time.Millisecond)

	if err := p.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	got := p.Stop()

	if got.Present() {
		t.Fatal("a scraper that only failed reported samples")
	}
	if len(got.Errors) == 0 {
		t.Fatal("failures must be recorded")
	}
	if len(got.Errors) > 16 {
		t.Errorf("kept %d errors; this is not a log file", len(got.Errors))
	}
}

func TestScraperRefusesToStartTwice(t *testing.T) {
	p := NewPromScraper(func(context.Context) (string, error) { return "", nil }, time.Hour)
	if err := p.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	if err := p.Start(t.Context()); err == nil {
		t.Error("starting a running scraper must fail")
	}
}

func TestStopIsSafeWithoutStart(t *testing.T) {
	if got := NewPromScraper(nil, time.Second).Stop(); got.Present() {
		t.Error("a scraper that never started reported samples")
	}
}
