// Package report renders one self-contained HTML file from a finished campaign.
//
// It formats. It does not derive. Every number it prints was computed by
// [result.Rollup]; this package chooses layout, colour and wording, and the only
// arithmetic it performs is turning a value into a pixel position for a chart.
//
// That line matters because of where the old lab ended up: report.py reached a
// thousand lines, became a library three other scripts imported, and ended up as the
// only place several published numbers were calculated — which meant the numbers could
// not be checked without running the renderer, and could not be re-rendered without
// recalculating them.
//
// The output is one file. No stylesheet, no script tag, no font, no image request. A
// report that needs the network to render is a report that stops rendering, and these
// are meant to be readable in five years from a directory someone copied.
package report

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/juancavallotti/octo-performance/harness/internal/result"
)

//go:embed report.html.tmpl
var reportTemplate string

//go:embed report.css
var reportCSS string

// Render produces the report.
func Render(c *result.Campaign) ([]byte, error) {
	t, err := template.New("report").Funcs(funcs()).Parse(reportTemplate)
	if err != nil {
		return nil, fmt.Errorf("report: parsing the template: %w", err)
	}

	var buf bytes.Buffer
	data := struct {
		*result.Campaign
		CSS       template.CSS
		Generated string
	}{
		Campaign:  c,
		CSS:       template.CSS(reportCSS),
		Generated: c.EndedAt.Format("2006-01-02 15:04:05 MST"),
	}
	if err := t.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("report: rendering: %w", err)
	}
	return buf.Bytes(), nil
}

// SelfContained reports whether html reaches outside itself, and names what it found.
//
// Asserted in tests rather than trusted, because the failure is invisible at authoring
// time: a page with a broken external reference renders perfectly on the machine that
// made it and degrades silently everywhere else.
func SelfContained(html []byte) []string {
	var bad []string
	for _, probe := range []struct{ pattern, why string }{
		{"<script src", "an external script"},
		{"<link ", "an external stylesheet or resource"},
		{"http://", "an absolute http URL"},
		{"https://", "an absolute https URL"},
		{"url(", "a CSS url() reference"},
		{"@import", "a CSS import"},
		{"<img src=\"http", "a remote image"},
	} {
		if bytes.Contains(html, []byte(probe.pattern)) {
			bad = append(bad, fmt.Sprintf("%s (%s)", probe.pattern, probe.why))
		}
	}
	return bad
}

func funcs() template.FuncMap {
	return template.FuncMap{
		"num":      num,
		"pct":      func(f float64) string { return fmt.Sprintf("%+.1f%%", f) },
		"pctAbs":   func(f float64) string { return fmt.Sprintf("%.1f%%", f) },
		"dur":      durText,
		"spread":   spread,
		"deltaCls": deltaClass,
		"levelCls": levelClass,
		"strip":    strip,
		"pair":     pair,
		"short":    short,
		"metrics":  metricsOf,
		"lower":    strings.ToLower,
		"join":     func(xs []string) string { return strings.Join(xs, ", ") },
		"has":      func(xs []string) bool { return len(xs) > 0 },
		// Layout arithmetic only. Nothing here changes what a number is.
		"add": func(a, b float64) float64 { return a + b },
		"sub": func(a, b float64) float64 { return a - b },
	}
}

// num formats a measurement at a resolution the measurement can support.
//
// Six significant figures on a value known to two is a lie about precision, and the old
// index published throughput as "15999.221107831117".
func num(f float64) string {
	switch a := math.Abs(f); {
	case f == 0:
		return "0"
	case a >= 1000:
		return fmt.Sprintf("%.0f", f)
	case a >= 100:
		return fmt.Sprintf("%.1f", f)
	case a >= 1:
		return fmt.Sprintf("%.2f", f)
	case a >= 0.01:
		return fmt.Sprintf("%.3f", f)
	default:
		return fmt.Sprintf("%.4f", f)
	}
}

func durText(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
}

// spread renders an arm's dispersion. There is no code path that prints a median
// without it: a point estimate with no spread is the shape the old index published 61
// rows of.
func spread(m result.Metric) string {
	if !m.Present {
		return "—"
	}
	s := m.Summary
	if s.N == 1 {
		return "n=1"
	}
	return fmt.Sprintf("IQR %s · %s over %d reps", num(s.IQR), fmt.Sprintf("±%.1f%%", s.SpreadPct/2), s.N)
}

func deltaClass(label string) string {
	switch label {
	case "regression":
		return "bad"
	case "improvement":
		return "good"
	case "insufficient":
		return "unknown"
	default:
		return "noise"
	}
}

func levelClass(level string) string {
	switch level {
	case "invalid":
		return "bad"
	case "suspect":
		return "warn"
	default:
		return "good"
	}
}

