# Methodology

Every published result in this repository follows the same shape. This document defines that shape,
the metrics, and — just as importantly — what the numbers do *not* mean.

## The five things every result states

1. **The hardware.** Captured per host: CPU model, core count, page size, kernel, machine type, and
   the clock offset between the machines involved.
2. **The integration under test.** Described in the scenario's `README.md`: which connectors and
   blocks it exercises, the request shape, the response shape, and why it is worth measuring.
3. **The out-of-the-box result.** The runtime with every tunable stripped, so it falls back to
   whatever it actually ships with.
4. **The optimized result.** The same integration with the knobs written as literals.
5. **The cost.** CPU-ms per request and peak RSS, alongside throughput. Speed with no cost attached
   is half a result.

And underpinning all of it: **the version under test**, checked against what the running process
reported about itself.

## Arms

| Arm | Config | Meaning |
|---|---|---|
| `baseline` | every tunable stripped | What a user gets by default. |
| `tuned` | the same flow with knobs written as literals | What deliberate configuration buys. |

Both are derived from **one** checked-in `integration.yaml`. Two checked-in configs would drift, and
the drift would look like a result. The renderer re-parses its own output and refuses a baseline that
still declares a tunable — an unverified baseline is indistinguishable from a verified one until it
silently reports the wrong default months later.

Because provenance cannot rest on byte-exactness (the YAML is re-emitted with the library's own
indentation), each cell archives `config.render.json`: source digest, selector, node path, before →
after, and source line. That says *what changed and where*, which is strictly more than the archived
file alone ever conveyed.

## Load model

Open, everywhere, unless a spec says otherwise — and the model is **recorded** with every run rather
than inferred later, so an open-model and a closed-model number can never end up in the same table.

Under a closed model (a fixed virtual-user population looping), a slow server simply receives fewer
requests, and throughput self-limits into a number that looks stable while hiding the problem. Under
an open model, load is offered at a fixed rate regardless of how the server is coping, so degradation
surfaces honestly as rising latency and non-zero `dropped_iterations`.

`model: closed` exists for cross-vendor comparability only. See [COMPARISON.md](COMPARISON.md).

## The rate is measured, not remembered

Each campaign opens with a **capacity ramp** per scenario: a staircase from a start rate to a peak,
each rung held for a dwell and measured only over the dwell. A rung is *held* when it achieved what
was offered, shed no iterations, failed nothing, and stayed within a factor of the first rung's mean
latency — and when it is not held, which of those four gave way is named. A bend in a curve is not a
diagnosis; "the generator shed 9,161 iterations" is.

- A ramp that folds over yields a **knee**.
- A ramp that holds every rung yields a **lower bound**, and the report says which it got. Calling a
  bound a measurement is how a campaign ends up running at half of a ceiling that was never located.

The chosen rate is a stated fraction of the knee (default 50%), and **every arm of that scenario runs
at it**. An arm that cannot hold the shared rate then produces a saturation finding rather than a
quietly smaller number.

The ramp itself is warmed first, by a discarded rung at the start rate. Without it the first rung —
the reference every later rung's latency is compared against — measures a cold runtime and a
generator still allocating its virtual-user pool. That is not hypothetical: it read 11.57 ms against
a true 0.44 ms and chose a rate 3% of capacity. See [L26](docs/LEARNINGS.md).

## Run procedure

Per cell — one scenario, one arm, one repetition:

1. Build the request body, hash it, archive it. What was offered is as much a part of a result as
   what came back.
2. Render this arm's config and stage the whole integration directory where the subject will read it.
3. Ask the artifact what flags it accepts. Never infer that from a version string.
4. Assert the workload port is free. A stale process answers 404 quickly, which reads as excellent
   throughput.
5. Start the runtime, recording the exact argv.
6. Poll until ready, confirming that a detected admin port actually answers. Detection that is wrong
   in the optimistic direction is the dangerous one. That latency is recorded as **cold start**.
7. Ask the running process what it is, and check it against what was started.
8. Start every collector **before** the window opens. A collector that starts when the measurement
   starts cannot answer whether the measurement was steady.
9. Warm-up pass — kept and labelled, never silently discarded.
10. The measured pass.
11. Stop the collectors, then the runtime.
12. **Choose the window from the data.** See below.
13. Window everything against the interval that was chosen: the subject's counters, the runtime's own
    exposition, the client's series.
