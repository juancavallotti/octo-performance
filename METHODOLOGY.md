# Methodology

Every published result in this repository follows the same shape. This document defines that
shape, the metrics, and — just as importantly — what the numbers do *not* mean.

## The five things every result states

1. **The hardware.** Captured automatically into `env.json`: CPU model, core topology
   (performance/efficiency split on Apple silicon), RAM, OS version, and for the container target
   the CPU/memory budget the container runtime was actually given.
2. **The integration under test.** Described in the scenario's `README.md`: which connectors and
   blocks it exercises, the request shape, the response shape, and why it is worth measuring.
3. **The out-of-the-box result.** Octo run with no tuning at all.
4. **The optimized result.** The same integration with `workers`, `buffer`, and `pool` chosen
   deliberately, usually via `make sweep`.
5. **The cost.** CPU and memory consumed to reach that throughput, plus the runtime's static
   footprint.

And underpinning all of it: **the version under test**, so that a future run can be compared
against this one and a regression is visible.

## Variants

| Variant | Config | Meaning |
|---|---|---|
| `baseline` | `octo/baseline.yaml` | No `workers`, `buffer`, or `pool` keys present. What a user gets by default. |
| `tuned` | `octo/tuned.yaml` | Same flow, with the three knobs set explicitly. |

Octo's documented defaults are `workers: 8`, `buffer: 64`, `pool: 8`. `tuned.yaml` declares those
same values as its `env` defaults, which gives a free **control run**: tuned with no environment
overrides should be statistically indistinguishable from baseline. If it is not, the
parameterisation itself is costing something and the comparison is compromised.

The harness diffs the two configs and aborts if they differ anywhere other than the tuning knobs
and their `env` declarations.

## Targets

| Target | What it is |
|---|---|
| `native` | The standalone distribution — a single Go binary run directly on the host. |
| `docker` | The published runtime image `juancavallotti/octo-runtime`, config bind-mounted at `/etc/octo/integrations`. |

Both are benchmarked because both are how people actually deploy. They are **not** directly
comparable to each other on macOS (see Caveats).

## Tests

All three are k6 scripts sharing `lab/k6/lib/`.

**`smoke`** — 1 VU, ~30 iterations. Asserts status 200, `Content-Type: text/html`, and that
template interpolation actually happened (the path parameter appears in the body). This is a
correctness gate, not a measurement. A load run whose smoke did not pass is discarded.

**`capacity`** — `ramping-arrival-rate`, climbing request rate until p95 latency crosses the
threshold or k6 starts dropping iterations. Locates the knee of the curve, which is how the
steady-state rate for a scenario gets chosen.

**`steady`** — `constant-arrival-rate` at a fixed offered rate for a fixed duration. This is the
headline comparison.

The arrival-rate executors are an **open model** and that choice is deliberate. Under a closed
model (fixed VUs looping), a slow server simply receives fewer requests, and throughput
self-limits into a number that looks stable while hiding the problem. Under an open model, load is
offered at a fixed rate regardless of how the server is coping, so degradation surfaces honestly
as rising latency and non-zero `dropped_iterations`.

Load tests set `discardResponseBodies: true` to keep the generator cheap; k6 still accounts for
bytes received. Smoke does not, because it inspects the body.

## Run procedure

Per variant, per repetition:

1. Stage exactly one config variant into a clean directory.
2. Start the target. Poll the real route until it returns 200 — that latency is recorded as
   **cold start**.
3. Run a warm-up k6 pass and discard it. This pays for Go's runtime warm-up, connection
   establishment, and any lazy initialisation, so the measured window is steady-state.
4. Begin 1 Hz resource sampling of the server process (or container).
5. Run the k6 test.
6. Stop sampling, stop the target, capture whole-process CPU and peak RSS totals.

`REPS=3` by default. The report shows the **median** repetition by achieved throughput; every
repetition's artifacts are kept so the spread can be inspected.

## Metrics

### Throughput and latency (from k6)

| Metric | Definition |
|---|---|
| Offered rate | Requests per second k6 was instructed to generate. |
| Achieved RPS | `http_reqs / duration`. Below the offered rate means the server could not keep up. |
| `dropped_iterations` | Iterations k6 could not start because the VU pool was saturated. **Non-zero means the result is a saturation measurement, not a latency measurement.** |
| `http_req_duration` | p50 / p90 / p95 / p99 / max. Full client-observed request time. |
| `http_req_waiting` | Time to first byte after the request was written — the closest available proxy for server-side think time, since it excludes DNS, connect, and TLS. |
| `http_req_failed` | Error rate. Any non-zero value on this workload demands an explanation before the run is published. |
| `data_received` | Throughput in bytes/s, a sanity check that responses are the expected size. |

### Resource utilisation (OS-level, server side)

CPU is measured by differentiating cumulative process CPU time between samples rather than reading
an instantaneous percentage. On macOS, `ps -o %cpu` reports a decaying average over the process
lifetime, which would systematically understate a short benchmark window; `cputime` deltas give
exact CPU-seconds consumed per interval. Whole-run totals come from `/usr/bin/time -l` (macOS) or
`-v` (Linux), which also yields an authoritative maximum RSS.

For the container target the equivalent data comes from `docker stats`.

