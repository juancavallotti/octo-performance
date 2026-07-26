# AGENTS.md — working contract for the Octo performance lab

This repository benchmarks [Octo](https://juancavallotti.github.io/octo/), a Go integration
runtime that executes YAML-defined flows. Its purpose is to produce **reproducible, comparable,
version-stamped** performance numbers so that regressions are caught and tuning decisions are
evidence-based.

Read [METHODOLOGY.md](METHODOLOGY.md) before interpreting or producing any result.

## Repo map

| Path | What it is |
|---|---|
| `lab/bin/` | The harness. Shell scripts + Python helpers (render, sample, report, index). No dependencies beyond coreutils, `python3`, `curl`, `k6`, and optionally `docker`. |
| `lab/k6/lib/` | Shared k6 helpers: executor/threshold presets and the `handleSummary` that writes `summary.json`. |
| `lab/hosts/` | One `.env` per machine that can run the lab. Defines `HOST_PROFILE` and `BASE_URL`. |
| `scenarios/<id>-<slug>/` | One benchmark scenario: its Octo configs, its template/data assets, its k6 tests, and a README describing the integration. |
| `results/<run-id>/` | Immutable run output. Never hand-edited. |
| `_config.yml`, `index.md` | GitHub Pages. The site is served from the repo **root**, so `results/` and `scenarios/` publish as they are — there is no copy step and no second source of truth for a number. |

A run id is `<date>-<host-profile>-<scenario>-<target>-v<octo-version>`, e.g.
`2026-07-25-m1pro-16gb-001-template-page-native-v0.4.2`.

## Golden rules

These are not style preferences. Breaking one invalidates the numbers.

1. **No version, no result.** Every run writes `env.json` recording the Octo version under test
   (and, for the container target, the image digest). A result that cannot be attributed to a
   version cannot be used for regression tracking.
2. **One scenario, one config.** A scenario declares exactly one `octo/integration.yaml` and names
   its knobs in `scenario.env` as `TUNABLES`. Both arms are *derived* from that file by
   `lab/bin/render-config.py`, so they cannot drift: `baseline` has every tunable stripped,
   `tuned` has them rewritten to the requested values. Prefer plain integers: the rendered file
   is archived beside the result as `config.yaml`, and a literal there is provenance where a
   `${ENV}` placeholder would only record that the value came from somewhere.
3. **Baseline strips, it never hardcodes.** Writing `workers: 8` into a baseline config would
   freeze it at today's default and silently stop tracking the real one. Stripping the key makes
   the runtime fall back to whatever it actually ships with, so if a future Octo version changes a
   default, the baseline arm follows it — which is exactly the regression this lab exists to catch.
   `render-config.py` verifies the rendered baseline declares no tunable.
4. **Never hand-edit anything under `results/`.** If a number looks wrong, re-run. Editing results
   destroys the only thing that makes them worth publishing.
5. **Compare within a target.** `native` vs `docker` numbers on macOS are dominated by Docker
   Desktop's VM and userland port proxy. Baseline↔tuned within one target is the real comparison;
   cross-target is indicative only and must be labelled as such.
6. **Smoke must pass before a load run counts.** A fast server returning 404s is not a result.
7. **Three reps minimum, report the median, keep every rep.** Single runs on a thermally
   throttling laptop are noise.
8. **Report cost, not just speed.** Throughput without CPU-ms/request and peak RSS is an
   incomplete result. The question is never "how fast" alone — it is "how fast, for what".
9. **Nothing runs in the hot path that is not part of the scenario.** No `log` blocks inside the
   measured flow unless logging is the thing being measured.

## Running the lab

Entry points are [go-task](https://taskfile.dev) tasks, matching the convention used in the `octo`
repo. `task --list` shows them all.

```bash
task preflight                                       # check tooling, print versions
task verify                                          # check the lab itself, no runtime needed
task smoke    SCENARIO=001-template-page             # correctness gate
task capacity SCENARIO=001-template-page             # find the knee
task sweep    SCENARIO=001-template-page             # grid search the tuning knobs
task bench    SCENARIO=001-template-page             # baseline + tuned, REPS=3
task build    TARGET=native                          # build the runtime from source
task compare  SCENARIO=001-template-page             # release vs source build, back to back
task report   RUN=<run-id>                           # regenerate a REPORT.md
task index                                           # regenerate results/index.md
```

Common variables: `TARGET=native|docker`, `BUILD=release|dev`, `REPS=1`, `HOST=local`,
`TEST=steady|capacity`, `TUNED_WORKERS=16 TUNED_BUFFER=256 TUNED_POOL=8`,
`OCTO_IMAGE=juancavallotti/octo-runtime:0.4.3`, `COOLDOWN_SECONDS=15`. A knob the Taskfile does
not forward works as an environment prefix, since task inherits the environment:
`TUNED_MAXOPENCONNS=64 task bench SCENARIO=...`.

## Two axes: TARGET and BUILD

`TARGET` says how the runtime is **deployed** — `native` for the binary on the host, `docker` for
the container image. `BUILD` says where the artifact **came from**:

| `BUILD` | Native target | Docker target |
|---|---|---|
| `release` | `OCTO_BIN`, else `PATH` | `OCTO_IMAGE` |
| `dev` | built from `OCTO_SRC` (default `../octo`) | image built from `$OCTO_SRC/runtime/Dockerfile` |

All four combinations work. `BUILD=dev` builds before the run, caching a clean checkout by commit
so re-running costs nothing; a dirty tree is rebuilt every time, because the only honest
assumption about uncommitted work is that it moved.

**Why a dev build gets its own version string.** The source declares the same version constant as
the last release, so `octo version` cannot tell them apart — a dev run would take the release's
run id, collide with it, and be filed as a repeat measurement. A dev build is therefore stamped
`0.4.3-dev.<commit>` (plus `.dirty`), and `env.json` carries the full source provenance: path,
commit, branch, subject, tree state, Go version, build tags.

The native dev build carries no build tags and the container dev build carries `k8s`, matching
how each artifact actually ships. That is not a detail: the tag decides which services provider
is compiled in, so building both the same way would compare against something nobody runs.

```bash
task bench SCENARIO=005-http-proxy BUILD=dev           # measure unreleased work
task compare SCENARIO=005-http-proxy                   # and against the release, back to back
OCTO_SRC=~/src/octo task build TARGET=docker           # a checkout somewhere else
```

`task compare` runs both arms in one invocation on purpose: they then share a host, a thermal
state and a cooldown. Two runs a day apart on a laptop that throttles are not a before-and-after.
Its report states the rep-to-rep spread within each arm and labels any delta smaller than that as
noise, so a 3% difference is never presented as an improvement.

**A dev result is not a published result.** It describes code that has not shipped, and
`REPORT.md` says so. A dirty-tree result is not reproducible by anyone and is marked more
strongly. Quote release numbers; use dev numbers to decide whether a change worked.

**Benchmarking a specific release.** `OCTO_BIN` points the native target at a particular binary,
which is how two releases get compared on the same host:

```bash
OCTO_BIN=~/.octo-versions/octo-0.4.2 task bench SCENARIO=001-template-page
OCTO_BIN=~/.octo-versions/octo-0.4.3 task bench SCENARIO=001-template-page
```

Each run stamps its own version into `env.json` and the run id, so `results/index.md` lines them
up in the regression view. Prefer the released tarball from GitHub over `go install` when the
question is "how does the shipped distribution behave" — they are not the same binary (0.4.3 is
46 MB from the release, 65 MB built locally).

The tasks are a thin interface; the work lives in `lab/bin/` because it involves background
process supervision, signal handling, and PID discovery — things that belong in scripts rather
than in YAML.

`task bench` takes minutes. Run it in the background and poll rather than blocking an interactive
session on it.

## Adding a scenario

1. `scenarios/<nnn>-<slug>/README.md` — describe the integration: what it exercises, which
   connectors and blocks, what the request and response look like, why it is interesting. The
   methodology requires the integration to be described, so this is not optional.
2. `octo/integration.yaml` — the flow, with its knobs written as **plain integers**
   (`workers: 8`) so the archived config records the value that ran. Declare each knob in exactly
   **one** place, or the renderer — which rewrites every occurrence of a name — will set them all
   together.
3. `scenario.env` — `ROUTE`, `TUNABLES` (default `workers buffer pool`; add e.g. `maxOpenConns`
   or `listeners` where the scenario exposes them), `STEADY_RATE`, durations, and the sweep grid.
   Set `READY_ROUTE` if the measured route is a POST or otherwise cannot answer a bare GET.
   If the scenario needs infrastructure, add executable `setup.sh` / `teardown.sh` beside it;
   the harness runs them outside the measured window. If its expressions need CEL functions
   an older runtime does not have, declare `REQUIRES_CEL` — see below.
4. `k6/smoke.js`, `k6/steady.js`, `k6/capacity.js` — import from `lab/k6/lib/`. Always read
   `BASE_URL` from the environment; never hardcode a host.
5. `task verify:render SCENARIO=<id>` to confirm both arms render, and `task diff SCENARIO=<id>`
   to see exactly what separates them.
6. Run `task smoke` before anything else.

### When a scenario needs a newer runtime

A scenario written against CEL functions an older runtime does not declare fails in the worst
possible place: the run reaches preflight clean, starts the target, and *then* the flow fails to
build — a wall of `undeclared reference` inside `octo.log`, after the scenario's dependencies are
already up. `REQUIRES_CEL` in `scenario.env` moves that to the top of the run:

```bash
REQUIRES_CEL='"a,b".split(",").size() == 2 && ["b","a"].sort() == ["a","b"]'
```

One expression, covering everything the flows draw on. It must **compile and return true**, so a
function that is present but behaves differently fails the gate as loudly as one that is absent.

The harness asks the artifact under test with `octo eval` — the binary for `TARGET=native`, the
image for `TARGET=docker` — rather than comparing version strings, because a version string
cannot answer the question: a build from a source checkout reports the same constant as the
release it branched from, feature or no feature. Preflight then names the functions the build
lacks and points at `BUILD=dev`.

This is the mechanism for measuring a capability before it ships. Note the corollary from the
BUILD axis above: until it does ship, what comes out is a dev result, not a published one.

## Findings workflow

Two stages, deliberately separate:

1. **Observe → Notion.** Anything surprising — a bug, a missing capability, an odd curve, a
   question — becomes a dated bullet in the
   [Performance Benchmarking](https://app.notion.com/p/juancavallotti/Performance-Benchmarking-3a88c36eda30803ab07dde64a2b38a1c)
   page, under `Findings Log`, `Enhancements`, `Bugs`, or `Open Questions`. Include the scenario,
   the Octo version, and a link to the evidence in `results/`. This page is for thinking, so
   low-confidence observations belong there too.
2. **Decide → GitHub.** Once we decide to act on something, open an issue on the
   [`juancavallotti/octo`](https://github.com/juancavallotti/octo) repo with `gh issue create`,
   then back-link the issue URL into the Notion bullet. Notion is the log; GitHub is the commitment.

Do not open GitHub issues speculatively from a benchmark observation. The Notion stage exists so
that judgement happens first.

## Things the runtime does not give us

Recorded here so nobody re-discovers them:

- There is no `/metrics`, `/healthz`, or pprof endpoint, and no metrics connector among the
  shipped connectors. All resource data comes from OS-level sampling, and readiness is detected by
  polling a real business route until it returns 200.
- `octo --config <dir>` loads **every** config in that directory, so `baseline.yaml` and
  `tuned.yaml` sitting side by side would both load and collide on the port. `stage-config.sh`
  exists to copy exactly one variant into a clean directory.
- `workers`, `buffer`, and `pool` are **root-flow only**. Sub-flows inside composite blocks
  inherit from the parent and cannot declare their own.

## Comparing against other runtimes

[COMPARISON.md](COMPARISON.md) is the authority: what has to match before two figures
describe the same thing, and what may be claimed. Read it before writing any sentence that
puts an Octo number next to somebody else's. The short form:

1. **Load model is not a detail.** Published benchmarks are almost always closed-model —
   throughput against a fixed virtual-user population — and the "knee point" is an artifact of
   that model.
   This lab is open-model everywhere except `task vuramp`, which exists for exactly this purpose.
   Never put an open-model number and a closed-model number in the same table.
2. **Only footprint and CPU-ms/request are defensible today.** Throughput is not: theirs comes
   from dedicated servers with dedicated load generators, ours from a laptop running both.
3. **Match the scenario before claiming anything.** Camel's headline 0.345 ms is *in-process
   routing latency*, not an end-to-end HTTP request.
4. **Record what could not be built.** Several ordinary integration workloads have no Octo
   equivalent — CSV and XML transformation, policy enforcement, record-oriented batch, Kafka
   and JMS. Those gaps belong in COMPARISON.md — they are findings, not omissions.
5. **Describe workloads, not vendors.** A scenario is justified by what it exercises in the
   runtime — blocking I/O, collection mapping, fan-out — not by who else measured something
   similar. Keep product names out of the repo.

`CPU_LIMIT=1 task bench ...` caps the container to a stated size and stamps the cap into the
run id. Use it whenever the point of a run is comparability rather than Octo-against-itself.

## Probing by hand

Ad-hoc measurement outside the harness is fine for forming a hypothesis and is **not** a result.
If you do it, two non-negotiables, both learned the hard way in one sitting:

- **Assert the port is free before starting, and that the runtime logged `runtime ready`.** A
  stale process holding 8080 does not fail loudly; it answers 404 quickly, which reads as
  excellent throughput.
- **Read `http_req_failed` before reading `http_reqs`.** A run that failed 100% of its requests
  reports a throughput number like any other.

Anything worth publishing gets re-measured through `task bench` or `task vuramp`.
