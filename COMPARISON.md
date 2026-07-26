---
title: "Reading benchmark numbers across runtimes"
---

# Reading benchmark numbers across runtimes

"Octo does 6,000 req/s" means nothing on its own. The obvious next move — putting that
number beside a figure someone else published — is wrong more often than it is right.

This document is about **how to compare integration-runtime benchmarks at all**: what has
to match before two numbers describe the same thing, what this lab does to make its own
numbers comparable, and what may honestly be concluded today. It is deliberately
conservative. A comparison that flatters Octo by ignoring a methodological difference is
worth less than no comparison.

## The four things that make published numbers incomparable

**1. Load model.** Most published benchmarks are **closed-model**: a fixed population of
virtual users, each waiting for a response before sending again. Throughput is then what
the clients could *extract*. This lab is otherwise **open-model**: load is offered at a
fixed rate regardless of whether the server copes, so throughput is what the server was
*asked* for, and degradation surfaces as latency and dropped iterations rather than as a
quietly smaller number.

The two are not convertible. A closed-model benchmark also produces a **knee point** — the
virtual-user count at which throughput stops rising — which simply does not exist under an
open model, where the same server has no knee and just accumulates latency.

> This is why the lab has `task vuramp`. It is the only closed-model test here, and it
> exists so a curve can be drawn on the axes most published results use.

**2. What counts as one transaction.** A request, a record, a message delivered to a
destination queue, and a file processed are all reported as "TPS" somewhere. A batch
figure and a proxy figure are not the same unit even when both are labelled throughput.

**3. The envelope.** Cores, memory, and — for JVM runtimes — heap size. Server-class x86
parts behave differently from laptop ARM parts with a mix of performance and efficiency
cores. This lab publishes from a 10-core M1 Pro **that is also running the load
generator**; most published figures come from dedicated server hardware.

**4. Where the generator and any backend live.** Serious benchmarks put the load driver,
the runtime, and any backing service on separate machines. Everything in this lab shares
one host. That compresses the achievable ceiling and adds contention noise.

## Making a run comparable

Two harness features exist only for this:

```bash
# Closed-model sweep: throughput and CPU against virtual users, one k6 execution
# per level, server restarted between levels so no ordering artifact accumulates.
task vuramp SCENARIO=005-http-proxy

# Cap the container's CPU and memory, so a run describes a known deployment size
# rather than "whatever the laptop had spare".
CPU_LIMIT=1 MEM_LIMIT=4g task bench SCENARIO=005-http-proxy TARGET=docker
```

`CPU_LIMIT` maps to `--cpus`, is read back from the daemon rather than trusted, and is
stamped into the run id so a capped run cannot be filed as a repeat of an uncapped one.
Summaries record their load model, so an open-model and a closed-model number can never
end up in the same table by accident.

## Published figures worth knowing about

Only sources that publish their conditions are useful. Two do:

**Apache Camel** reports routing latency rather than end-to-end throughput. Camel 4.2.0
routes an exchange through a content-based router in **0.345 ms**, improved from 0.367 ms
in 4.0.3, on 2× Xeon Silver 4116 (24 cores), 192 GB, Java 17, RHEL 9.3.

**This is not comparable to an HTTP benchmark.** It is in-process exchange routing: no
network, no HTTP parsing, no serialisation. Scenario 001's 0.09 ms p50 is an entire HTTP
round trip. The magnitudes are similar and the quantities are not — which is exactly the
trap this document exists to describe.

**Camel on Quarkus** reaches roughly 14,700–16,400 req/s in Red Hat's published figures,
with native compilation roughly halving throughput in exchange for footprint; a Camel +
Quarkus native image lands near 118 MB.

**Hosted iPaaS products** generally publish platform limits rather than benchmarks, and
the limits show why throughput comparison is a category error rather than a hard problem.
Workato, for instance, documents a steady webhook ingress ceiling of 20 events/second and
a maximum recipe concurrency of 30. Their unit of scale is the job, priced by task volume;
they are not competing on requests per second and do not claim to. The useful question
against a hosted platform is not "which is faster" but "what do you give up by running
your own runtime, and what do you get back" — throughput that is not rate-limited by a
vendor, and a per-request cost you control, against the operability burden of owning it.

