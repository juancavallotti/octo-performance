# Learnings

Every row below is a failure the lab actually produced, with the evidence, and the thing that now
makes it impossible to repeat silently.

**The rule: a new failure appends a row *and* a test.** A learning recorded here without a test that
enforces it is a note, and notes do not survive contact with the fourth testing attempt. That is
exactly how the last harness accumulated correct documentation alongside wrong numbers —
[METHODOLOGY.md](../METHODOLOGY.md) declared interleaving mandatory while `run-bench.sh` ran all of A
then all of B, for the entire life of the project.

Specimens for L1, L2, L5, L6 and L7 are frozen in
[`internal/gate/testdata/corpus/`](../internal/gate/testdata/corpus/README.md).

---

## L1 — A colocated load generator makes results bimodal, not just slower

**Evidence.** `~/.octo-versions/octo-0.4.3`, byte-identical, two consecutive days: 15,999 req/s at
0.859 ms p95, then 6,938 req/s at 1,359 ms p95. 2.3× on throughput, 1,580× on p95.

The instinct is to treat generator contention as a constant tax that cancels out of an A/B
comparison. It does not. It is a feedback loop — more VUs, less CPU for the subject, higher latency,
more VUs — so the system has two stable regimes and which one a run lands in is not a property of
the thing being measured.

**Enforced by.** The runner and subject are separate machines. `gate.GeneratorSaturation` marks a
cell invalid on runner-CPU ceiling, pool growth, and cross-cell pool disagreement. The `collapsed`
corpus specimen is a permanent regression test.

## L2 — The VU pool was the cause and was never recorded

**Evidence.** `vus_max` 1,600 in the healthy runs against 7,113 in the collapsed one. 1,600 is
`poolFor()`'s Little's-law floor; the rest is k6 scaling under duress.

`poolFor()` lived inside `lab/k6/lib/options.js`, computed silently at script start, and its output
appeared in no result file. The single number that discriminates a good run from a worthless one was
not merely unpublished — it was uncomputable after the fact.

**Enforced by.** `loadgen.SizePool` computes the pool in Go. `PreAllocatedVUs`, `MaxVUs` and
`ObservedMaxVUs` are all written to `cell.json`, and `VUCap` is a hard ceiling that reads as invalid
when reached. Anything a gate needs to see must exist as a recorded value, not as a side effect
inside someone else's script.

## L3 — Prose in a methodology document is not a control

**Evidence.** [METHODOLOGY.md](../METHODOLOGY.md): *"Alternate and repeat when comparing two
configurations, rather than running all of A then all of B. A difference that survives interleaving
is a difference; one that tracks position in the sequence is an artifact."* Marked **mandatory**.
`lab/bin/run-bench.sh:148` is `for variant; do for rep; do`.

Every "Gain" number the lab ever published is confounded with time-in-session and chassis
temperature.

**Enforced by.** `plan.Expand` emits the execution order, and a table test asserts it exactly.
Blocked order remains expressible but requires an `orderReason` that the report prints in red.

## L4 — Plain A,B,A,B is not balanced either

Alternation still gives arm A every "first slot after cooldown" — the ordinal that inherits the most
thermal and socket state. The order is rep-major with the within-rep arm order flipping: `A B / B A /
A B …`, so for 2 arms × 5 reps the sequence is `A B B A A B B A A B` and both arms see each ordinal
parity equally. Three or more arms rotate a seeded Latin square.

**Enforced by.** `plan.Expand` + a test asserting parity balance, not just alternation. And
`stats.OrderEffect` regresses each metric against execution ordinal, so the claim in the report is
"position explains 4% of variance, r²=0.11" rather than "we interleaved, trust us".

## L5 — A calibrated rate is a perishable good

**Evidence.** `STEADY_RATE=16000` was chosen from a real capacity ramp on 2026-07-25 — the reasoning
is a comment in `scenario.env`, and it was sound. By 2026-07-26 every cell exited k6 with 99 and shed
262k–629k iterations. Nothing re-checked it, and `results/index.md` never showed the breach.

A rate calibrated on one machine at one moment is a constant in the code and a variable in reality.

**Enforced by.** Each campaign opens with a calibration phase per scenario, records the knee and the
chosen fraction of it, and shares one rate across all arms of that scenario so arms are compared at
equal offered load. `gate.RateGap` and `gate.DroppedIterations` fire on the breach itself.

## L6 — A saturated run reports a throughput number like any other

**Evidence.** Every published cell was saturated. The `⚠ non-zero dropped iterations … describes a
saturated system` warning existed — in each individual `REPORT.md`, stripped out of the index that
anyone actually read. 61 rows of point estimates with no verdict, no spread, and no mark on the
invalid ones.

**Enforced by.** `gate.Verdict` is a value attached to the cell, and the report is structurally
unable to put a non-valid cell into a median. Excluded cells stay visible in a validity ledger rather
than being silently dropped — a missing row and a bad row are both worse than a struck-through one.