// metricsOf lists an arm's metrics in the order the report shows them. Keeping the
// order here rather than in the template means the same order everywhere.
func metricsOf(a result.ArmRollup) []result.Metric {
	return []result.Metric{
		a.Throughput, a.ClientP50, a.ClientP95, a.ClientP99,
		a.ServerMean, a.CPUPerReq, a.RSSPeak, a.ColdStart,
	}
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// Point is one repetition's position in a strip plot.
type Point struct {
	X, Y  float64
	Value string
}

// Strip is a strip plot: every repetition as a dot, with the median marked.
//
// A box plot would hide n. These campaigns run five repetitions, and with five points
// the honest chart is the five points — a reader can see immediately whether a delta
// comes from a tight cluster or from two runs that disagree, which is exactly the
// judgement the old lab's point estimates removed.
type Strip struct {
	Present bool
	Width   float64
	Height  float64
	Rows    []StripRow
	Min     string
	Max     string
	Unit    string
	Label   string
}

// StripRow is one arm's dots.
type StripRow struct {
	Arm      string
	Y        float64
	Points   []Point
	MedianX  float64
	Median   string
	HasCI    bool
	CILo     float64
	CIHi     float64
	Excluded int
}

// strip lays out one metric across every arm of a scenario.
//
// The only arithmetic in this package: values to pixels. Nothing here changes what a
// number is, only where it is drawn.
func strip(s result.ScenarioRollup, which string) Strip {
	const (
		width     = 560.0
		rowHeight = 34.0
		leftPad   = 4.0
		rightPad  = 4.0
	)

	var (
		vals  []float64
		rows  []result.ArmRollup
		label string
		unit  string
	)
	for _, a := range s.Arms {
		m := pick(a, which)
		if !m.Present {
			continue
		}
		label, unit = m.Label, m.Unit
		vals = append(vals, m.Summary.Values...)
		rows = append(rows, a)
	}
	if len(vals) == 0 || len(rows) == 0 {
		return Strip{}
	}

	lo, hi := vals[0], vals[0]
	for _, v := range vals {
		lo = math.Min(lo, v)
		hi = math.Max(hi, v)
	}
	// A flat scale would put every dot on top of the others and read as agreement
	// when it is really an absence of range.
	if hi-lo < 1e-12 {
		pad := math.Max(math.Abs(hi)*0.05, 1e-9)
		lo, hi = lo-pad, hi+pad
	} else {
		pad := (hi - lo) * 0.12
		lo, hi = lo-pad, hi+pad
	}

	span := width - leftPad - rightPad
	x := func(v float64) float64 { return leftPad + (v-lo)/(hi-lo)*span }

	out := Strip{
		Present: true,
		Width:   width,
		Height:  float64(len(rows))*rowHeight + 8,
		Min:     num(lo),
		Max:     num(hi),
		Unit:    unit,
		Label:   label,
	}
	for i, a := range rows {
		m := pick(a, which)
		r := StripRow{
			Arm:      a.Arm,
			Y:        float64(i)*rowHeight + rowHeight/2,
			MedianX:  x(m.Summary.Median),
			Median:   num(m.Summary.Median),
			Excluded: a.Excluded,
		}
		for _, v := range m.Summary.Values {
			r.Points = append(r.Points, Point{X: x(v), Y: r.Y, Value: num(v)})
		}
		if m.Summary.HasCI {
			r.HasCI = true
			r.CILo, r.CIHi = x(m.Summary.CI.Lo), x(m.Summary.CI.Hi)
		}
		out.Rows = append(out.Rows, r)
	}
	return out
}

func pick(a result.ArmRollup, which string) result.Metric {
	switch which {
	case "throughput":
		return a.Throughput
	case "p50":
		return a.ClientP50
	case "p95":
		return a.ClientP95
	case "p99":
		return a.ClientP99
	case "server":
		return a.ServerMean
	case "cpu":
		return a.CPUPerReq
	case "rss":
		return a.RSSPeak
	case "coldstart":
		return a.ColdStart
	}
	return result.Metric{}
}

// LatencyPair is one arm's client and server latency, side by side.
//
// This comparison is the reason the report exists in this shape. The old lab published
// a client p95 of 933 ms as the runtime's latency while the runtime's own mean flow
// duration, sitting in the same result directory, was 0.41 ms. Neither number was
// wrong; showing only one of them was.
type LatencyPair struct {
	Arm         string
	Client      string
	Server      string
	HasServer   bool
	Absent      string
	GapPct      float64
	ClientWidth float64
	ServerWidth float64
	Ratio       string
}

// pair lays out the client-against-server comparison for one scenario.
func pair(s result.ScenarioRollup) []LatencyPair {
	const width = 320.0

	max := 0.0
	for _, a := range s.Arms {
		if a.ClientP95.Present {
			max = math.Max(max, a.ClientP95.Summary.Median)
		}
		if a.ServerMean.Present {
			max = math.Max(max, a.ServerMean.Summary.Median)
		}
	}
	if max <= 0 {
		return nil
	}

	var out []LatencyPair
	for _, a := range s.Arms {
		if !a.ClientP95.Present {
			continue
		}
		client := a.ClientP95.Summary.Median
		p := LatencyPair{
			Arm:         a.Arm,
			Client:      num(client),
			ClientWidth: client / max * width,
		}
		if a.ServerMean.Present {
			server := a.ServerMean.Summary.Median
			p.HasServer = true
			p.Server = num(server)
			p.ServerWidth = math.Max(server/max*width, 1)
			if server > 0 {
				p.Ratio = fmt.Sprintf("%.0fx", client/server)
				p.GapPct = (client - server) / client * 100
			}
		} else {
			p.Absent = a.ServerMean.Absent
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Arm < out[j].Arm })
	return out
}