| Metric | Definition |
|---|---|
| Total CPU-seconds | `user + sys` for the whole server process lifetime. |
| Mean / peak CPU % | From the 1 Hz series. 100% = one fully saturated core. |
| Mean / peak RSS | Resident set size from the same series, plus the authoritative peak. |
| RSS drift | Last sample − first sample. Sustained positive drift across reps is a leak signal. |

### Derived efficiency numbers

These are the point of the exercise. Throughput alone says nothing about whether it was bought
cheaply.

- **CPU-ms per request** = `total_cpu_seconds × 1000 / http_reqs`. The headline efficiency number
  and the most portable one across hardware.
- **RSS per 1k RPS** = `peak_rss / (achieved_rps / 1000)`. How much memory a unit of throughput costs.
- **Requests per CPU-core-second** = `http_reqs / total_cpu_seconds`. The reciprocal view, useful
  when reasoning about capacity planning.

### Runtime footprint

Measured once per target per run, independent of variant — the standing cost of the runtime before
it serves anything.

| Metric | Definition |
|---|---|
| Artifact size | Binary size on disk, or image size and digest for the container target. |
| Cold start | Process/container start until the route first answers 200. |
| Idle RSS | Resident memory after a 30 s idle hold post-readiness. |
| Idle CPU % | Mean CPU over that same idle hold. Should be ~0; anything else is a finding. |

## Cooldown between runs

Every measured run is followed by a cooldown (`COOLDOWN_SECONDS`, default 15) before
the next one starts. This is not a courtesy — it is load-bearing.

A minute at 24,000 req/s leaves on the order of a million sockets working through
`TIME_WAIT`, and a laptop chassis that has been sitting at 300% CPU is thermally a
different machine from a cold one. A run that starts in that state inherits both,
and the resulting ordering artifacts are large — easily large enough to look like a
real difference between configurations that are in fact identical.

Two defences, both mandatory:

1. **Cool down between runs**, so each starts from a comparable state.
2. **Alternate and repeat** when comparing two configurations, rather than running
   all of A then all of B. A difference that survives interleaving is a difference;
   one that tracks position in the sequence is an artifact.

This is also why a single run is never a result: see repetitions and medians above.

## Caveats

Stated plainly, because a benchmark that hides its limitations is marketing.

- **Docker on macOS is not measuring only Octo.** Docker Desktop runs containers inside a Linux VM
  and publishes ports through a userland proxy. The container target therefore measures that
  network path as much as it measures the runtime. Baseline↔tuned within the container target is
  still perfectly valid — the proxy is constant across both. Native-vs-container is indicative
  only.
- **`docker stats` understates true host cost.** It accounts for the container's own usage and not
  the Docker Desktop VM overhead required to run it. This is why the container sometimes reports a
  *lower* CPU-ms/request than native (scenario 002: 0.437 against 0.485; scenario 006: 0.311
  against 0.347). That is an accounting boundary, not an efficiency win — the port proxy's work is
  real and simply falls outside the cgroup being measured.
- **A containerised runtime reaches host dependencies by a longer road, and it shows.** Scenarios
  003 and 005 talk to something on the host, so the container has to address it as
  `host.docker.internal`: out through the VM's NAT, onto the host, and back in through a published
  port. For a flow doing three database round trips per request that path dominates. Scenario 003
  holds 1,500 req/s at a 1.3 ms p95 natively and cannot hold it at all in a container
  (1,270 req/s, 4,147 ms p95), and raising `workers` there makes it *worse* rather than better,
  because the extra concurrency piles onto the constrained path rather than onto Postgres.
  **Read that as a property of this measurement setup, not of the runtime.** The comparable
  arrangement is container-to-container on a shared Docker network, which the harness does not yet
  do. Until it does, cross-target comparison for scenarios with host-side dependencies (003, 005)
  is not merely indicative — it is misleading, and the container column for 003 should be ignored.
- **The load generator shares the host with the server.** k6 and Octo compete for the same cores.
  This is a recorded known limitation, not a controlled variable. It compresses the absolute
  ceiling; it does not invalidate baseline↔tuned comparison, since both variants pay it equally.
  Splitting the generator onto a separate machine is supported by design (`BASE_URL` and host
  profiles) but not yet exercised.
- **Apple silicon is heterogeneous and thermally variable.** The M1 Pro has 8 performance and 2
  efficiency cores, and sustained load on a laptop drifts as the chassis heats. Hence repetitions
  and medians, and hence a preference for comparing runs captured in the same session.
- **These are single-instance numbers.** One Octo process serving one integration. Nothing here
  says anything about horizontal scaling or the Kubernetes platform deployment.

## Reading a report

`REPORT.md` leads with the environment and version under test, then the baseline↔tuned comparison
table, then resource cost, then footprint. When scanning:

1. Check `dropped_iterations` and `http_req_failed` first. If either is non-zero, the latency
   numbers describe a saturated system and the rest of the table means something different than it
   appears to.
2. Check achieved RPS against offered rate. A gap means saturation regardless of what the latency
   percentiles say.
3. Only then read latency, and read p95/p99 rather than the mean.
4. Finally read CPU-ms/request. A tuned config that improves latency while burning materially more
   CPU per request has traded, not won.
