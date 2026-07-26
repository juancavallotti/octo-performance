#!/usr/bin/env python3
"""Turn a profile run into PROFILE.md: where the time went, block by block.

Usage: profile-report.py <out-dir>

Reads cell/metrics-start.prom and cell/metrics-end.prom and ranks the blocks by the
time they held over the window. Deliberately does not write a result.json and does
not touch results/index.md — a profile is a diagnostic and does not belong in the
regression view. See run-profile.sh for why.
"""

import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import prom  # noqa: E402


def fmt_ms(seconds):
    if seconds is None:
        return "n/a"
    return f"{seconds * 1000:,.3f} ms"


def fmt_quantile(detail):
    """A quantile as what its buckets can support — see prom.quantile_detail.

    Block timings use exponential buckets from 0.5 ms, which is finer than the flow
    histogram's 5 ms, but a block that returns in nanoseconds still lands entirely in
    the first bucket and its p95 is then a property of that bucket.
    """
    bound = detail.get("bound")
    if bound == "below":
        return f"< {fmt_ms(detail.get('edge'))}"
    if bound == "above":
        return f"> {fmt_ms(detail.get('edge'))}"
    seconds = detail.get("seconds")
    return "n/a" if seconds is None else f"~{fmt_ms(seconds)}"


def fmt_num(n, digits=0):
    return "n/a" if n is None else f"{n:,.{digits}f}"


def main():
    if len(sys.argv) < 2:
        print("usage: profile-report.py <out-dir>", file=sys.stderr)
        return 2
    out_dir = os.path.abspath(sys.argv[1])
    cell = os.path.join(out_dir, "cell")

    start = prom.parse_file(os.path.join(cell, "metrics-start.prom"))
    end = prom.parse_file(os.path.join(cell, "metrics-end.prom"))
    if not start or not end:
        print("profile-report: no metrics snapshots to read", file=sys.stderr)
        return 1

    summary = {}
    try:
        with open(os.path.join(cell, "summary.json")) as fh:
            summary = json.load(fh)
    except (OSError, json.JSONDecodeError):
        pass
    k6 = summary.get("metrics", {})
    requests = (k6.get("http_reqs") or {}).get("count")

    # ---- flows, so a per-block number has a denominator to be a fraction of.
    flow_msgs = {
        k: int(v) for k, v in
        prom.counter_delta_by_label(start, end, "octo_flow_messages_total", "flow").items()
    }
    flow_time = {}
    for flow in flow_msgs:
        w = prom.histogram_window(start, end, "octo_flow_duration_seconds", flow=flow)
        flow_time[flow] = (w or {}).get("sum")

    # ---- blocks.
    blocks = []
    seen = set()
    for labels, _ in end.get("octo_block_duration_seconds_count") or []:
        path = labels.get("path", "")
        if not path or path in seen:
            continue
        seen.add(path)
        w = prom.histogram_window(start, end, "octo_block_duration_seconds", path=path)
        count = int((w or {}).get("count") or 0)
        if not count:
            continue
        blocks.append({
            "path": path,
            "type": labels.get("type", ""),
            "flow": labels.get("flow", ""),
            "invocations": count,
            "totalSeconds": w.get("sum") or 0.0,
            "meanSeconds": prom.mean_seconds(w),
            "p95": prom.quantile_detail(w, 0.95),
        })
    blocks.sort(key=lambda b: b["totalSeconds"], reverse=True)

    dropped = prom.counter_delta(start, end, "octo_block_events_dropped_total")
    build = prom.labels_of(end, "octo_build_info")

    lines = []
    a = lines.append
    scenario = os.environ.get("SCENARIO_ID", "")
    a("---")
    a(f'title: "profile: {scenario}"')
    a("---")
    a("")
    a(f"# Where the time goes — {scenario}")
    a("")
    a(f"Scenario **{scenario}**, variant **{os.environ.get('VARIANT', '')}**, target "
      f"**{os.environ.get('TARGET', '')}**, Octo **{os.environ.get('VERSION', '')}** "
      f"(services module `{build.get('services_module', '?')}`). "
      f"Blocks watched: `{os.environ.get('BLOCKS', '*')}`.")
    a("")
    a("> **This is a diagnostic, not a result.** Watching any block makes the engine emit an "
      "event around every block in every flow, so the throughput and latency of this run "
      "describe a runtime carrying instrumentation nobody deploys. What survives that is the "
      "*ranking* — which block holds the time relative to its siblings. Re-measure through "
      "`task bench` before quoting any number.")
    a("")

    if not blocks:
        a("No per-block series were recorded. Either no block was matched by the address "
          "filter, or the run took no traffic.")
        write(out_dir, lines)
        return 0

    a("## Blocks by time held")
    a("")
    a("| Block | Type | Flow | Invocations | Mean | p95 | Total held |")
    a("|---|---|---|---|---|---|---|")
    for b in blocks:
        a(f"| `{b['path']}` | `{b['type']}` | `{b['flow']}` | {fmt_num(b['invocations'])} | "
          f"{fmt_ms(b['meanSeconds'])} | {fmt_quantile(b['p95'])} | "
          f"{b['totalSeconds']:,.3f} s |")
    a("")

    top = blocks[0]
    a(f"**`{top['path']}` holds the most time** — {top['totalSeconds']:,.3f} s across "
      f"{top['invocations']:,} invocations, {fmt_ms(top['meanSeconds'])} each.")
    a("")

    a("> **A composite's duration includes its children's**, and each child times itself, so "
      "the Total column double-counts across nested paths and does not sum to the flow's "
      "time. The same applies across flows: a flow reached by `flow-ref` times itself, and "
      "that time nests inside its caller's. Compare siblings, not totals.")
    a("")

    if dropped:
        a(f"> ⚠ **{dropped:,.0f} block event(s) were dropped.** Per-block delivery is "
          "at-most-once — the dispatcher's queue sheds rather than slowing the flow — so the "
          "counts above under-report by an unknown amount. Lower the rate "
          "(`PROFILE_RATE=`) and re-run if the ranking matters.")
        a("")
    else:
        a("*No block events were dropped, so the counts above are complete.*")
        a("")

    a("## Flows")
    a("")
    a("| Flow | Messages | Total flow time |")
    a("|---|---|---|")
    for flow, n in sorted(flow_msgs.items(), key=lambda kv: kv[1], reverse=True):
        t = flow_time.get(flow)
        a(f"| `{flow}` | {fmt_num(n)} | {'n/a' if t is None else f'{t:,.3f} s'} |")
    a("")
    if requests:
        a(f"*k6 sent {requests:,.0f} request(s) over the window.*")
        a("")

    write(out_dir, lines)
    return 0


def write(out_dir, lines):
    path = os.path.join(out_dir, "PROFILE.md")
    with open(path, "w") as fh:
        fh.write("\n".join(lines) + "\n")
    print(f"  profile -> {path}")


if __name__ == "__main__":
    sys.exit(main())