14. Gate it. Gates read; they never mutate a measurement and never abort a campaign.
15. Write it all down, atomically.
16. Cool down, so the next cell does not inherit this one's thermal state.

Five repetitions by default, and **every one is kept and shown**. The report draws each repetition as
a dot: with n=5 the honest chart is the five points, because a reader can then see whether a delta
comes from a tight cluster or from two runs that disagree.

## The measured window is detected, not assumed

The old lab slept ten seconds and called the rest steady. This one buckets the client's throughput
into one-second bins and accepts the earliest suffix whose drift across the window and whose
coefficient of variation are both inside bound, with enough duration left to be worth measuring.

It corroborates with the subject's CPU rate where the sampler can actually see it — throughput can
plateau while the runtime is still warming, because the offered rate caps what the generator delivers
and a saturated server looks identical to a settled one from the client alone. Where the sampler is
coarse, the weaker claim is made explicitly rather than a window being rejected for want of evidence.

No window is found ⇒ the gate fires. There is no silent fallback. Both the detected window and the
fixed-offset window the old lab would have used are recorded, so detection can be audited across a
campaign rather than trusted.

## Ordering

Arms rotate within each repetition, so over n repetitions each arm occupies each position exactly
once. `order: blocked` is expressible but requires a stated reason that the report prints.

This is not a preference. The old harness ran all of A then all of B, which gave every published
"Gain" the same confound: time-in-session and chassis temperature. `stats.OrderEffect` now regresses
each metric against execution position and publishes the slope and r², which turns "we interleaved,
so trust us" into a number.

Every cell is followed by a cooldown. A minute at 24,000 req/s leaves on the order of a million
sockets working through `TIME_WAIT`, and a chassis that has been at 300% CPU is thermally a different
machine from a cold one.

## Metrics

### Throughput and latency (client side, from k6)

| Metric | Definition |
|---|---|
| Offered rate | Requests per second the generator was instructed to produce. |
| Achieved RPS | Below the offered rate means the server could not keep up. |
| `dropped_iterations` | Iterations the generator could not start. **Non-zero means the result is a saturation measurement, not a latency measurement.** Absent from a summary when zero — absent is not zero. |
| `http_req_duration` | p50 / p95 / p99 / max, full client-observed request time. |
| `http_req_waiting` | Time to first byte after the request was written. |
| `http_req_failed` | Any non-zero value demands an explanation before publication. Fast failures look exactly like fast successes in a throughput number. |
| `vus_max` against pre-allocated | How far the generator's pool grew. This is the number that discriminates a healthy run from a collapsed one, and in the old lab it appeared in no result file. |

The pool is sized in Go by Little's law from the **tail** latency, not the median, and the sizing is
*recorded*. In the old lab it was computed inside a k6 script that nobody archived — so the one value
that explains the 15,996-against-6,939 incident could not be recovered afterwards.

### The runtime's own view (from `/metrics`, 0.5.0 and later)

Every scrape is kept, not two snapshots, and the two bracketing the **detected** window are the ones
differenced. Once the window is detected rather than slept through, a pair captured at the edges of
the load pass brackets the wrong interval.

`octo_flow_messages_total` by outcome, and `octo_flow_duration_seconds` as a histogram. The
histogram's lowest bucket edge is 5 ms and most flows finish in microseconds, so a quantile inside
that bucket is returned as a **bound** — `ok=false, bound=below, edge=0.005` — rather than as an
interpolated number that was never measured.

### Resource utilisation (OS level, subject side)

CPU is measured by **differentiating cumulative process CPU time** between samples, never by reading
an instantaneous percentage: a decaying average over the process lifetime systematically understates
a short benchmark window.

| Metric | Definition |
|---|---|
| Mean CPU % | From the 1 Hz series over the detected window. 100% = one saturated core. |
| Peak / drift RSS | Sustained positive drift across repetitions is a leak signal. |
| Open descriptors, threads, context switches | Voluntary against involuntary distinguishes a process that blocked from one that was starved of a core. |

### Derived efficiency numbers

These are the point of the exercise. Throughput alone says nothing about whether it was bought
cheaply.

