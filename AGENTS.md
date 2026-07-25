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
| `docs/` | GitHub Pages skeleton. Pages is **not** enabled yet; reports are Pages-ready markdown. |

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
   `tuned` has them rewritten to the requested values. Knobs are plain integers, **not**
   `${ENV}` placeholders — substitution does not reach root-flow fields (see below).
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
task report   RUN=<run-id>                           # regenerate a REPORT.md
task index                                           # regenerate results/index.md
```

Common variables: `TARGET=native|docker`, `REPS=1`, `HOST=local`, `TEST=steady|capacity`,
`TUNED_WORKERS=16 TUNED_BUFFER=256 TUNED_POOL=8`, `OCTO_IMAGE=juancavallotti/octo-runtime:0.4.3`,
`COOLDOWN_SECONDS=15`. A knob the Taskfile does not forward works as an environment prefix,
since task inherits the environment: `TUNED_MAXOPENCONNS=64 task bench SCENARIO=...`.

**Benchmarking a specific build.** `OCTO_BIN` points the native target at a particular binary
instead of whatever is on `PATH`, which is how two releases get compared on the same host:

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
   (`workers: 8`). Do not use `${FLOW_WORKERS}`-style placeholders: `${ENV}` substitution does
   not reach root-flow fields and fails at load. Declare each knob in exactly **one** place, or
   the renderer — which rewrites every occurrence of a name — will set them all together.
3. `scenario.env` — `ROUTE`, `TUNABLES` (default `workers buffer pool`; add e.g. `maxOpenConns`
   or `listeners` where the scenario exposes them), `STEADY_RATE`, durations, and the sweep grid.
   Set `READY_ROUTE` if the measured route is a POST or otherwise cannot answer a bare GET.
   If the scenario needs infrastructure, add executable `setup.sh` / `teardown.sh` beside it;
   the harness runs them outside the measured window.
4. `k6/smoke.js`, `k6/steady.js`, `k6/capacity.js` — import from `lab/k6/lib/`. Always read
   `BASE_URL` from the environment; never hardcode a host.
5. `task verify:render SCENARIO=<id>` to confirm both arms render, and `task diff SCENARIO=<id>`
   to see exactly what separates them.
6. Run `task smoke` before anything else.

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

[COMPARISON.md](COMPARISON.md) is the authority: what each vendor published, under what
conditions, which scenarios have been rebuilt, and what may be claimed. Read it before writing
any sentence that puts an Octo number next to somebody else's. The short form:

1. **Load model is not a detail.** Every published vendor benchmark is closed-model — throughput
   against a fixed virtual-user population — and their "knee point" is an artifact of that model.
   This lab is open-model everywhere except `task vuramp`, which exists for exactly this purpose.
   Never put an open-model number and a closed-model number in the same table.
2. **Only footprint and CPU-ms/request are defensible today.** Throughput is not: theirs comes
   from dedicated servers with dedicated load generators, ours from a laptop running both.
3. **Match the scenario before claiming anything.** Camel's headline 0.345 ms is *in-process
   routing latency*, not an end-to-end HTTP request.
4. **Record what could not be built.** Four of a commercial platform's six standalone use cases have no Octo
   equivalent. Those gaps belong in COMPARISON.md and in Notion — they are findings, not
   omissions.

`CPU_LIMIT=1 task bench ...` caps the container the way a commercial platform sizes a a hosted platform worker, and
stamps the cap into the run id. Use it whenever the point of a run is comparability rather than
Octo-against-itself.

## Probing by hand

Ad-hoc measurement outside the harness is fine for forming a hypothesis and is **not** a result.
If you do it, two non-negotiables, both learned the hard way in one sitting:

- **Assert the port is free before starting, and that the runtime logged `runtime ready`.** A
  stale process holding 8080 does not fail loudly; it answers 404 quickly, which reads as
  excellent throughput.
- **Read `http_req_failed` before reading `http_reqs`.** A run that failed 100% of its requests
  reports a throughput number like any other.

Anything worth publishing gets re-measured through `task bench` or `task vuramp`.
