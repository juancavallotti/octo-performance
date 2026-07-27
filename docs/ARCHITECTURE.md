# Architecture

The harness that produces every number in this repository. Read [LEARNINGS.md](LEARNINGS.md) first —
this document is the design, that one is why it is shaped this way.

## What it is

`perf` is a single Go binary that executes a **campaign**: a declarative comparison of two or more
arms across a set of scenarios, run on dedicated hardware, with every cell gated for validity before
it is allowed to contribute to a number, producing one self-contained HTML report.

It replaces 5,530 lines of shell and Python in `lab/bin/`, four orchestrators that each re-implemented
the same procedure, and a publishing path that printed point estimates from saturated runs.

## Topology

```
  operator laptop
        │  ssh (IAP tunnel)
        ▼
  runner VM   c4-standard-16
    ├─ perf              the orchestrator: plans, drives, gates, reports
    ├─ k6                load generation
    └─ host sampler      1 Hz runner CPU / RSS / loadavg
        │
        │  HTTP load, internal VPC, no external hop
        ▼
  subject VM  c4-standard-8
    ├─ perf-agent        supervises octo; samples /proc at 1 Hz; NDJSON over one ssh session
    └─ octo run --config … [--observability-addr :39999] [--metrics]
        │
        ▼
  deps VM     c4-standard-4      (only scenarios that need it)
    ├─ postgres      003-postgres-crud
    └─ labbackend    005-http-proxy
```

Three properties the laptop could not provide:

