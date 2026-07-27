# octo-performance

A benchmarking lab for [Octo](https://juancavallotti.github.io/octo/) — a Go integration runtime
that executes flows defined in YAML.

The goal is boring and specific: produce **reproducible, version-stamped** performance numbers for
real integration scenarios, so that tuning is evidence-based and regressions between runtime
versions get caught.

Every result answers four questions:

- What hardware and what runtime version?
- What integration was under test?
- What does it do **out of the box**, and what does it do **tuned**?
- What did that throughput **cost** in CPU and memory?

## Quick start

```bash
brew install k6 go-task terraform             # prerequisites
task test                                     # the harness, no runtime needed, seconds

task plan CAMPAIGN=campaigns/regression-050-vs-060.yaml   # review before it runs
task run  CAMPAIGN=campaigns/regression-050-vs-060.yaml
```

A campaign writes one directory under `campaigns/out/`, and one **self-contained
`report.html`** inside it that leads with a sentence rather than a table. `task --list` shows
every entry point.

The experiment lives in a checked-in `campaigns/*.yaml`: which scenarios, which arms, how many
repetitions. A sweep is a campaign with one repetition and an arm per grid point; a capacity
probe is the calibration phase. There is one procedure, not one program per experiment shape.

## What gets measured

**Throughput and latency** via [Grafana k6](https://grafana.com/docs/k6/latest/using-k6/), using
open-model arrival-rate executors so that a struggling server shows up as latency and dropped
iterations rather than as quietly reduced load.

**Resource cost** via OS-level sampling of the server process — CPU-seconds, RSS, and the derived
numbers that actually matter: **CPU-ms per request**, RSS per 1k RPS.

**The runtime's own view.** Since 0.5.0 the runtime serves `/metrics`, and every scrape across a
cell is kept so the pair bracketing the measured window can be differenced. The report shows the
client's latency and the runtime's side by side — the comparison that would have caught a published
p95 of 933 ms sitting in the same directory as the runtime's own mean of 0.41 ms.

**Runtime footprint** — artifact size, cold-start time, idle memory, idle CPU. The standing cost of
the runtime before it serves a single request.

## Where it runs

| Machine | Runs |
|---|---|
| runner | the harness and k6, on hardware twice the subject's |
| subject | `octo`, and nothing else |
| deps | Postgres and the slow backend, for the two scenarios that need them |

The split is the whole point. With the load generator beside the runtime, the same binary produced
15,996 req/s and 6,939 req/s on consecutive days — k6 grows its virtual-user pool, the extra
goroutines take cores from the server, the server slows, and the pool grows further. It is a
feedback loop, not a constant tax, so it does not cancel out between two arms.

`infra/terraform` stands the three machines up; a campaign runs against them over SSH. The same
campaign runs entirely on a laptop through the same code path, which is a development loop rather
than a published result — and every report states which one it was.

## Tuning knobs

Octo exposes three concurrency settings on root flows, and they are exactly what the
baseline-vs-tuned comparison explores:

| Field | Default | Meaning |
|---|---|---|
| `workers` | 8 | Consumers pulling from the flow's message channel. |
| `buffer` | 64 | Channel depth before publishers block. |
| `pool` | 8 | Shared worker pool handed to concurrent composites such as `fork`. |

A campaign with an arm per grid point searches them, and the report says which won and by how much
against the noise band.

**When they matter.** Only once the flow blocks. On a CPU-bound flow ([001](scenarios/001-template-page/),
[002](scenarios/002-fanout-transform/)) tuning changes nothing measurable — a worker never
waits, so there is no queue to relieve. On a flow that makes three database round trips per
request ([003](scenarios/003-postgres-crud/)), raising `workers` from the default 8 to 64
took p95 from **4,134 ms to 25.7 ms** at the same offered rate, and turned a saturated system
into one running at the full requested throughput.

The rule that falls out: for a blocking flow, `workers` needs to be at least
`target_rps × seconds_blocked_per_request`. The default of 8 suits CPU-bound work and is
badly wrong for anything that waits on I/O.

## Scenarios

| Scenario | What it exercises |
|---|---|
| [001-template-page](scenarios/001-template-page/) | HTTP route rendering a template resource as an HTML page. The floor: HTTP source, path parameters, templating, no I/O. |
| [002-fanout-transform](scenarios/002-fanout-transform/) | `fork` fan-out, `multi-transform`, mapped `foreach`, and `flow-ref` composition. The first scenario that actually schedules work on `pool`. |
| [003-postgres-crud](scenarios/003-postgres-crud/) | Write, read back, and delete a row per request against containerised Postgres. The first flow that **blocks** — where `workers` meets `maxOpenConns`. |
| [004-queue-roundtrip](scenarios/004-queue-roundtrip/) | Two flows joined by an internal queue with `awaitReply`. The only scenario with a knob on **both** sides: producer `workers` against consumer `listeners`. |
| [005-http-proxy](scenarios/005-http-proxy/) | Pass a request to a backend that takes 70 ms — the canonical API-gateway shape. Out of the box this caps at **108 req/s**; tuned it reaches 1,780. |
| [006-json-transform](scenarios/006-json-transform/) | Reshape a JSON collection, two ways. Shows `foreach mode: map` is **quadratic** in record count. |
| [007-csv-transform](scenarios/007-csv-transform/) | The same records as 006, arriving as CSV. The first workload whose **format decoder is written in CEL** rather than compiled into the runtime. |

More scenarios — other connectors, database-backed flows, outbound REST calls, forked composites —
get added over time. See [AGENTS.md](AGENTS.md) for the checklist.

## Reading these numbers against other runtimes

[COMPARISON.md](COMPARISON.md) covers what has to match before two benchmark figures describe
the same thing — load model, transaction definition, envelope, generator placement — and what
this lab does about each.

Two things are worth knowing first:

- Published benchmarks are almost always **closed-model** (throughput against a fixed
  virtual-user population). This lab is open-model unless a spec says `model: closed`, and the
  model is recorded with every run so the two can never share a table.
- **Throughput becomes defensible only on the split topology.** Numbers produced with the
  generator beside the runtime are development output, and the report marks them as such.

Building those scenarios also produced a capability checklist: no CSV or XML parser, no policy
engine, no batch component, no Kafka or JMS connector. That says more about where Octo sits
than any throughput figure.

## Layout

```
harness/            the harness — one Go module, `go test ./...` covers it
harness/internal/   one package per responsibility, each with its invariants in doc.go
scenarios/          benchmark scenarios: scenario.yaml + octo/integration.yaml
campaigns/          checked-in campaign specs; the experiment's intent lives here
campaigns/out/      campaign output (gitignored)
infra/terraform/    the three machines a defensible campaign runs on
docs/               ARCHITECTURE.md and the failure ledger, LEARNINGS.md
```

## Documentation

- [METHODOLOGY.md](METHODOLOGY.md) — how runs are conducted, what each metric means, and what the
  numbers do not mean. Read before interpreting any result.
- [AGENTS.md](AGENTS.md) — the working contract: golden rules, how to add a scenario, and the
  findings workflow.
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — the topology, every interface, and the invariants
  the tests defend.
- [docs/LEARNINGS.md](docs/LEARNINGS.md) — the failure ledger. Twenty-six rows, each naming a
  failure this lab produced and the test that now prevents it. Read before changing the harness.
- [COMPARISON.md](COMPARISON.md) — what other runtimes have published, and what may honestly be
  concluded from it.

Published at **https://juancavallotti.github.io/octo-performance/**.

## Findings

Observations about Octo made while benchmarking are logged in a
[Notion page](https://app.notion.com/p/juancavallotti/Performance-Benchmarking-3a88c36eda30803ab07dde64a2b38a1c)
for thinking. Anything we decide to act on becomes an issue on
[`juancavallotti/octo`](https://github.com/juancavallotti/octo).
