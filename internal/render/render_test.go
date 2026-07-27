package render

import (
	"strings"
	"testing"

	"github.com/juancavallotti/octo-performance/internal/spec"
	"gopkg.in/yaml.v3"
)

// The shape of 001-template-page: knobs on the root flow.
const simple = `
service:
  name: perf-template-page

flows:
  - name: page
    workers: 8
    buffer: 64
    pool: 8
    source:
      connector: api
      type: http
`

// The shape of 004-queue-roundtrip: two root flows, a different knob on each, and a
// consumer flow that deliberately declares no workers of its own.
const twoFlows = `
flows:
  - name: submit
    workers: 8
    buffer: 64
    source:
      connector: api
      type: http
  - name: worker
    source:
      connector: bus
      type: queue
      settings:
        subject: work.jobs
        listeners: 8
        timeout: 30s
`

// The shape of 003-postgres-crud: knobs on a connector's settings as well as the flow.
const connectorKnobs = `
connectors:
  - name: api
    type: http
    settings:
      port: 8080
  - name: db
    type: database
    settings:
      driver: postgres
      maxOpenConns: 25
      maxIdleConns: 25
      connMaxLifetime: 5m

flows:
  - name: order-crud
    workers: 8
    pool: 8
`

func sel(names ...string) []spec.Selector {
	out := make([]spec.Selector, len(names))
	for i, n := range names {
		out[i] = spec.Selector{Name: n}
	}
	return out
}

func mustRender(t *testing.T, req Request) Result {
	t.Helper()
	r, err := Render(req)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return r
}

// keys reports every path at which name appears as a mapping key.
func keys(t *testing.T, doc []byte, name string) []string {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal(doc, &n); err != nil {
		t.Fatalf("parse: %v", err)
	}
	var out []string
	keyPathsNamed(&n, name, "", &out)
	return out
}

func TestBaselineStripsEveryTunable(t *testing.T) {
	r := mustRender(t, Request{
		Source: []byte(simple), SourceName: "integration.yaml",
		Mode: spec.Baseline, Tunables: sel("workers", "buffer", "pool"),
	})

	for _, knob := range []string{"workers", "buffer", "pool"} {
		if got := keys(t, r.Rendered, knob); len(got) != 0 {
			t.Fatalf("baseline still declares %q at %v", knob, got)
		}
	}
	// Everything else survives.
	if !strings.Contains(string(r.Rendered), "perf-template-page") {
		t.Fatal("baseline lost unrelated content")
	}
	if len(keys(t, r.Rendered, "source")) == 0 {
		t.Fatal("baseline removed more than the knobs")
	}
	if len(r.Changes) != 3 {
		t.Fatalf("expected 3 changes, got %d: %+v", len(r.Changes), r.Changes)
	}
	for _, c := range r.Changes {
		if c.Action != Stripped {
			t.Fatalf("change %+v should be a strip", c)
		}
		if c.From == "" || c.Line == 0 {
			t.Fatalf("change %+v lost its provenance", c)
		}
	}
}

func TestTunedRewritesToLiterals(t *testing.T) {
	r := mustRender(t, Request{
		Source: []byte(simple), SourceName: "integration.yaml",
		Mode:     spec.Tuned,
		Tunables: sel("workers", "buffer", "pool"),
		Values:   map[string]string{"workers": "128", "buffer": "256", "pool": "8"},
	})

	out := string(r.Rendered)
	for _, want := range []string{"workers: 128", "buffer: 256", "pool: 8"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered config missing %q:\n%s", want, out)
		}
	}
	// Literals, not quoted strings: the archived config must record a value.
	if strings.Contains(out, `workers: "128"`) {
		t.Fatal("knob was emitted as a quoted string")
	}
}

// AGENTS.md documents the landmine the line-regex renderer created: rewriting every
// occurrence of a name sets them all together. The root-flow default path is what
// prevents it.
func TestKnobsAreScopedToTheirDeclaredPath(t *testing.T) {
	r := mustRender(t, Request{
		Source: []byte(twoFlows), SourceName: "integration.yaml",
		Mode: spec.Tuned,
		Tunables: []spec.Selector{
			{Name: "workers"},
			{Name: "buffer"},
			{Name: "listeners", Path: "flows[*].source.settings.listeners"},
		},
		Values: map[string]string{"workers": "512", "listeners": "64"},
	})

	out := string(r.Rendered)
	if !strings.Contains(out, "workers: 512") {
		t.Fatalf("producer workers not set:\n%s", out)
	}
	if !strings.Contains(out, "listeners: 64") {
		t.Fatalf("consumer listeners not set:\n%s", out)
	}
	// The consumer flow declares no workers and must not acquire one.
	if got := keys(t, r.Rendered, "workers"); len(got) != 1 {
		t.Fatalf("workers appears at %v; it must reach only the flow that declares it", got)
	}
	// buffer was declared as a tunable but given no value: recorded as kept, not
	// silently inherited.
	var keptBuffer bool
	for _, c := range r.Changes {
		if c.Knob == "buffer" && c.Action == Kept {
			keptBuffer = true
		}
	}
	if !keptBuffer {
		t.Fatalf("a tuned arm inheriting a source value must record it: %+v", r.Changes)
	}
}

