---
title: "Comparing Octo to other integration runtimes"
---

# Comparing Octo to other integration runtimes

This document exists because "Octo does 6,000 req/s" means nothing on its own, and
because the obvious next move — putting that number next to a vendor's published
number — is wrong more often than it is right.

It records what other runtimes have actually published, under what conditions, which
of their scenarios we have rebuilt, which we cannot, and what may honestly be
concluded today. It is deliberately conservative: the point of the lab is numbers
that survive scrutiny, and a comparison that flatters Octo by ignoring a
methodological difference is worth less than no comparison at all.

## The four things that make published numbers incomparable

Before any table, the ways two throughput figures can look alike and mean different
things:

**1. Load model.** a commercial platform, Camel and most vendor benchmarks are **closed-model**:
a fixed population of virtual users, each waiting for a response before sending
again. Throughput is then what the clients could *extract*. This lab is otherwise
**open-model**: load is offered at a fixed rate regardless of whether the server
copes, so throughput is what the server was *asked* for and degradation shows up as
latency and dropped iterations. The two are not convertible. A closed-model
benchmark also produces a "knee point" — the VU count where throughput peaks — which
simply does not exist under an open model.

> This is why the lab has `task vuramp`. It is the only closed-model test here, and
> it exists solely so a curve can be drawn on the same axes as theirs.

**2. What counts as one transaction.** a commercial platform says it plainly: for the proxy and
API cases a transaction is an HTTP request; for batch it is *a record from the input
file*; for JMS it is *a message arriving on the destination queue*. A "2,800 TPS"
batch figure and a "10,000 TPS" proxy figure are not the same unit.

**3. The envelope.** a commercial platform publishes at 0.1, 1 and 4 CPUs, where a CPU is
"the number of CPU cores available to a given deployment". Camel's figures come from
a 24-core dual-Xeon server. Ours come from a 10-core laptop **that is also running
the load generator**. Cores are not the only difference — theirs are x86 server
parts, ours are ARM laptop parts with 8 performance and 2 efficiency cores.

**4. Where the generator and the backend live.** a commercial platform's reference deployment is
three separate EC2 instances: JMeter on a c5n.2xlarge, the runtime on a c5n.xlarge,
the backend on a third. Everything in this lab shares one machine.

## What has been published

### a commercial platform — *the platform Performance* whitepaper (2020, the platform.3.0)

The most complete public benchmark of the three, and the reason this document
exists. Reference deployment: RHEL 7.6, Java 8 Oracle Hotspot, JMeter load driver on
a separate instance.

| Emulated environment | Instance | vCPU | Memory | JVM heap |
|---|---|---|---|---|
| On-premise runtime | c5n.xlarge | 4 | 10.5 GB | 2 GB |
| On-premise backend | c5n.xlarge | 4 | 10.5 GB | 2 GB |
| Load client | c5n.2xlarge | 8 | 21 GB | 16 GB |
| a hosted platform 1 CPU | t3.medium | 2 | 4 GB | 2 GB |
| a hosted platform 0.1 CPU | t3.micro | 2 | 1 GB | 500 MB |

Headline figures, read from their charts:

| Use case | Shape | 4 CPU knee | Peak TPS (4 CPU) |
|---|---|---|---|
| HTTP proxy, 1 KB | GET → Vert.x backend with **70 ms delay** | 1,000 VUs | ~10,000 |
| HTTP proxy, 1 MB | same, non-repeatable streaming | 300 VUs | ~1,200 |
| API gateway (Client ID) | proxy + policy | 1,000 VUs | ~10,000 |
| schema-driven REST REST, no validation | POST, 1 KB, **no backend, no delay** | 300 VUs | ~25,000 |
| schema-driven REST REST, validation | POST, 1 KB | 300 VUs | ~17,500 |
| payload transformation CSV→JSON | HTTP in, transform, out | **10 VUs** | ~1,000 |
| payload transformation JSON→POJO | " | **10 VUs** | ~650 |
| payload transformation XML→JSON | " | **10 VUs** | ~600 |
| JMS bridge (ActiveMQ) | queue → queue | 15,000 msg/s ingress | ~15,000 |
| JMS bridge XA | 2-phase commit | 200 msg/s ingress | ~200 |
| Kafka publisher | GET → Kafka insert | 1,000 VUs | ~27,000 |
| Batch (CSV → file → MySQL) | per-record | — | ~2,800 records/s |

Two of their own observations are worth carrying over because they describe
behaviour we also have to reason about:

- Past the knee, CPU **drops** while throughput falls — they attribute it to GC
  pauses. Octo has no GC pauses, but scenario 003 showed the same signature for a
  different reason (workers blocked on I/O), so "CPU falling as latency rises" is
  not diagnostic on its own.
- At 0.1 CPU, "the application fails from 5,000 virtual users onwards". Small
  deployments do not degrade gracefully.

### Apache Camel

Camel publishes routing latency rather than end-to-end throughput. Camel 4.2.0
routes an exchange through a **content-based router** in **0.345 ms**, improved from
0.367 ms in 4.0.3 — about 18% faster than 4.0.2, on 2× Xeon Silver 4116 (24 cores),
192 GB, Java 17, RHEL 9.3.

**This number is not comparable to an HTTP benchmark.** It is in-process exchange
routing with no network, no HTTP parsing, and no serialisation. Scenario 001's 0.09 ms
p50 is an entire HTTP round trip. The magnitudes are similar and the quantities are
not.

Camel on Quarkus reaches roughly 14,700–16,400 req/s in Red Hat's published figures,
with native compilation roughly halving throughput in exchange for footprint — a
Camel + Quarkus native image lands near 118 MB.

