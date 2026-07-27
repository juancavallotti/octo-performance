# Running the tests

Three things are called "tests" in this repository and they are not the same thing. Read the one you
want.

| I want to… | Go to |
|---|---|
| check the harness itself works | [1. The harness test suite](#1-the-harness-test-suite) — seconds |
| check every scenario still runs | [2. The smoke campaign](#2-the-smoke-campaign) — ~6 minutes |
| get a number I can act on | [3. A real campaign](#3-a-real-campaign) — hours |
| get a number I can publish | [4. On real hardware](#4-on-real-hardware) — hours, plus a `terraform apply` |

---

## Prerequisites

```sh
brew install k6 go-task terraform
go version        # 1.26.4 or later
```

The releases under test live in `~/.octo-versions/octo-<version>`:

```sh
ls ~/.octo-versions/
# octo-0.4.2  octo-0.4.3  octo-0.5.0  octo-0.6.0
```

Scenario 003 needs Docker running. Scenario 005 needs the Go toolchain (it builds `lab/backend`).

---

## 1. The harness test suite

No runtime, no network, no VM. This is the loop to stay in while changing the harness.

```sh
task test
```

`go vet` plus `go test -race ./...` across 18 packages. It runs a real `k6` against a real
fake-octo process, so it covers the parsers whose input contract nobody controls — but it finishes
in under a minute.

It fails loudly if `k6` is missing rather than skipping, because a quietly skipped parser test is
the same as no parser test.

---

## 2. The smoke campaign

Every scenario, both arms, eight seconds a cell. **This is a plumbing check, not a measurement** —
one repetition at a fixed low rate on a machine that is also running the load generator. Every
number it produces is inside its own noise, and the report says so.

```sh
task smoke
```

What it proves: each scenario's config renders, its dependencies come up, its payload is accepted
by its route, the runtime becomes ready, and the harness can find the flow metrics afterwards.

Run it after touching a scenario, an integration, or anything in `internal/campaign`.

---

## 3. A real campaign

### Review the plan first

```sh
task plan CAMPAIGN=campaigns/regression-050-vs-060.yaml
```

This prints the exact ordered cell list, the rate each scenario will use, and the estimated wall
clock. **An eight-hour campaign gets reviewed as a plan, not discovered as a mistake.**

Check three things before you run it:

1. **The order.** `A B B A A B…` — each arm should occupy each position equally. If it says
   `blocked`, the report will print your stated reason in red, and every delta is confounded with
   time-in-session.
2. **The rates.** `(calibrated)` means a capacity ramp will measure it. `(fixed)` means the spec
   declared it, and the spec should say why.
3. **The wall clock.** It is a floor: load plus warm-up plus cooldown, excluding calibration,
   start-up and shutdown. Real elapsed time runs 30–50% higher.

### Run it

```sh
task run CAMPAIGN=campaigns/regression-050-vs-060.yaml
```

It takes hours. **Background it and poll** rather than blocking a terminal:

```sh
task run CAMPAIGN=campaigns/regression-050-vs-060.yaml > /tmp/campaign.log 2>&1 &
tail -f /tmp/campaign.log
```

`Ctrl-C` stops at the next cell boundary, not mid-cell, and still writes a report from whatever
completed. A campaign cut short answers what it managed to measure and the ledger says how much
that was.

Output lands in `campaigns/out/<date>-<name>-<planhash>/`:

```
plan.json                what it set out to do, written before anything ran
calibration.json         how each rate was chosen
calibration/<scenario>/  the capacity ramp: k6 summary, series, the runtime's log
cells/<scenario>__<arm>__rep<n>/
    cell.json            everything measured, complete
    verdict.json         which gates fired and on what evidence
    config.render.json   what changed in the config, where, and from what
    series.csv           the 1 Hz series
    metrics.ndjson       every /metrics scrape
    argv.json  help.txt  octo.log
campaign.json            the machine-readable roll-up
report.html              ← the thing to read
```

### Write your own campaign

Copy `campaigns/regression-050-vs-060.yaml` and change the arms. The minimum:

```yaml
name: my-question
question: "The sentence the report should answer."

scenarios: [001-template-page]
reps: 5

arms:
  - name: "0.5.0"                      # the first arm is the baseline
    binary: { version: "0.5.0" }
    config: { mode: baseline }
  - name: "0.6.0"
    binary: { version: "0.6.0" }
    config: { mode: baseline }
```

Other shapes, all the same program:

```yaml
# A tuning sweep: one rep, an arm per grid point.
reps: 1
arms:
  - { name: "w8",  binary: {version: "0.6.0"}, config: {mode: tuned, knobs: {workers: "8"}} }
  - { name: "w64", binary: {version: "0.6.0"}, config: {mode: tuned, knobs: {workers: "64"}} }
```

```yaml
# Measure a local build against a release.
arms:
  - { name: "0.6.0", binary: {version: "0.6.0"}, config: {mode: baseline} }
  - { name: "dev",   binary: {path: "/Users/me/src/octo/bin/octo"}, config: {mode: baseline} }
```

```yaml
# Skip calibration and pin the rate, e.g. to reproduce an earlier campaign.
load:
  rate: 8000
  duration: 60s
```

Naming a rate in the campaign displaces the scenarios' `calibrate: true`, so no ramp runs.

### The A/A control — run this before trusting anything

Put the **same version in both arms**. Every scenario must come back `noise`. Any scenario
reporting a significant delta means the harness is measuring itself.

```yaml
name: aa-control
question: "Does the harness report a difference where there is none?"
scenarios: [001-template-page]
reps: 5
arms:
  - { name: "a", binary: {version: "0.6.0"}, config: {mode: baseline} }
  - { name: "b", binary: {version: "0.6.0"}, config: {mode: baseline} }
```

---

## 4. On real hardware

Everything above works on a laptop and **none of it produces a publishable throughput number**,
because the load generator shares the subject's cores. That is not a constant tax that cancels
between arms: k6 grows its virtual-user pool, the extra goroutines take cores from the runtime, the
runtime slows, the pool grows further. The same binary gave 15,996 req/s and 6,939 req/s on
consecutive days.

```sh
cd infra/terraform
terraform init
terraform apply \
  -var project=my-gcp-project \
  -var "ssh_public_key=$(cat ~/.ssh/id_ed25519.pub)"

terraform output run_command      # the invocation these machines were built for
```

This needs `gcloud auth application-default login` and a project id, neither of which is configured
in this checkout. The first `apply` will say so.

Then stage the binaries on the subject and run from the runner:

```sh
scp ~/.octo-versions/octo-0.6.0 perf@<subject-ip>:/srv/perf/octo-versions/

perf run --campaign campaigns/regression-050-vs-060.yaml \
  --subject-ssh perf@<subject-ip> --subject <subject-ip> \
  --subject-dir /srv/perf --versions /srv/perf/octo-versions \
  --deps-ssh perf@<deps-ip> --deps <deps-ip>
```

And when it is done: `terraform destroy`. The campaign directory is the artifact; the machines are
not.

**A note on the flags.** `--subject-ssh` is where octo *runs*; `--subject` is the address the runner
*reaches it on*. They are separate because they can differ, and the harness refuses `--subject
127.0.0.1` alongside `--subject-ssh` — loopback on the runner is the runner, so the subject would
come up, answer nothing, and every cell would fail readiness.

---

## Reading the report

`report.html` is one self-contained file. No network, no assets — it renders from a copied
directory in five years.

Read it in this order:

1. **The sentence at the top.** "No regression was found" and "nothing could be established" are
   different statements and only the first is a result. A campaign whose comparisons all declined
   for want of evidence says so explicitly, and that is what a two-repetition run produces.
2. **The warnings under it.** "The generator shared a host with the subject" qualifies every number
   below it.
3. **The validity ledger.** Excluded cells are struck through and contributed to nothing. If most
   cells are excluded, the headline is built on very little.
4. **How each rate was chosen.** A measured knee and a declared rate mean different things, and a
   ramp that held every rung established a *lower bound*, not a knee.
5. **The regression matrix.** Every delta carries the noise band it was measured against. A delta
   inside its band is labelled noise and is not a result however large the percentage looks.
6. **Client against server latency.** The gap is queueing at the generator, not work in the runtime.
   A large gap means the client-side number is measuring k6.
7. **Cost.** A config that improves latency while burning materially more CPU per request has
   traded, not won.

---

## When something goes wrong

| Symptom | Cause |
|---|---|
| `port 8080 is already held` | A subject from an interrupted run. `lsof -ti:8080 \| xargs kill` |
| every cell fails readiness | Wrong `--subject` address, or the config did not stage. Read `cells/*/octo.log`. |
| `the ramp established no rate` | The capacity ramp failed its first rung. Read `calibration/<scenario>/calibration.json` — the rung table names the reason. |
| every cell is `suspect: window.steady-state` | On macOS, expected: there is no `/proc`, so the subject's CPU cannot corroborate the window and the report says the window was accepted on throughput alone. On Linux it should disappear. |
| `loadgen.saturated` on scenario 005 baseline | Correct. That arm caps at ~108 req/s by design, and the saturation *is* the result. |
| the report says a delta is noise and you expected a result | Read the band. Five repetitions on a laptop rarely resolve less than a few percent. |
| `0 comparisons` | Fewer repetitions than the minimum-n rule requires. Raise `reps`. |

Anything that turns out to be a harness bug gets a row in [LEARNINGS.md](LEARNINGS.md) **and** a
test. That is the rule, and twenty-six rows say it has been followed.