## What can honestly be said today

**Defensible now:**

- **Footprint, by an order of magnitude.** Octo idles at ~22 MiB RSS native (~9.5 MiB
  containerised) and cold-starts in ~130 ms. A Camel + Quarkus native image is ~118 MB;
  JVM integration runtimes are typically given hundreds of megabytes of heap before
  serving a request. This survives every methodological difference above, because none of
  them touch it.
- **Cost per request on CPU-bound work.** 0.097 CPU-ms per request on scenario 001,
  measured from a differentiated CPU counter. That is a property of the runtime, not of
  the harness.

**Not defensible yet, and should not be claimed:**

- Any throughput comparison. Ours is a laptop also generating the load.
- Anything about behaviour past saturation at high concurrency, which we have not
  generated and could not from this host.

**What weakens the case regardless of hardware:**

- Out of the box, [scenario 005](scenarios/005-http-proxy/) — a proxy to a service that
  takes 70 ms — is capped at **108 req/s** by the default `workers: 8`. Tuned, it reaches
  ~1,780 req/s on the same host. The gap is a default, not a runtime.
- `foreach mode: map` is [quadratic](scenarios/006-json-transform/), which rules out the
  record-oriented workloads that make up much of what integration platforms are used for.
- No `/metrics`, no `/healthz`, no tracing. This is the difference between a fast runtime
  and an operable one.

## Common workloads with no Octo equivalent

Building the comparison scenarios turned into a capability checklist. These are ordinary
integration workloads that cannot currently be expressed:

| Workload | Blocker |
|---|---|
| XML → JSON transformation | No XML parser. XML remains the lingua franca of enterprise integration. |
| API gateway policies | No policy engine — no rate limiting, quotas, or client-identity enforcement. `jwt-validate` is the only comparable primitive. |
| Record-oriented batch | No batch component: no job/step model, no chunked commit, no per-record error isolation, no restart. Compounded by the quadratic `foreach`. |
| Kafka / JMS integration | No connector for either. |
| Schema-driven REST APIs | No RAML/OpenAPI-driven router or request validation. |
| Distributed transactions | No XA, and no documented transaction boundary for a multi-step flow. |

That list says more about where Octo currently sits than any throughput number.

**One row has left it.** CSV → JSON was on this list because CEL had no `split` and
no block provided a parser. The runtime still ships no CSV parser, but the cel-go
utility libraries — strings, lists, encoders, math, two-variable comprehensions,
sets, regex — are now registered on every expression, so the parse can be written
as one. [Scenario 007](scenarios/007-csv-transform/) does exactly that, with the
caveat a naive reader carries: it does not implement RFC 4180 quoting, so it is the
cheap case rather than the general one. The support is unreleased at the time of
writing, and the row comes back if it does not ship.

XML has no equivalent route. The libraries add no parser for it and none of them
compose into one, so that row stands.

## Rules for adding to this document

1. **Record the conditions with the number.** A figure without its envelope, load model,
   and transaction definition is not usable.
2. **Match the scenario before comparing.** A number is comparable only to a scenario
   built to the same spec.
3. **Say what could not be built.** The gaps are findings.
4. **Prefer footprint and cost-per-request over throughput** until the lab runs on a Linux
   x86 host with a separate load generator. That runner is the single change that would
   move most of the second list into the first.
5. **Describe workloads, not vendors.** Scenarios here are justified by what they exercise
   in the runtime — blocking I/O, collection mapping, fan-out — not by who else measured
   something similar.

## Sources

- [The Rise and Fall of the Performance Monsters](https://camel.apache.org/blog/2023/11/camel-4-performance-improvements-2/) (Apache Camel, 2023)
- [Boost Apache Camel performance on Quarkus](https://developers.redhat.com/articles/2021/12/06/boost-apache-camel-performance-quarkus) (Red Hat, 2021)
- [Workato platform limits](https://docs.workato.com/limits.html)