func TestKnobsOnConnectorSettings(t *testing.T) {
	tunables := []spec.Selector{
		{Name: "workers"},
		{Name: "pool"},
		{Name: "maxOpenConns", Path: "connectors[*].settings.maxOpenConns"},
		{Name: "maxIdleConns", Path: "connectors[*].settings.maxIdleConns"},
	}

	tuned := mustRender(t, Request{
		Source: []byte(connectorKnobs), SourceName: "integration.yaml",
		Mode: spec.Tuned, Tunables: tunables,
		Values: map[string]string{"workers": "512", "maxOpenConns": "64", "maxIdleConns": "64"},
	})
	out := string(tuned.Rendered)
	for _, want := range []string{"maxOpenConns: 64", "maxIdleConns: 64", "workers: 512"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	// The unrelated http connector keeps its port.
	if !strings.Contains(out, "port: 8080") {
		t.Fatal("a sibling connector was disturbed")
	}
	// connMaxLifetime is not a tunable here and must be untouched.
	if !strings.Contains(out, "connMaxLifetime: 5m") {
		t.Fatal("a non-tunable setting was altered")
	}

	base := mustRender(t, Request{
		Source: []byte(connectorKnobs), SourceName: "integration.yaml",
		Mode: spec.Baseline, Tunables: tunables,
	})
	for _, knob := range []string{"workers", "pool", "maxOpenConns", "maxIdleConns"} {
		if got := keys(t, base.Rendered, knob); len(got) != 0 {
			t.Fatalf("baseline still declares %q at %v", knob, got)
		}
	}
	if !strings.Contains(string(base.Rendered), "connMaxLifetime: 5m") {
		t.Fatal("baseline stripped a setting that is not a tunable")
	}
}

func TestRenderIsIdempotent(t *testing.T) {
	req := Request{
		Source: []byte(simple), SourceName: "integration.yaml",
		Mode: spec.Tuned, Tunables: sel("workers", "buffer", "pool"),
		Values: map[string]string{"workers": "128", "buffer": "256", "pool": "8"},
	}
	once := mustRender(t, req)

	req2 := req
	req2.Source = once.Rendered
	twice := mustRender(t, req2)

	if string(once.Rendered) != string(twice.Rendered) {
		t.Fatalf("rendering is not idempotent:\n--- once ---\n%s\n--- twice ---\n%s",
			once.Rendered, twice.Rendered)
	}

	baseReq := Request{
		Source: []byte(simple), SourceName: "integration.yaml",
		Mode: spec.Baseline, Tunables: sel("workers", "buffer", "pool"),
	}
	b1 := mustRender(t, baseReq)
	baseReq.Source = b1.Rendered
	b2 := mustRender(t, baseReq)
	if string(b1.Rendered) != string(b2.Rendered) {
		t.Fatal("baseline rendering is not idempotent")
	}
}

func TestCommentsSurvive(t *testing.T) {
	src := `
# leading comment
flows:
  - name: page
    # why this knob exists
    workers: 8
    pool: 8
`
	r := mustRender(t, Request{
		Source: []byte(src), SourceName: "integration.yaml",
		Mode: spec.Tuned, Tunables: sel("workers", "pool"),
		Values: map[string]string{"workers": "128"},
	})
	out := string(r.Rendered)
	if !strings.Contains(out, "leading comment") {
		t.Fatalf("lost the document comment:\n%s", out)
	}
	if !strings.Contains(out, "why this knob exists") {
		t.Fatalf("lost a knob's comment:\n%s", out)
	}
}

// --- refusals ---

func TestTunedRefusesAKnobTheScenarioDoesNotDeclare(t *testing.T) {
	_, err := Render(Request{
		Source: []byte(simple), SourceName: "integration.yaml",
		Mode: spec.Tuned, Tunables: sel("workers"),
		Values: map[string]string{"wrokers": "128"},
	})
	if err == nil {
		t.Fatal("a mistyped knob would render a config identical to the baseline and report it as tuned")
	}
	if !strings.Contains(err.Error(), "wrokers") {
		t.Fatalf("error should name the offending knob: %v", err)
	}
}

func TestTunedRefusesAKnobTheIntegrationDoesNotHave(t *testing.T) {
	_, err := Render(Request{
		Source: []byte(simple), SourceName: "integration.yaml",
		Mode:     spec.Tuned,
		Tunables: []spec.Selector{{Name: "listeners", Path: "flows[*].source.settings.listeners"}},
		Values:   map[string]string{"listeners": "64"},
	})
	if err == nil {
		t.Fatal("setting a knob the document never declares must fail")
	}
	if !strings.Contains(err.Error(), "listeners") {
		t.Fatalf("error should name the knob: %v", err)
	}
}

// A knob appearing outside its declared path is a scenario bug. Leaving it behind in a
// baseline would report the wrong default; failing is the honest outcome.
func TestBaselineFailsWhenAKnobHidesOutsideItsDeclaredPath(t *testing.T) {
	src := `
flows:
  - name: outer
    workers: 8
    process:
      - type: composite
        settings:
          flow:
            workers: 4
`
	_, err := Render(Request{
		Source: []byte(src), SourceName: "integration.yaml",
		Mode: spec.Baseline, Tunables: sel("workers"),
	})
	if err == nil {
		t.Fatal("a surviving declaration must fail verification")
	}
	if !strings.Contains(err.Error(), "baseline strips") {
		t.Fatalf("error should explain the rule: %v", err)
	}
}

func TestRenderRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		req  Request
		want string
	}{
		{
			name: "unknown mode",
			req:  Request{Source: []byte(simple), Mode: "sideways", Tunables: sel("workers")},
			want: "unknown mode",
		},
		{
			name: "malformed yaml",
			req:  Request{Source: []byte("flows:\n  - [unclosed"), Mode: spec.Baseline, Tunables: sel("workers")},
			want: "parsing",
		},
		{
			name: "unterminated path index",
			req: Request{Source: []byte(simple), Mode: spec.Baseline,
				Tunables: []spec.Selector{{Name: "workers", Path: "flows[*.workers"}}},
			want: "unterminated",
		},
		{
			name: "non-numeric path index",
			req: Request{Source: []byte(simple), Mode: spec.Baseline,
				Tunables: []spec.Selector{{Name: "workers", Path: "flows[x].workers"}}},
			want: "neither a number nor",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Render(tc.req)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestExplicitIndexSelectsOneElement(t *testing.T) {
	r := mustRender(t, Request{
		Source: []byte(twoFlows), SourceName: "integration.yaml",
		Mode:     spec.Baseline,
		Tunables: []spec.Selector{{Name: "listeners", Path: "flows[1].source.settings.listeners"}},
	})
	if got := keys(t, r.Rendered, "listeners"); len(got) != 0 {
		t.Fatalf("listeners survived at %v", got)
	}
	if !strings.Contains(string(r.Rendered), "subject: work.jobs") {
		t.Fatal("stripped more than the indexed key")
	}
}

func TestResultRecordsProvenance(t *testing.T) {
	r := mustRender(t, Request{
		Source: []byte(simple), SourceName: "scenarios/001/octo/integration.yaml",
		Mode: spec.Tuned, Tunables: sel("workers", "buffer", "pool"),
		Values: map[string]string{"workers": "128"},
	})
	if r.Digest == "" || len(r.Digest) != 64 {
		t.Fatalf("digest = %q", r.Digest)
	}
	if r.Source != "scenarios/001/octo/integration.yaml" {
		t.Fatalf("source = %q", r.Source)
	}
	if len(r.Declared) != 3 {
		t.Fatalf("declared = %v, want all three found in the source", r.Declared)
	}
	var set *Change
	for i := range r.Changes {
		if r.Changes[i].Knob == "workers" {
			set = &r.Changes[i]
		}
	}
	if set == nil || set.Action != Set || set.From != "8" || set.To != "128" {
		t.Fatalf("workers change = %+v, want a recorded 8 -> 128", set)
	}
	if !strings.Contains(set.Path, "flows[0].workers") {
		t.Fatalf("path = %q, want the concrete node path", set.Path)
	}
}

func TestDeclaredReportsWhatTheSourceActuallyHas(t *testing.T) {
	r := mustRender(t, Request{
		Source: []byte(simple), SourceName: "integration.yaml",
		Mode:     spec.Baseline,
		Tunables: sel("workers", "buffer", "pool", "listeners"),
	})
	for _, d := range r.Declared {
		if d == "listeners" {
			t.Fatal("declared should list only knobs the source has")
		}
	}
	if len(r.Declared) != 3 {
		t.Fatalf("declared = %v", r.Declared)
	}
}
