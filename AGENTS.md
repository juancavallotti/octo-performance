# AGENTS.md — working contract for the Octo performance lab

This repository benchmarks [Octo](https://juancavallotti.github.io/octo/), a Go integration runtime
that executes YAML-defined flows. Its purpose is to produce **reproducible, comparable,
version-stamped** performance numbers so that regressions are caught and tuning decisions are
evidence-based.

Read [METHODOLOGY.md](METHODOLOGY.md) before interpreting or producing any result, and
[docs/LEARNINGS.md](docs/LEARNINGS.md) before changing the harness. The learnings ledger is not
background reading: every row is a failure this lab actually produced, and every row names the test
that now prevents it.

## Repo map

| Path | What it is |
|---|---|
| `harness/` | The harness. One Go module: `perf`, plus `fakeocto` for testing it offline. `go test ./...` covers it. |
| `harness/internal/` | One package per responsibility, each with a `doc.go` stating its single job and its invariants. |
| `scenarios/<id>-<slug>/` | One workload: `scenario.yaml`, `octo/integration.yaml`, assets, and a README describing the integration. |
| `campaigns/*.yaml` | Checked-in campaign specs. A campaign spec **is** the experiment's intent. |
| `campaigns/out/` | Campaign output. Gitignored; publish by copying, never by editing. |
| `infra/terraform/` | The three machines a defensible campaign runs on. |
| `docs/ARCHITECTURE.md` | Topology, package tree, every interface, and the invariants the tests defend. |
| `docs/LEARNINGS.md` | The failure ledger. New failure ⇒ new row **and** new test. |
| `docs/RUNNING.md` | The operator's guide: how to run each kind of test, and how to read what comes out. |

## Golden rules

These are not style preferences. Breaking one invalidates the numbers.

1. **No version, no result.** Every cell records the artifact's sha256, its probed capabilities, and
   what the running process said it was via `octo_build_info` — which is checked against what the
   harness intended to start. A result that cannot be attributed to a version cannot be used for
   regression tracking.
2. **One scenario, one config.** A scenario declares exactly one `octo/integration.yaml` and names
   its knobs in `scenario.yaml` under `tunables`. Both arms are *derived* from that file, so they
   cannot drift.
3. **Baseline strips, it never hardcodes.** Writing `workers: 8` into a baseline would freeze it at
   today's default and silently stop tracking the real one. Stripping the key makes the runtime fall
   back to whatever it actually ships with, so if a future version changes a default the baseline arm
   follows it — which is exactly the regression this lab exists to catch. `render.Verify` re-parses
   its own output and refuses a baseline that still declares a tunable.
4. **Never hand-edit a campaign directory.** If a number looks wrong, re-run. Editing results
   destroys the only thing that makes them worth publishing.
5. **Interleave, always.** Arms rotate within each repetition, so no arm systematically occupies the
   position that inherits the most thermal and socket state. `order: blocked` exists but requires a
   stated `orderReason`, which the report prints in red. The old harness ran all of A then all of B,
   which confounded every published "Gain" with time-in-session.
6. **Five repetitions, median with its spread, keep every one.** There is no code path in this
   harness that prints a point estimate without its dispersion and its n. The old index published 61
   rows of bare medians.
7. **A delta inside the noise band is not a result.** It is labelled noise and shown with the band it
   was measured against.
8. **Report cost, not just speed.** Throughput without CPU-ms/request and peak RSS is incomplete. The
   question is never "how fast" alone — it is "how fast, for what".
9. **Nothing runs in the hot path that is not part of the scenario.** No `log` blocks inside the
   measured flow unless logging is the thing being measured.
10. **An absent measurement is not a zero.** Every optional number in the result model carries its own
    `Present` flag. `dropped_iterations` is missing from a k6 summary when it is zero *and* when the
    field moved; a histogram lookup that matches nothing looks exactly like a runtime that served no
    traffic. Both have cost this lab a day.

## Running a campaign

[docs/RUNNING.md](docs/RUNNING.md) is the operator's guide. The short form:

```bash
task build                                            # build ./harness into ./bin
perf plan --campaign campaigns/regression-050-vs-060.yaml   # review before it runs
perf run  --campaign campaigns/regression-050-vs-060.yaml
```

`perf plan` prints the exact ordered cell list, the rate each scenario will use, and the estimated
wall clock. An eight-hour campaign gets reviewed as a plan rather than discovered as a mistake.

`perf run` writes `plan.json` before anything starts, then a directory per cell, then
`campaign.json` and `report.html`. Ctrl-C stops at a cell boundary. It takes hours — run it in the
background and poll rather than blocking an interactive session on it.

**On the real topology**, the subject is another machine:

```bash
perf run --campaign campaigns/regression-050-vs-060.yaml \
  --subject-ssh perf@10.20.0.3 --subject 10.20.0.3 \
  --subject-dir /srv/perf --versions /srv/perf/octo-versions \
  --deps-ssh perf@10.20.0.4 --deps 10.20.0.4
```

Everything above `internal/exec.Runner` is unchanged by that flag: the same procedure runs against a
local subject in a `go test` in seconds, and against a VM for a real result. The fast loop therefore
tests the slow one.

## The campaign spec is the experiment

```yaml
name: regression-050-vs-060
question: "Did 0.6.0 regress against 0.5.0 on any scenario?"

scenarios: [001-template-page, 002-fanout-transform]
reps: 5

arms:
  - name: "0.5.0"
    binary: { version: "0.5.0" }
    config: { mode: baseline }
  - name: "0.6.0"
    binary: { version: "0.6.0" }
    config: { mode: baseline }
```

Intent lives in a checked-in file, not in a directory name. The old lab encoded it as free text in
run ids — `-mON`, `-probesonly`, `-metricson` — decoded nowhere except four lines of a log.

A sweep is `reps: 1` with an arm per grid point. A capacity probe is the calibration phase. A VU ramp
is the closed model with an arm per level. There are no other programs.

## Rates are measured, never remembered

A scenario says `calibrate: true` and declares a ramp. Each campaign opens with a capacity probe per
scenario, finds the knee, and runs **every arm at the same measured fraction of it** — so an arm that
cannot hold the rate produces a saturation finding rather than a quietly lower number.

The old lab carried `STEADY_RATE=16000`, calibrated once on a laptop on 2026-07-25. By the next day
every cell at that rate was shedding 260,000–630,000 iterations. A rate calibrated once is a constant
in the code and a variable in reality.

Scenario 005 declares a fixed rate on purpose, and says why in the file: its result *is* that the two
arms cannot both hold 800 req/s.

## Validity is a value, not a warning

Every cell gets a verdict. Invalid cells are excluded from every aggregate and listed, struck
through, in the report's validity ledger — visible, never silently dropped.

Gates ship in **observe mode**: evidence is recorded unconditionally, nothing escalates past
`suspect`, and the report carries a gate-calibration table showing how often each fired. Thresholds
tighten from that data. Nobody knows the right runner-CPU ceiling yet, because the old lab never
measured one.

## Adding a scenario

1. `scenarios/<nnn>-<slug>/README.md` — describe the integration: what it exercises, which connectors
   and blocks, what the request and response look like, why it is interesting. The methodology
   requires the integration to be described, so this is not optional.
2. `octo/integration.yaml` — the flow, with knobs as **plain integers** so the archived config
   records the value that ran. Declare each knob in exactly **one** place, or the renderer will set
   every occurrence together. A knob that is not a root-flow `workers`/`buffer`/`pool` needs an
   explicit `path:` on its tunable.
3. `scenario.yaml` — `route`, `readyRoute`, `request`, `tunables`, `load`, `capacity`, `thresholds`,
   and the `calibration:` block explaining why the rate is what it is. In the old lab that reasoning
   was the most valuable line in `scenario.env` and existed only as a shell comment, so it never
   reached a report.
4. Dependencies go under `deps:` with executable `setup.sh`/`teardown.sh`. Write addresses as
   `${DEPS_HOST}`; the harness substitutes it from the topology and hands the same string to the
   script and to the runtime. Hard-coding `localhost` is how `host.docker.internal` happened.
5. If the flows need CEL functions an older runtime lacks, declare `requiresCel:` — one expression
   that must **compile and return true**, so a function that is present but behaves differently fails
   as loudly as one that is absent. It is asked of the artifact, never inferred from a version
   string: a source build reports the same constant as the release it branched from.
6. `go test ./internal/spec/` — every shipped scenario is loaded, validated, and its payload built.

## What the runtime actually gives us

Corrected from an earlier version of this file, which asserted the opposite and was wrong for two
releases:

- **0.5.0 added an admin port.** `--observability-addr` serves `/healthz`, `/readyz` and — with
  `--metrics` — `/metrics`. There is no `/livez`.
- **0.6.0 moved block events inline.**
- **0.4.2 and 0.4.3 have neither flag**, and passing `--metrics` to them is a hard parse failure.
  Capabilities are therefore probed against the artifact, never inferred from the version string, and
  the raw help text is archived per cell.
- **`octo --config <dir>` loads every config in that directory**, so two variants side by side would
  both load and collide on the port. Each cell stages exactly one.
- **`workers`, `buffer` and `pool` are root-flow only.** Sub-flows inside composite blocks inherit
  from the parent and cannot declare their own.
- **The flow-duration histogram's lowest bucket edge is 5 ms**, and most flows finish in
  microseconds. A quantile inside that bucket is a *bound*, not a value, and `promx.Quantile` returns
  it as one rather than interpolating an invented number.

## Findings workflow

Two stages, deliberately separate:

1. **Observe → Notion.** Anything surprising — a bug, a missing capability, an odd curve, a
   question — becomes a dated bullet in the
   [Performance Benchmarking](https://app.notion.com/p/juancavallotti/Performance-Benchmarking-3a88c36eda30803ab07dde64a2b38a1c)
   page. Include the scenario, the version, and a link to the evidence. This page is for thinking, so
   low-confidence observations belong there too.
2. **Decide → GitHub.** Once we decide to act, open an issue on
   [`juancavallotti/octo`](https://github.com/juancavallotti/octo) with `gh issue create`, then
   back-link it into the Notion bullet. Notion is the log; GitHub is the commitment.

Do not open GitHub issues speculatively from a benchmark observation. The Notion stage exists so that
judgement happens first.

## Comparing against other runtimes

[COMPARISON.md](COMPARISON.md) is the authority. The short form:

1. **Load model is not a detail.** Published benchmarks are almost always closed-model, and the
   "knee point" is an artifact of that model. This lab is open-model except where a spec says
   `model: closed`, which exists for exactly this purpose. The model is *recorded* per run, never
   inferred, so the two can never end up in one table.
2. **Match the scenario before claiming anything.** A headline "routing latency" is usually
   in-process, not an end-to-end HTTP request.
3. **Record what could not be built.** Ordinary integration workloads with no Octo equivalent belong
   in COMPARISON.md — they are findings, not omissions.
4. **Describe workloads, not vendors.** A scenario is justified by what it exercises in the runtime —
   blocking I/O, collection mapping, fan-out — not by who else measured something similar.

## Changing the harness

- Every `internal/` package has a `doc.go` stating **one** responsibility. A package whose `doc.go`
  cannot is the wrong package.
- The pure packages — `series`, `promx`, `stats`, `spec`, `plan`, `render`, `gate`, `payload` — carry
  the correctness burden. A bug there corrupts a number instead of crashing.
- `report` formats and derives nothing. That rule is what kept the old `report.py` from happening
  twice: it reached a thousand lines and became the only place several published numbers were
  computed.
- A new failure mode appends a row to [docs/LEARNINGS.md](docs/LEARNINGS.md) **and** a test. That is
  the rule, and 26 rows say it has been followed.

## Probing by hand

Ad-hoc measurement outside the harness is fine for forming a hypothesis and is **not** a result.
Two non-negotiables, both learned the hard way in one sitting:

- **Assert the port is free before starting, and that the runtime became ready.** A stale process
  holding 8080 does not fail loudly; it answers 404 quickly, which reads as excellent throughput.
- **Read `http_req_failed` and `dropped_iterations` before reading `http_reqs`.** A run that failed
  every request reports a throughput number like any other.

Anything worth publishing gets re-measured through a campaign.
