package promx

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Labels is one series' label set.
type Labels map[string]string

// Matches reports whether every label in want is present with the same value.
// An empty want matches everything.
func (l Labels) Matches(want Labels) bool {
	for k, v := range want {
		if l[k] != v {
			return false
		}
	}
	return true
}

// Key is a stable identity for a series, so two scrapes can be lined up.
// Labels named in ignore are excluded — "le" when comparing histogram buckets.
func (l Labels) Key(ignore ...string) string {
	skip := make(map[string]bool, len(ignore))
	for _, k := range ignore {
		skip[k] = true
	}
	parts := make([]string, 0, len(l))
	for k, v := range l {
		if !skip[k] {
			parts = append(parts, k+"="+v)
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// Sample is one series' labels and value at one scrape.
type Sample struct {
	Labels Labels
	Value  float64
}

// Exposition is a parsed scrape: metric name to its series.
type Exposition map[string][]Sample

// Parse reads exposition text.
//
// Comments and HELP/TYPE lines are skipped. A line whose value will not parse as a
// float is skipped rather than failing the scrape: one malformed series should not
// discard the other 175 that were fine. NaN is not data and is dropped; infinities
// are legitimate exposition values and are kept.
func Parse(r io.Reader) (Exposition, error) {
	out := Exposition{}
	sc := bufio.NewScanner(r)
	// Exposition lines are short, but a label set with many block addresses is not.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Split the value off the end. A label value may contain spaces, so this
		// cannot split on the first separator.
		idx := strings.LastIndexByte(line, ' ')
		if idx < 0 {
			continue
		}
		value, ok := parseValue(line[idx+1:])
		if !ok {
			continue
		}
		head := strings.TrimSpace(line[:idx])

		name, labels := head, Labels{}
		if strings.HasSuffix(head, "}") {
			if open := strings.IndexByte(head, '{'); open >= 0 {
				name = strings.TrimSpace(head[:open])
				labels = parseLabels(head[open+1 : len(head)-1])
			}
		}
		if name == "" {
			continue
		}
		out[name] = append(out[name], Sample{Labels: labels, Value: value})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("promx: reading exposition: %w", err)
	}
	return out, nil
}

// ParseBytes is [Parse] over a byte slice.
func ParseBytes(b []byte) (Exposition, error) { return Parse(strings.NewReader(string(b))) }

// parseValue accepts what exposition format allows. NaN is rejected: it means the
// runtime had nothing to report, which is not the same as a measurement.
func parseValue(raw string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(v) {
		return 0, false
	}
	return v, true
}

// parseLabels reads `a="1",b="x,y"` into a label set.
//
// It splits on commas outside quotes, because a block address is a legitimate label
// value and --metrics-blocks takes a comma-separated list of them. A parser that
// split naively would silently mangle exactly the metrics that only appear when
// block-level telemetry is on.
func parseLabels(text string) Labels {
	out := Labels{}
	var key, buf strings.Builder
	readingKey, inQuotes, escaped := true, false, false

	flush := func() {
		if k := strings.TrimSpace(key.String()); k != "" {
			out[k] = buf.String()
		}
		key.Reset()
		buf.Reset()
		readingKey = true
	}

	for _, ch := range text {
		if readingKey {
			switch ch {
			case '=':
				key.WriteString(buf.String())
				buf.Reset()
				readingKey = false
			case ',':
				buf.Reset()
			default:
				buf.WriteRune(ch)
			}
			continue
		}
		if escaped {
			// Exposition escapes \\, \" and \n inside label values.
			switch ch {
			case 'n':
				buf.WriteRune('\n')
			default:
				buf.WriteRune(ch)
			}
			escaped = false
			continue
		}
		switch {
		case ch == '\\' && inQuotes:
			escaped = true
		case ch == '"':
			inQuotes = !inQuotes
		case ch == ',' && !inQuotes:
			flush()
		default:
			buf.WriteRune(ch)
		}
	}
	if !readingKey {
		flush()
	}
	return out
}

// First returns the value of a metric's first series. Most metrics the harness reads
// this way are single-series gauges: octo_ready, go_goroutines, process_open_fds.
func (e Exposition) First(name string) (float64, bool) {
	s := e[name]
	if len(s) == 0 {
		return 0, false
	}
	return s[0].Value, true
}

// LabelsOf returns the label set of a metric's first series. This is how
// octo_build_info is read — the labels are the payload, the value is always 1.
func (e Exposition) LabelsOf(name string) Labels {
	s := e[name]
	if len(s) == 0 {
		return Labels{}
	}
	out := make(Labels, len(s[0].Labels))
	for k, v := range s[0].Labels {
		out[k] = v
	}
	return out
}

// Total sums a metric's series, optionally filtered by exact label values.
//
// Ok is false when the metric is absent or when no series matched, which is stricter
// than reporting zero. For a counter, an absent label combination usually does mean
// the event never happened — but it equally means a mistyped label name, and this
// package will not guess which. Callers that know the semantics say so explicitly at
// the call site.
func (e Exposition) Total(name string, match Labels) (float64, bool) {
	samples, present := e[name]
	if !present {
		return 0, false
	}
	var (
		sum     float64
		matched bool
	)
	for _, s := range samples {
		if s.Labels.Matches(match) {
			sum += s.Value
			matched = true
		}
	}
	return sum, matched
}

// ByLabel splits a metric by one label, summing series that share a value.
// Series lacking the label are collected under "".
func (e Exposition) ByLabel(name, label string) map[string]float64 {
	out := map[string]float64{}
	for _, s := range e[name] {
		out[s.Labels[label]] += s.Value
	}
	return out
}

// Names returns the metric names present, sorted.
func (e Exposition) Names() []string {
	out := make([]string, 0, len(e))
	for k := range e {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
