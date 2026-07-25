# 005 — HTTP proxy

The shape a commercial platform benchmarks first and calls "one of the most common use cases for
the platform": a request arrives, is passed to a backend that takes 70 ms to answer, and the
response goes back to the caller.

This is the lab's first scenario built deliberately to be *comparable to somebody
else's published numbers* rather than only to Octo's own. See
[COMPARISON.md](../../COMPARISON.md).

## The integration

```
GET /proxy/{id}
  └── rest ──▶ http-client "backend" ──▶ GET /payload?size=1024&delay=70ms
```

One block. Nothing else is in the hot path, on purpose: what is being measured is
the runtime's ability to hold many requests in flight while they wait on somebody
else, which is most of what an integration runtime actually does.

The backend is [`lab/backend`](../../lab/backend/) — a small Go server that sleeps
for the requested duration and returns a pre-generated payload of the requested
size. It sleeps rather than works so that it competes with the runtime for as little
CPU as possible on a shared host.

| | |
|---|---|
| Connectors | `http` (source), `http-client` (backend) |
| Blocks | `rest` |
| Knobs | `workers`, `buffer`, `pool` |
| Backend delay | 70 ms (a commercial platform's figure) |
| Payload | 1 KB (their headline chart); 1 MB via `BACKEND_SIZE=1048576` |

## Why this scenario is different

Every other scenario in this lab is CPU-bound: a request occupies a worker for as
long as it takes to compute, which is microseconds. Here a request occupies a worker
for **70 ms of doing nothing**. That single change turns `workers` from a
second-order tuning knob into the number that decides throughput outright:

```
ceiling ≈ workers / seconds_blocked_per_request = 8 / 0.070 ≈ 114 req/s
```

Measured on `m1pro-16gb` / Octo 0.4.3, closed model, 1 KB payload:

| Concurrency | Out of the box (`workers` unset) | | |
|---|---|---|---|
| | throughput | p50 | p95 |
| 8 VUs | 107.5 req/s | 74 ms | 79 ms |
| 50 VUs | 107.8 req/s | 445 ms | 522 ms |
| 200 VUs | 108.7 req/s | 1.83 s | 1.85 s |

Throughput is **flat to three significant figures** across a 25× increase in offered
concurrency, while latency rises in exact proportion. That is the signature of a
hard concurrency limit, and it matches the arithmetic above to within 6%.

Raising `workers` moves it:

| `workers` | Throughput at 200 VUs | p50 | vs default |
|---|---|---|---|
| default (8) | 108.7 req/s | 1.83 s | — |
| 64 | 857 req/s | 224 ms | 7.9× |
| 512 | **1,779 req/s** | 72.9 ms | **16.4×** |
| 2048 | 1,432 req/s | 81.2 ms | 13.2× |

At `workers: 512` the p50 is 72.9 ms — the backend's own 70 ms plus 3 ms. The queue
is gone; requests are no longer waiting for a worker, they are only waiting for the
backend. Going further to 2048 costs throughput back, so the knob has an optimum
rather than a direction.

**Read this against the runtime's own documentation, which gives `workers` as
"consumers pulling from the flow's message channel" with a default of 8.** Nothing
about that description suggests it is also the maximum number of simultaneous
outbound HTTP calls the flow can have in flight — but for a blocking flow, it is.

## A second finding: outbound connections are not being reused

At `workers: 512` the flow reaches 1,779 req/s. With 200 virtual users and a 72.9 ms
service time, Little's law says the ceiling should be about
`200 / 0.073 ≈ 2,740 req/s`. The missing 35% is not workers, and it is not CPU.

Counting sockets to the backend during a fixed 2,000-request run:

| Concurrency | New backend TCP connections per proxied request |
|---|---|
| 20 VUs | 0.66 |
| 200 VUs | ~1.0 |

At 200 VUs the runtime opens **roughly one new TCP connection for every request it
proxies** — 11,531 sockets in `TIME_WAIT` accumulated in six seconds of load. Reuse
gets worse as concurrency rises, which is the signature of a small fixed idle-
connection pool: Go's `http.Transport` keeps `MaxIdleConnsPerHost: 2` by default, so
above two concurrent calls to the same host the surplus connections are closed
rather than parked.

That diagnosis is an inference from the shape of the data — the runtime's source has
not been read to confirm it — but the measurement is not in doubt, and the
`http-client` connector exposes no setting that would change it either way
(`baseURL`, `timeout`, `headers`, `maxResponseBytes`, `auth`, `retry`).

Why it matters beyond this benchmark:

- **Ephemeral port exhaustion.** 11.5k sockets in six seconds against a default
  ephemeral range of roughly 16k–28k ports means a sustained proxy workload runs out
  of source ports in well under a minute, then fails in a way that looks like the
  backend is down.
- **TLS.** This backend is plaintext. Against an HTTPS backend, one connection per
  request means one TLS handshake per request — an extra round trip and a
  significant CPU cost per call, neither of which appears here.

## Running it

```bash
task smoke SCENARIO=005-http-proxy
task bench SCENARIO=005-http-proxy TUNED_WORKERS=512 TUNED_BUFFER=1024 TUNED_POOL=8

# The closed-model sweep, for comparison against published vendor curves
task vuramp SCENARIO=005-http-proxy VARIANT=baseline
task vuramp SCENARIO=005-http-proxy VARIANT=tuned TUNED_WORKERS=512

# a commercial platform's 1 MB streaming comparison
BACKEND_SIZE=1048576 task bench SCENARIO=005-http-proxy
```

`setup.sh` builds and starts the backend before the measured window; `teardown.sh`
stops it. Both need the Go toolchain.

## Caveats specific to this scenario

- The backend shares a host with the runtime and the load generator. a commercial platform put
  all three on separate EC2 instances. The backend sleeps rather than works,
  which keeps the contention small, but it is not zero.
- The steady-state rate of 800 req/s is chosen so that the two arms **cannot both
  pass**. The baseline arm's dropped iterations are genuine server saturation — no
  VU pool can sustain 800 req/s against a hard 108 req/s ceiling — not the
  generator artifact documented in [METHODOLOGY.md](../../METHODOLOGY.md).