## L7 — Client-side latency is not server-side latency, and only one run could tell

**Evidence.** The `metricson` run: client p95 933.01 ms, runtime mean flow duration 0.41 ms. Its own
report says the gap is *"queueing at the load generator, not work in the runtime"*.
`results/index.md` published the 933.01 ms as Octo's p95. 35 of 37 runs had no server-side data at
all, so for those the question could not even be asked.

**Enforced by.** Server-side metrics are on by default, client and server latency sit side by side in
the report, and a divergence beyond threshold is a finding.

## L8 — Interpolating inside the lowest histogram bucket invents a number

**Evidence.** `octo_flow_duration_seconds` uses Prometheus default buckets, lowest edge 5 ms; the
flow completes in microseconds, so every observation lands in the first bucket. Linear interpolation
there yields a p50 of 2.5 ms — a function of bucket width and nothing else.

`lab/bin/prom.py` got this right, and getting it right is worth more than most of the rest of the
harness.

**Enforced by.** `promx.Quantile` is a struct carrying `Ok`, `Bound` and `Edge`; `Seconds` is
meaningless unless `Ok`. No caller can receive a bare float, so nobody can print false precision by
accident. Tested against the real exposition in `internal/promx/testdata/`.

## L9 — Ask the artifact, never the version string

**Evidence.** A source build reports the same version constant as the release it branched from, so a
version string cannot answer "does this binary have an admin port". And guessing wrong is not a
degraded measurement — passing `--metrics` to 0.4.3 is a hard flag-parse failure at start-up.

**Enforced by.** `subject.Capabilities` greps the artifact's own `run --help`, archives the raw help
text, and the assembled argv is written to `argv.json` so the invariant is auditable after the fact.

## L10 — Detection that is wrong optimistically is the dangerous direction

Capability detection is a grep of someone else's CLI output, which is not an API. If it says the
admin port is absent, the harness loses a data source. If it says the port is present and it is not,
the harness waits on readiness that never arrives, or worse, reports a cold start measured by a
different method than the arm it is compared against.

**Enforced by.** Positive confirmation: if detection claims an admin port, `/livez` must answer or
the cell is invalid. `ReadyResult.Method` is recorded per cell, and arms whose readiness method
differs are not comparable on cold start.

## L11 — Two bracketing snapshots bracket the wrong window once the window is detected

`scrape-metrics.py` wrote `metrics-start.prom` and `metrics-end.prom` around the whole load pass.
That is correct only if the measured window *is* the whole load pass. Once warm-up is detected rather
than slept through, those two files describe an interval nobody is reporting on.

**Enforced by.** Every scrape is kept (`metrics.ndjson.gz`, a few MB per campaign), and the two
bracketing the *detected* window are the ones differenced.

## L12 — The measured window was a `sleep`

A fixed 10 s warm-up assumes the shape of the ramp. It is also unfalsifiable: nothing recorded
whether the system had actually settled.

**Enforced by.** `stats.DetectSteady` accepts the earliest suffix whose CV and slope clear threshold
*and* whose subject-CPU series is also flat, with a minimum duration. No window → the gate fires;
there is no silent fallback. Both the detected window and the naive fixed window are written to
`cell.json`, so detection can be audited across a campaign instead of trusted.

## L13 — Experiment intent lived in directory names

**Evidence.** `-mON`, `-mOFF`, `-mNOSCRAPE`, `-probesonly`, `-metricson`: five free-text `RUN_LABEL`
values covering two actual axes, two of them synonyms. The only place the mapping is written down is
four lines of `results/isolate-2026-07-26.log`, which nothing links to. Two runs are stamped
`vunknown-dev` and are unattributable to any commit, and were published anyway.

**Enforced by.** The campaign spec is the intent, is checked in, and is archived verbatim with the
result. A campaign refuses to start an arm whose version cannot be resolved.

## L14 — An aborted run looks exactly like a finished one

**Evidence.** Three directories on disk have partial cells and no `result.json`. `index.py` silently
skips them, so `results/` holds 40 directories while the index lists 34 and nothing explains the gap.

**Enforced by.** A campaign state file records per-cell completion; incomplete cells are marked as
such and a campaign resumes rather than restarting.

## L15 — n=3 and a median is not a result

**Evidence.** `+5.1%` published from baseline `{11600.8, 10781.5, 9989.8}` against tuned
`{10457.9, 11577.5, 11333.4}` — distributions that almost entirely overlap. Gains of `+305.1%` and
`-0.0%` printed to one decimal with identical typographic weight.

**Enforced by.** `stats.Compare` returns a `Comparison` with a populated `Reason` whenever it refuses
significance — delta inside the noise band, overlapping CIs, N below minimum, or a non-valid cell in
either arm. There is no code path that emits a bare point estimate.

## L16 — Four programs, one procedure