### Workato

Workato publishes **no throughput benchmark**, and its published limits show why the
comparison is a category error rather than a hard one:

| Limit | Value |
|---|---|
| Webhook ingress, steady | **20 events/second** (72,000/hour) |
| Webhook burst allowance | 9,000 events beyond steady rate |
| Maximum recipe concurrency | **30** |
| Default recipe concurrency | **1** |
| Developer API | 60 requests/minute |
| Max trigger payload | 50 MB |
| Standard job timeout | 90 minutes |

Workato's unit of scale is the *job* and the *recipe*, priced by task volume, with a
platform-enforced ceiling three orders of magnitude below what a single Octo process
does on a laptop. They are not competing on throughput and do not claim to: the
product is managed connectivity and governance, and the runtime is somebody else's
problem by design.

The useful comparison with Workato is therefore not performance. It is: what do you
give up by running your own runtime, and what do you get back? You get throughput
that is not rate-limited by a vendor, and a per-request cost you control. You take
on the operability burden — which is exactly the section below.

## What we have rebuilt, and what we cannot

| a commercial platform use case | Octo scenario | Status |
|---|---|---|
| HTTP proxy (70 ms backend, 1 KB) | [005-http-proxy](scenarios/005-http-proxy/) | **built** — same delay, same payload size |
| payload transformation JSON→POJO | [006-json-transform](scenarios/006-json-transform/) | **built** |
| HTTP proxy 1 MB | 005 with `BACKEND_SIZE=1048576` | buildable |
| schema-driven REST REST, no validation | closest is [001](scenarios/001-template-page/) / [002](scenarios/002-fanout-transform/) | approximate — no RAML/OAS-driven router in Octo |
| payload transformation CSV→JSON | — | **impossible**: no CSV parser in CEL or in any block |
| payload transformation XML→JSON | — | **impossible**: no XML parser in CEL or in any block |
| API gateway policies | — | **impossible**: no policy engine; `jwt-validate` is the only comparable primitive and it is not rate limiting or client-ID enforcement |
| Batch (CSV → file → DB) | — | **not buildable as published**: no batch component, and `foreach mode: map` is quadratic (see 006), so the record-oriented shape their benchmark uses does not survive the record counts they use |
| JMS bridge / JMS bridge XA | [004-queue-roundtrip](scenarios/004-queue-roundtrip/) is *adjacent* | **not comparable**: theirs is ActiveMQ between two processes with optional XA; ours is an in-process queue with no broker and no distributed transaction |
| Kafka publisher | — | **impossible**: no Kafka connector |
| Camel content-based router | planned, `switch` block | not yet built |

The impossible rows are not padding. Four of a commercial platform's six standalone use cases
cannot be reproduced in Octo at all, and that is a more informative result about
where Octo currently sits than any throughput number in this document.

## What can honestly be said today

**Defensible now:**

- **Footprint, by an order of magnitude.** Octo idles at ~22 MiB RSS native
  (~9.5 MiB containerised) and cold-starts in ~130 ms. A Camel + Quarkus native
  image is ~118 MB; a 0.1-CPU a hosted platform worker is given 500 MB of JVM heap before
  it has served a request. This comparison survives every methodological difference
  above, because none of them touch it.
- **Cost per request on CPU-bound work.** 0.097 CPU-ms per request on scenario 001
  is measured directly from a differentiated CPU counter and is a property of the
  runtime, not of the harness.

**Not defensible yet, and should not be claimed:**

- Any throughput comparison. Ours is a laptop that is also generating the load;
  theirs is a dedicated server with a dedicated generator. Even scenario 005, built
  to their spec, differs in envelope and in where the backend runs.
- Anything about behaviour past saturation. a commercial platform reports what happens at 5,000
  and 20,000 virtual users. We have not run those concurrencies and could not
  generate them from the same host.

**What we already know weakens the case, regardless of hardware:**

- Out of the box, scenario 005 — a commercial platform's most common use case — is capped at
  **108 req/s** by the default `workers: 8` against a 70 ms backend. a commercial platform's
  *smallest* published deployment does thousands. Tuned, Octo reaches ~1,780 req/s
  on this host. The gap between 108 and 1,780 is a default, not a runtime.
- `foreach mode: map` is quadratic, which rules out the record-oriented workloads
  that make up a large part of what integration platforms are bought for.
- No `/metrics`, no `/healthz`, no tracing. Every vendor above ships this. It is the
  difference between a fast runtime and an operable one.

## Rules for adding to this document

1. **Record their conditions with their number.** A figure without the envelope,
   load model, and transaction definition that produced it is not usable.
2. **Match the scenario before comparing.** A number is only comparable to a
   scenario built to the same spec — same delay, same payload, same transaction.
3. **Say what could not be built.** The gaps are findings.
4. **Prefer footprint and cost-per-request over throughput** until the lab runs on a
   Linux x86 host with a separate load generator. That runner is the single change
   that would move most of the "not defensible yet" list into the first list.

## Sources

- Vendor performance reports for commercial integration platforms, where they document their conditions
- [The Rise and Fall of the Performance Monsters](https://camel.apache.org/blog/2023/11/camel-4-performance-improvements-2/) (Apache Camel, 2023)
- [Boost Apache Camel performance on Quarkus](https://developers.redhat.com/articles/2021/12/06/boost-apache-camel-performance-quarkus) (Red Hat, 2021)
- [Workato platform limits](https://docs.workato.com/limits.html)
- [Workato recipe settings — concurrency](https://docs.workato.com/recipes/settings.html)
