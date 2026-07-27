# Replay corpus

Three real cells from the laptop lab, kept so the gates are tested against the failure modes they
exist to catch rather than against synthetic data someone invented to make them pass.

All three are scenario `001-template-page`, `native` target, `baseline` variant, offered rate 16,000
req/s for 60 s, load generator colocated with the subject. Two of them are the *same binary*.

| Specimen | Source run | Achieved | p95 | dropped | `vus_max` | Required verdict |
|---|---|---|---|---|---|---|
| `clean/` | `2026-07-25 … native-v0.4.3` rep1 | 15,999 req/s | 0.859 ms | 0 | 1,600 | **Valid** |
| `collapsed/` | `2026-07-26 … native-v0.4.3` rep1 | 6,938 req/s | 1,359.01 ms | 535,026 | 7,113 | **Invalid** |
| `metricson/` | `2026-07-26 … native-v0.5.0-metricson` rep2 | 9,897 req/s | 933.01 ms | 358,033 | 6,794 | **Invalid** |

## What each one is for

**`clean/` and `collapsed/` are the same file.** `~/.octo-versions/octo-0.4.3`, one day apart:
2.3× on throughput, 1,580× on p95. No version delta can explain it and none was involved. The
discriminator is `vus_max` — 1,600 is k6's pre-allocated floor (Little's law on
`EXPECTED_LATENCY_MS=25` at 16,000 req/s), 7,113 means k6 grew its pool 4.4× and put thousands of
extra goroutines on the same ten cores as the subject. That is a feedback loop: more VUs, less CPU
for the server, higher latency, more VUs.

A gate suite that passes `collapsed/` is broken, no matter what else it does.

**`metricson/` carries server-side ground truth**, and it is the only cell on disk that does. Its
`/metrics` exposition puts the runtime's mean flow duration at 0.41 ms while k6 reported a 933.01 ms
p95. The ~2,270× gap is entirely queueing inside the load generator. The published
`results/index.md` printed the 933.01 ms as Octo's p95.

It also exercises the histogram-bound path: `octo_flow_duration_seconds` uses Prometheus default
buckets whose lowest edge is 5 ms, and this flow completes in microseconds, so every observation
lands in the first bucket. Any quantile derived from it must come back as `< 5 ms`, never as an
interpolated number. `internal/promx/testdata/` holds the same two exposition snapshots for the
parser's own tests.

## Rules

- **Do not regenerate these.** The lab that produced them is being deleted; they cannot be
  reproduced.
- **Do not "fix" a specimen to make a test pass.** If a gate disagrees with the table above, the
  gate is wrong.
- Fields are the k6 `handleSummary` shape from `lab/k6/lib/summary.js` and the 1 Hz sampler CSV from
  `lab/bin/sample-resources.py`. The new harness writes different files; loaders for this corpus live
  in the test files and are deliberately separate from the production result schema, so the corpus
  never constrains the new format.

Provenance: commit `b18dabe`, "Keep the runs that show the harness measures itself, not the runtime".