`run-bench.sh`, `sweep.sh`, `run-vuramp.sh` and `run-profile.sh` each re-implemented
start → ready → warm-up → sample → measure → stop → cooldown with a copy-pasted `k6 run`. A fix
applied to one did not reach the others.

**Enforced by.** `campaign.RunCell` is the only implementation. Sweep, capacity and VU-ramp are
campaign *shapes* — different specs, same code.

## L17 — A reporter that computes will become a library

`report.py` reached 1,020 lines doing derivation, validity rendering and formatting at once, and
`sweep-report.py` began importing from it. It printed a validity table and then printed the numbers
anyway, because nothing structural stopped it.

**Enforced by.** `report` receives a fully computed model and derives nothing.

## L18 — Capturing a field is not the same as using it

`time.txt` carries `instructions retired` and `cycles elapsed` on every run, and nothing has ever
read them. Meanwhile ~60 captured fields per rep funnelled down to 4 published columns, so the
interesting data was on disk and invisible.

**Enforced by.** The typed result model carries everything and the report chooses. A field that
reaches neither the report nor a gate is deleted rather than collected.

## L19 — Reaching a dependency by a longer road measures the road

**Evidence.** Scenarios 003 and 005 addressed host-side dependencies as `host.docker.internal`: out
through the VM's NAT, onto the host, back through a published port. For a flow doing three database
round trips that path dominated, and raising `workers` made it *worse*.
[METHODOLOGY.md](../METHODOLOGY.md) calls the resulting column *"not merely indicative — it is
misleading"* and says to ignore it, which is the correct call and also an admission that the
measurement should not have been taken.

**Enforced by.** Dependencies run on their own VM on the same VPC — an ordinary network hop, honest
to describe, and the same road in every arm.

## L20 — Correlating three machines means correlating three clocks

At 1 Hz over a 60 s window, 500 ms of skew moves a sample by a whole bucket, which is enough to
invert "the CPU spike preceded the RPS drop". GCP VMs are NTP-disciplined, but "disciplined" is not
"synchronised", and a step correction mid-campaign is not detectable after the fact.

**Enforced by.** The agent handshake measures the offset over N round trips (minimum-RTT sample), at
cell start and again at cell end. Offset, uncertainty and inter-probe drift are recorded per cell,
and drift beyond threshold is a finding.

## L21 — Absent and zero are different claims

**Evidence.** k6 omits `dropped_iterations` from the summary entirely when a run dropped nothing.
A parser that reads a missing metric as zero and a present zero as zero cannot tell a clean run from
one whose generator never reported. The same shape appears throughout: `octo_build_info` is absent
from a build with no `--metrics`, and the old lab's `runtime-identity.json` recorded that absence as
an empty string indistinguishable from a failed scrape.

This is the optimistic direction, which is the dangerous one. Every absence here resolves to
"nothing went wrong".

**Enforced by.** Every parsed metric carries `Present` beside its value —
`loadgen.Counted`, `Rated`, `Gauged`, `Trend`, and `promx.Quantile.Ok`. `TestAbsentIsNotZero` asserts
it against both k6 fixtures, one of which dropped 11,292 iterations and one of which dropped none.

## L22 — A time series that costs two gigabytes per cell is a time series nobody keeps

**Evidence.** k6 emits one CSV row per observation and about a dozen observations per request. Sixty
seconds at sixteen thousand requests per second is thirteen million rows, roughly two gigabytes, for
one cell out of seventy. The old lab's response was to keep no time series at all — only the
end-of-run summary — which is why it could not detect a steady window, could not see the achieved
rate fall during a run, and could not distinguish a generator that grew its pool from one that did
not. Three of the gates this rebuild depends on were impossible for want of a file nobody wanted to
store.

**Enforced by.** `k6 --out csv=` writes to a named pipe and `loadgen.Aggregator` folds the rows into
one-second buckets as they arrive; the raw rows are never stored. Sixty rows reach disk. `RawRows`
and `UnparsedRows` are recorded, so loss is visible as a number rather than as silence, and
`TestK6DrivesARealLoadPassAndStreamsItsSeries` fails if any artifact in a cell directory exceeds four
megabytes.

## L23 — Documenting from memory is the same mistake as inferring from a version string

**Evidence.** `docs/ARCHITECTURE.md` specified the positive capability confirmation as "the admin
port must answer `/livez`". The runtime has never served `/livez`. `octo run --help` says `/healthz`
and `/readyz`, and it said so the whole time.

This ledger's oldest rule is to ask the artifact rather than believe a claim about it, and the
document stating that rule broke it — in the one paragraph specifying a probe. Had the code been
written to match the document, every cell would have failed its confirmation and the failure would
have read as a broken runtime.

**Enforced by.** The three admin routes are constants in `internal/subject`, named once, and the
fixtures under `internal/subject/testdata/help/` are the verbatim output of all four octo binaries
this lab compares. `TestCapabilitiesComeFromTheArtifactNotTheVersionString` reads them rather than
any prose, including this sentence.