1. **The generator is measured, not assumed.** The runner samples itself, so contention becomes a
   failed gate instead of a published number ([L1](LEARNINGS.md#l1)).
2. **The subject VM runs only the subject.**
3. **Dependencies are a network hop, not a NAT excursion** ([L19](LEARNINGS.md#l19)).

Infrastructure is ephemeral: `terraform apply` → campaign → collect → `terraform destroy`. Because
the machines do not outlive the campaign, the fingerprint has to be complete enough that the results
stay interpretable afterwards.

**Arms alternate on the same subject VM.** Giving each arm its own VM would make interleaving
impossible and reintroduce exactly the confound interleaving exists to remove.

## Package tree

```
cmd/perf                  operator CLI: plan | run | resume | collect | report | doctor
cmd/perf-agent            subject-side supervisor + sampler; pushed per campaign, never installed

internal/spec             campaign + scenario YAML: types, loading, defaulting, validation
internal/plan             spec → ordered []Cell; interleaving; deterministic; content-hashed
internal/render           integration.yaml → arm config (baseline strips / tuned rewrites)
internal/exec             Runner: run a process and move bytes on a host. local + ssh.
internal/agent            agent wire protocol, runner-side client, subject-side server
internal/subject          Target: octo lifecycle, capability detection, admin-port client
internal/loadgen          LoadGenerator: k6 invocation, summary + time-series parsing, pool sizing
internal/collect          runner-side collectors: prom scraper, runner host sampler
internal/promx            Prometheus exposition parsing, window differencing, bounded quantiles
internal/series           time-series primitive: Point / Series / Frame, windowing, differentiation
internal/stats            median/IQR/MAD, bootstrap CI, noise band, steady-state, order effect
internal/gate             Gate interface, the concrete gates, Verdict aggregation
internal/fingerprint      host fingerprint capture + comparability diff/hash
internal/result           on-disk artifact layout, cell.json / campaign.json schema
internal/report           the single self-contained HTML file
internal/campaign         the orchestrator: the one per-cell procedure
internal/fake             test doubles: in-process octo-alike, fake load generator, fake runner
```

**Dependency shape.** `series`, `promx`, `stats`, `render` and `spec` are leaves with no I/O.
`gate` depends only on leaves plus value types. `campaign` is the only package that performs effects
in sequence, and it takes every effect as an interface. That split is why roughly 60% of the value
is testable without a VM, a network, or a subprocess.

## The one procedure

`campaign.RunCell` exists once and replaces four copies ([L16](LEARNINGS.md#l16)):

1. render the arm's config → `Put` into a per-cell staging dir on the subject
2. `Capabilities` (cached by binary sha256) → assemble argv; withhold `--metrics` when absent
3. `Start`; `Ready`; record cold start **and the method used to detect it**
4. positive capability confirmation: if an admin port was detected, `/livez` must answer
5. `Identity` from `/metrics` — what is *running*, against what was intended
6. start samplers (subject proc, prom, runner host) — **before** the window
7. smoke pass, once per scenario+arm — a load run against a broken endpoint is not a result
8. warm-up pass, artifacts kept and marked as warm-up
9. measured pass, with `k6 --out csv` so there is a time series to reason about
10. stop samplers; stop the subject → whole-lifetime `Rusage` from `wait4`
11. `stats.DetectSteady` over the k6 RPS series → `Window`
12. window everything: series slicing, prom scrape selection and differencing, CPU delta
13. `gate.Evaluate` → `Verdict`
14. write `cell.json` and artifacts atomically
15. cooldown

Sweep is `reps: 1` with an arm per grid point. Capacity is `load.test: capacity`. VU-ramp is
`load.model: closed` with an arm per VU level. No new programs.

## Core interfaces

Five abstractions. Everything else is a plain struct.

```go
// exec — the boundary between local iteration and a real campaign.
// Args is argv, never a shell string: every quoting bug in lab/bin/ is gone by construction.
type Runner interface {
	Name() string
	Run(ctx context.Context, c Cmd) (Result, error)
	Start(ctx context.Context, c Cmd) (Process, error)
	Put(ctx context.Context, dst string, mode fs.FileMode, r io.Reader) error
	Get(ctx context.Context, src string) (io.ReadCloser, error)
	Close() error
}

// subject — something startable, probeable, stoppable.
type Target interface {
	// Capabilities asks the ARTIFACT, never a version string. See L9.
	Capabilities(ctx context.Context, bin string) (Caps, error)
	Start(ctx context.Context, req StartRequest) (Handle, error)
}

// loadgen — how load is offered. Model is recorded, never inferred, so an open-model
// and a closed-model number can never share a table.
type LoadGenerator interface {
	Name() string
	Version(ctx context.Context) (string, error)
	Run(ctx context.Context, req Request) (Run, error)
}

// collect — anything producing a timestamped series. Samplers start before the window
// opens and stop after it closes; the window is selected afterwards, never by when
// sampling happened to begin.
type Sampler interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) (Collected, error)
}

// gate — validity. Pure: no I/O, no mutation, no aborting. Returns findings, never a bool.
type Gate interface {
	ID() string
	Check(Evidence) []Finding
}
```

`exec.Runner` has two implementations, `Local` and `SSH`. **`Local` is a supported topology, not a
test stub**: `runner: {kind: local}, subject: {kind: local}` reproduces the old colocated laptop
setup through the identical code path — and the saturation gate correctly marks those cells suspect.
The fast iteration loop therefore exercises the same code as the eight-hour campaign.

There is deliberately no `RemoteSampler`/`LocalSampler` split and no `target-native.sh` /
`target-docker.sh` dispatch. One `Target`, one `Sampler`, parameterised by a `Runner`.

## Why there is a second binary

`perf-agent` is cross-compiled for linux/amd64, embedded in `perf` via `go:embed`, pushed once per
campaign, verified by sha256, and driven over a single SSH session speaking NDJSON on stdio. No
listener, no auth story, no installation, removed on teardown.

It earns its place three times over:

- **It is octo's parent**, so `wait4` yields whole-lifetime user+sys CPU and max RSS directly. The
  `/usr/bin/time` wrapper, `pgrep -P "$wrapper"`, `wrapper.pid` and the BSD-vs-GNU flag branch all
  disappear — that mechanism was a genuine race.
- **It measures the clock offset** at cell start and cell end ([L20](LEARNINGS.md#l20)).
- It is Go, so `/proc` parsing is unit-tested against committed fixtures instead of being untested
  `awk`.

## Invariants

These are what the tests defend. A change that breaks one is a bug even if everything compiles.

**Validity**
- A `Verdict` is computed from evidence and attached to the cell. Gates read; they never mutate a
  measurement and never abort a run.
- No non-valid cell contributes to any aggregate. The report is structurally unable to include one
  in a median.
- `Invalid` means "excluded from the headline, kept on disk, shown in the appendix" — never
  "campaign aborted". A badly chosen threshold must cost a re-read, not an afternoon on a VM you are
  paying for.

**Numbers**
- No code path emits a bare point estimate. Every reported number carries its verdict and its
  dispersion.
- A quantile derived from a histogram is a `promx.Quantile`, never a float. `Seconds` is meaningless
  unless `Ok` ([L8](LEARNINGS.md#l8)).
- `loadgen.Run.Model` is recorded at the source. Open-model and closed-model numbers never share a
  table.
- Anything a gate needs to see is a recorded value, not a side effect inside a script
  ([L2](LEARNINGS.md#l2)).

**Windows**
- Samplers start before the window and stop after it. The window is chosen post-hoc.
- Every prom scrape is retained; the two bracketing the *detected* window are differenced
  ([L11](LEARNINGS.md#l11)).
- Both the detected window and the naive fixed window are recorded, so detection is auditable
  ([L12](LEARNINGS.md#l12)).

**Provenance**
- `perf` refuses to start an arm whose version cannot be resolved ([L13](LEARNINGS.md#l13)).
- The assembled argv, the raw `run --help`, and the rendered config are archived per cell.
- A field that reaches neither the report nor a gate is deleted, not collected
  ([L18](LEARNINGS.md#l18)).

**Layering**
- `report` derives nothing ([L17](LEARNINGS.md#l17)).
- `campaign` is the only package that sequences effects.

## Rendering: YAML nodes, not line regex

[AGENTS.md](../AGENTS.md) documents the landmine in the current renderer: *"Declare each knob in
exactly one place, or the renderer — which rewrites every occurrence of a name — will set them all
together."* And the actual rule — `workers`/`buffer`/`pool` are root-flow only, sub-flows inherit —
is not expressible as a line regex at all.

So selectors carry a path (`flows[*].workers`), and `render` operates on `yaml.v3` nodes. The cost is
that re-emission does not preserve byte-exactness in untouched regions, so provenance moves off
whitespace and into a structured record: `config.render.json` states source digest, selector, node
path, before → after, and source line. That is better provenance than the archived YAML alone ever
was — it says what changed and where.

`Render` fails when a tuned selector matches zero nodes, when a selector matches more than one path
without an explicit `[*]`, or when post-render verification finds a baseline still declaring a
tunable. `Render(Render(x)) == Render(x)` is asserted in tests.

## Gates

Ship them in **observe mode**. Nobody knows the right runner-CPU ceiling or pool-growth ratio,
because the old lab never measured them. So every gate computes and records its evidence
unconditionally from the first campaign, levels start conservative, and the report carries a
calibration section showing the observed distribution of each gate's evidence across all cells.
Thresholds tighten from data.

| Gate | Fires on |
|---|---|
| `loadgen.saturation` | runner CPU ceiling; pool growth over allocation; pool at cap; RPS falling while runner CPU rises; **a sibling cell's pool differing by more than 2×** |
| `loadgen.errors` | non-zero `http_req_failed`; also server-side `failed`/`dropped` while the client saw success |
| `loadgen.rate-gap` | achieved/offered below threshold |
| `loadgen.dropped` | `dropped_iterations != 0` |
| `window.steady-state` | no window satisfies CV, slope, minimum duration and flat subject CPU |
| `subject.identity` | `octo_build_info` version ≠ the version the harness intended to start |
| `env.fingerprint` | compared cells differ in cores, machine type, kernel, or readiness method |

The cross-cell check in the first row is the one that catches 1,600 → 7,613, which is why
`gate.Evidence` carries peer cells.

## Artifact layout

```
campaigns/<date>-<name>-<planhash8>/
  campaign.yaml               the spec as given, verbatim
  plan.json                   expanded cells + execution order, written BEFORE running
  fingerprint/{runner,subject}.json
  agent.json                  sha256 + protocol version of the deployed agent
  state.json                  per-cell completion — the resume key
  run.log                     the harness's own NDJSON log
  cells/<scenario>__<arm>__rep<N>/
    cell.json                 the machine-readable result
    verdict.json              duplicated, for grep
    config.yaml               what ran
    config.render.json        structured change record — the real provenance
    caps.json                 incl. raw `run --help`
    argv.json                 the exact octo argv
    k6/{summary.json,timeseries.csv.gz,k6.log,script.js}
    subject/{proc.csv.gz,octo.log,metrics.ndjson.gz}
    runner/host.csv.gz
  campaign.json               rolled-up stats, comparisons, headline verdict
  report.html                 the deliverable
```

The campaign id is content-addressed and deliberately **not** version-stamped. The old run id baked
one octo version into the directory name, which is precisely why comparing two versions took two runs
plus a separate compare step. A campaign compares versions; versions live per-arm.

## Testing

| Layer | How |
|---|---|
| `series`, `promx`, `stats`, `render`, `spec`, `plan`, `gate` | Pure, table-driven, no I/O. Golden files per scenario for `render`. The replay corpus for `gate`. |
| `agent`, `loadgen`, `fingerprint` | Parsers against committed fixtures: real `/proc` files, real k6 summary + CSV, real exposition. |
| `campaign.RunCell` | End-to-end against `fake.Subject` + `fake.LoadGen`, in normal `go test`, in seconds. |
| `report` | Golden HTML. Self-containment asserted by grepping the output for `<script src`, `<link `, `http://`, `url(`. |
| SSH transport, real k6 | Build-tagged, opt-in. k6's output shape is not a stable API and needs a nightly contract test. |

The replay corpus ([`internal/gate/testdata/corpus/`](../internal/gate/testdata/corpus/README.md))
holds three real cells whose required verdicts are fixed. A gate suite that passes the `collapsed`
specimen is broken regardless of what else it does.