- **CPU-ms per request** — the headline efficiency number and the most portable across hardware.
- **RSS per 1k RPS** — how much memory a unit of throughput costs.
- **Cold start** — process start until the route first answers.

## Client against server, side by side

This comparison is the reason the report exists in its current shape. The old lab published a client
p95 of **933.01 ms** as the runtime's latency while the runtime's own mean flow duration, sitting in
the same result directory, was **0.41 ms**. Neither number was wrong; showing only one of them was.

Every scenario's report shows both and states the gap: what the client saw, what the runtime said,
and the fact that the difference is queueing at the generator rather than work in the runtime.

## Validity is a value

Every cell carries a verdict. Invalid cells are excluded from every aggregate and listed, struck
through, in the report's validity ledger — visible, never silently dropped.

| Gate | Rejects |
|---|---|
| generator headroom | Runner CPU over ceiling, or a pool far above its allocation — the colocation artifact |
| error rate | Non-zero `http_req_failed` |
| saturation | Achieved below offered, or dropped iterations, or a pool that disagrees with a sibling **of the same scenario** by more than a factor |
| steady state | No window could be detected |
| identity | The running process is not the version the harness intended to start |
| capability | An admin port was detected and did not answer |
| clock | The offset between runner and subject drifted during the cell |

Gates ship in **observe mode**: evidence recorded unconditionally, nothing escalating past `suspect`,
and a gate-calibration table in every report showing how often each fired. Thresholds tighten from
that data rather than from a guess — nobody has measured the right runner-CPU ceiling yet, because
the old lab never measured one.

## Caveats

Stated plainly, because a benchmark that hides its limitations is marketing.

- **A colocated generator makes results bimodal, and no gate can subtract it.** k6 and the runtime on
  one machine produced 15,996 req/s and 6,939 req/s from a byte-identical binary a day apart — 2.3×
  on throughput, 1,580× on p95 — with `vus_max` at 1,600 on the healthy run and 7,113 on the
  collapsed one. The generator grows its pool, the extra goroutines take cores from the server, the
  server slows, the pool grows further. It is a feedback loop, not a constant tax, so it does **not**
  cancel between two arms. Every report states whether the campaign was colocated; a colocated
  campaign is a development loop, not a published result.
- **A dependency that shares the subject's cores is folded into the subject's cost.** Scenarios 003
  and 005 reached theirs through `host.docker.internal`, and for a flow doing three database round
  trips per request that path dominated: 1,500 req/s at 1.3 ms p95 natively against 1,270 req/s at
  4,147 ms in a container, with more workers making it *worse*. That is a property of the measurement
  setup, not of the runtime. Dependencies belong on their own host, reached over ordinary networking.
- **A laptop is thermally variable.** Sustained load drifts as the chassis heats, which is why
  repetitions rotate and why every cell is followed by a cooldown.
- **These are single-instance numbers.** One process serving one integration. Nothing here says
  anything about horizontal scaling.
- **Whole-lifetime `rusage` is unavailable over SSH** and is reported absent rather than filled in
  with the transport's own. The cost denominator comes from differencing the subject's cumulative CPU
  counter, accurate to the sampling interval.

## Reading a report

It leads with a **sentence**, not a table. A reader who stops at the top still has the answer.

Then, in order: the regression matrix with every delta labelled and every median carrying its spread
and its n; per-scenario detail with client against server latency side by side; how each rate was
chosen; the validity ledger; gate calibration; provenance.

When scanning:

1. **Read the verdict sentence, and read it carefully.** "No regression was found" and "nothing could
   be established" are different statements, and only the first is a result. A campaign whose
   comparisons all declined for want of evidence says so explicitly.
2. **Check the validity ledger.** Cells excluded by a gate contributed to nothing.
3. **Check how the rate was chosen.** A measured knee and a declared rate mean different things, and
   a lower bound is not a knee.
4. **Read latency only after achieved-against-offered and the drop count.** A gap there means the
   latency percentiles describe a saturated system.
5. **Read the spread, not only the median.** A delta inside its noise band is labelled noise and is
   not a result no matter how large the percentage looks.
6. **Finally read CPU-ms per request.** A config that improves latency while burning materially more
   CPU per request has traded, not won.
