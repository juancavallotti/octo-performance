package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/juancavallotti/octo-performance/harness/internal/spec"
	"gopkg.in/yaml.v3"
)

// Action is what happened to one knob.
type Action string

const (
	// Stripped: the key was removed so the runtime uses its own default.
	Stripped Action = "stripped"
	// Set: the key was rewritten to a literal.
	Set Action = "set"
	// Kept: the knob is declared as a tunable but the arm supplied no value, so the
	// source literal ran. Recorded because a tuned arm that quietly inherits a source
	// value is not the arm anyone thinks it is.
	Kept Action = "kept"
)

// Change records one edit, and is the provenance the rendered YAML cannot carry.
type Change struct {
	Knob   string `json:"knob"`
	Path   string `json:"path"`
	Action Action `json:"action"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Line   int    `json:"line"`
}

// Request is one rendering.
type Request struct {
	Source     []byte
	SourceName string
	Mode       spec.Mode
	Tunables   []spec.Selector
	// Values are the literals for a tuned arm, keyed by knob name.
	Values map[string]string
}

// Result is the rendered config plus everything needed to explain it.
type Result struct {
	Rendered []byte    `json:"-"`
	Source   string    `json:"source"`
	Digest   string    `json:"sourceDigest"`
	Mode     spec.Mode `json:"mode"`
	Changes  []Change  `json:"changes"`
	// Declared is every tunable actually found in the source, so a scenario that
	// names a knob its integration does not have is visible.
	Declared []string `json:"declared"`
}

// Render derives one arm's config, then verifies its own output.
func Render(req Request) (Result, error) {
	if req.Mode != spec.Baseline && req.Mode != spec.Tuned {
		return Result{}, fmt.Errorf("render: unknown mode %q", req.Mode)
	}

	sum := sha256.Sum256(req.Source)
	res := Result{
		Source: req.SourceName,
		Digest: hex.EncodeToString(sum[:]),
		Mode:   req.Mode,
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(req.Source, &doc); err != nil {
		return Result{}, fmt.Errorf("render: parsing %s: %w", req.SourceName, err)
	}

	if err := checkValuesAreDeclared(req); err != nil {
		return Result{}, err
	}

	// Collect every match first: stripping mutates the mappings, so locating and
	// editing cannot be interleaved.
	type located struct {
		knob    string
		matches []match
	}
	var all []located
	var errs []error

	for _, t := range req.Tunables {
		segs, err := parsePath(t.ResolvedPath())
		if err != nil {
			errs = append(errs, fmt.Errorf("tunable %q: %w", t.Name, err))
			continue
		}
		var ms []match
		findMatches(&doc, segs, "", &ms)
		if len(ms) > 0 {
			res.Declared = append(res.Declared, t.Name)
		}
		all = append(all, located{knob: t.Name, matches: ms})
	}
	if err := errors.Join(errs...); err != nil {
		return Result{}, fmt.Errorf("render: %w", err)
	}
	sort.Strings(res.Declared)

	switch req.Mode {
	case spec.Tuned:
		for _, l := range all {
			literal, wanted := req.Values[l.knob]
			for _, m := range l.matches {
				v := m.value()
				if !wanted {
					res.Changes = append(res.Changes, Change{
						Knob: l.knob, Path: m.path, Action: Kept, From: v.Value, To: v.Value, Line: m.line,
					})
					continue
				}
				res.Changes = append(res.Changes, Change{
					Knob: l.knob, Path: m.path, Action: Set, From: v.Value, To: literal, Line: m.line,
				})
				setScalar(v, literal)
			}
			if wanted && len(l.matches) == 0 {
				errs = append(errs, fmt.Errorf(
					"knob %q was given the value %q but %s declares it nowhere at %s",
					l.knob, literal, req.SourceName, selectorPath(req.Tunables, l.knob)))
			}
		}
	case spec.Baseline:
		// Strip back-to-front within each mapping so earlier indices stay valid.
		type removal struct {
			parent *yaml.Node
			keyIdx int
		}
		var removals []removal
		for _, l := range all {
			for _, m := range l.matches {
				res.Changes = append(res.Changes, Change{
					Knob: l.knob, Path: m.path, Action: Stripped, From: m.value().Value, Line: m.line,
				})
				removals = append(removals, removal{m.parent, m.keyIdx})
			}
		}
		sort.Slice(removals, func(i, j int) bool { return removals[i].keyIdx > removals[j].keyIdx })
		for _, r := range removals {
			r.parent.Content = append(r.parent.Content[:r.keyIdx], r.parent.Content[r.keyIdx+2:]...)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Result{}, fmt.Errorf("render: %w", err)
	}

	sort.SliceStable(res.Changes, func(i, j int) bool {
		if res.Changes[i].Path != res.Changes[j].Path {
			return res.Changes[i].Path < res.Changes[j].Path
		}
		return res.Changes[i].Knob < res.Changes[j].Knob
	})

	out, err := encode(&doc)
	if err != nil {
		return Result{}, fmt.Errorf("render: encoding %s: %w", req.SourceName, err)
	}
	res.Rendered = out

	if err := Verify(out, req); err != nil {
		return Result{}, err
	}
	return res, nil
}

// Verify re-parses rendered output and asserts the mode's invariant.
//
// It is called by [Render] rather than left to the caller, because an unverified
// baseline is indistinguishable from a verified one until it silently reports the
// wrong default months later.
func Verify(rendered []byte, req Request) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(rendered, &doc); err != nil {
		return fmt.Errorf("render: re-parsing rendered output: %w", err)
	}

	var errs []error
	switch req.Mode {
	case spec.Baseline:
		for _, t := range req.Tunables {
			var found []string
			keyPathsNamed(&doc, t.Name, "", &found)
			if len(found) > 0 {
				errs = append(errs, fmt.Errorf(
					"baseline still declares %q at %s; baseline strips, it never hardcodes",
					t.Name, strings.Join(found, ", ")))
			}
		}
	case spec.Tuned:
		for knob, want := range req.Values {
			segs, err := parsePath(selectorPath(req.Tunables, knob))
			if err != nil {
				errs = append(errs, err)
				continue
			}
			var ms []match
			findMatches(&doc, segs, "", &ms)
			if len(ms) == 0 {
				errs = append(errs, fmt.Errorf("tuned arm set %q but it is absent from the rendered config", knob))
				continue
			}
			for _, m := range ms {
				if got := m.value().Value; got != want {
					errs = append(errs, fmt.Errorf(
						"tuned arm set %s=%s but the rendered config has %s at %s", knob, want, got, m.path))
				}
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("render: verification failed: %w", err)
	}
	return nil
}

// checkValuesAreDeclared refuses a value for a knob the scenario does not expose. A
// typo here would otherwise render a config identical to the baseline and report it as
// tuned.
func checkValuesAreDeclared(req Request) error {
	if req.Mode != spec.Tuned || len(req.Values) == 0 {
		return nil
	}
	declared := make(map[string]bool, len(req.Tunables))
	for _, t := range req.Tunables {
		declared[t.Name] = true
	}
	var errs []error
	for _, k := range sortedKeys(req.Values) {
		if !declared[k] {
			errs = append(errs, fmt.Errorf(
				"knob %q is not a declared tunable of this scenario (has: %s)",
				k, strings.Join(tunableNames(req.Tunables), ", ")))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("render: %w", err)
	}
	return nil
}

// setScalar rewrites a value node to a plain literal, dropping any quoting style and
// letting the emitter infer the tag — so an integer knob emits as an integer, which is
// what makes the archived config record a value rather than a reference.
func setScalar(n *yaml.Node, literal string) {
	n.Kind = yaml.ScalarNode
	n.Tag = ""
	n.Style = 0
	n.Value = literal
	n.Content = nil
	n.Anchor = ""
	n.Alias = nil
}

func encode(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func selectorPath(ts []spec.Selector, knob string) string {
	for _, t := range ts {
		if t.Name == knob {
			return t.ResolvedPath()
		}
	}
	return "flows[*]." + knob
}

func tunableNames(ts []spec.Selector) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
