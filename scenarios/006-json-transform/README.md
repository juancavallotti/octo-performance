# 006 — payload transformation

Payload transformation, the workload every integration runtime is measured on: an HTTP
listener receives a payload in the source format, the runtime transforms it into the
target format, and the result is returned.

## What could and could not be built

The three transformations that matter in practice are JSON, CSV and XML. Only one has an
Octo equivalent:

| Transformation | Built here? | Why |
|---|---|---|
| JSON → JSON | **yes** | JSON in, reshaped JSON out |
| CSV → JSON | no | CEL has no CSV parser and no block provides one |
| XML → JSON | no | CEL has no XML parser and no block provides one |

Probed directly against 0.4.3: `parseCSV`, `parseXML`, `xmlToJson`, `fromJSON`,
`split` are all `undeclared reference`. This is recorded as a capability gap rather
than worked around — a benchmark that quietly substituted a different workload would
be worse than an absent one.

## The integration

Two flows perform the **identical** transformation by different means:

```
POST /transform            multi-transform → body.records.map(r, {...})   ← primary
POST /transform-foreach    foreach mode:map → set-payload per record
```

Each renames fields, nests what arrived flat, derives a taxed amount and a tier, then
envelopes the collection with a count.

| | |
|---|---|
| Connectors | `http` |
| Blocks | `multi-transform`, `foreach`, `set-payload` |
| Knobs | `workers`, `buffer`, `pool` |
| Payload | `PAYLOAD_BYTES`, default 1 KB (≈9 records) |

## Finding: `foreach` with `mode: map` is quadratic

The two flows produce byte-identical output. They do not perform alike.

Measured on `m1pro-16gb` / Octo 0.4.3, 4 VUs, closed model, median latency:

| Payload | Records | CEL `.map()` | `foreach mode: map` | Ratio |
|---|---|---|---|---|
| 1 KB | 9 | 11,659 req/s · 0.23 ms | 5,850 req/s · 0.50 ms | 2× |
| 12 KB | 89 | 1,895 req/s · 2.1 ms | 147 req/s · 27.0 ms | 13× |
| 100 KB | 753 | 278 req/s · 14.3 ms | 2.4 req/s · 1.64 s | **115×** |

Isolating the block from the expression inside it — a `foreach` whose body is the
single term `vars.rec.id`, so that per-element work is as close to nothing as the
runtime allows:

| Records | `foreach` median | Records ×2 → time × |
|---|---|---|
| 9 | 0.36 ms | — |
| 41 | 5.87 ms | ×4.32 |
| 89 | 25.36 ms | ×4.18 |
| 185 | 105.89 ms | ×3.86 |
| 369 | 408.82 ms | ×4.01 |
| 753 | 1,640 ms | ×4.01 |

**Every doubling of the record count quadruples the time.** That is O(n²), held
across four consecutive doublings. The same measurement for CEL's `.map()` gives a
flat ~6 µs per record from 41 records to 753 — linear, as it should be.

The cost is in the block, not in the work it is asked to do: a trivial body is just
as quadratic as the full reshape. The likely mechanism is the map-mode accumulator
being copied per element rather than appended to, which is exactly O(n²) — but that
is an inference from the shape of the curve, not something read out of the source.

Extrapolating the fit, a 1 MB document (≈7,700 records) would take about 100 seconds
in one `foreach`. Measured: every request timed out at the 30 s ceiling.

### Why this matters beyond this scenario

Mapping over a collection is not an exotic thing to ask an integration runtime to
do — it is close to the definition of one. Record-oriented batch work routinely
processes files of hundreds of megabytes row by row. Any Octo flow that uses
`foreach mode: map` over more than a few hundred elements will fall off this cliff,
and it will do so silently: throughput degrades smoothly with input size, so it
looks like "big payloads are slow" rather than like a defect.

[Scenario 002](../002-fanout-transform/) uses `foreach mode: map` over 8 order
lines. At n=8 the quadratic is invisible and its published numbers stand.

## Running it

```bash
task smoke SCENARIO=006-json-transform
task bench SCENARIO=006-json-transform

# The payload ladder
PAYLOAD_BYTES=102400 task bench SCENARIO=006-json-transform

# The two implementations against each other
ROUTE=/transform-foreach task bench SCENARIO=006-json-transform

# Closed-model sweep, for reading against published benchmarks
task vuramp SCENARIO=006-json-transform
```

## Caveats specific to this scenario

- The request body is built once per VU at init and reused. Serialising a fresh
  document per iteration would measure k6.
- Tuning is expected to do nothing here: the flow never blocks, so extra workers
  only add scheduling overhead. That is the point — it is the control case that
  makes [scenario 005](../005-http-proxy/)'s 16× credible.
