#!/usr/bin/env python3
"""Turn a run directory into REPORT.md plus a machine-readable result.json.

Usage: report.py <run-dir>

Reads env.json, footprint.json, and every <variant>-<test>-rep<N>/ cell. Picks the
median repetition by achieved throughput and renders the baseline-vs-tuned
comparison the methodology calls for.

CPU cost is computed from the *measurement window* — the delta of the cumulative
CPU counter across resources.csv — not from whole-process totals, which would also
include start-up and warm-up and so overstate per-request cost.

Where the runtime served /metrics, the same window is also read from inside the
process: server-side latency, the outcome split, in-flight saturation, and
allocation per request. That is not a second opinion on the same numbers — it
answers questions the outside view cannot reach. See the "server-side" section.
"""

import csv
import json
import os
import re
import statistics
import sys
from datetime import datetime, timezone

import prom

# ------------------------------------------------------------------ loading ---


def load_json(path, default=None):
    try:
        with open(path) as fh:
            return json.load(fh)
    except (OSError, json.JSONDecodeError):
        return default


def parse_time_file(path):
    """Parse /usr/bin/time output (BSD -l on macOS, GNU -v on Linux)."""
    try:
        text = open(path).read()
    except OSError:
        return {}

    out = {}
    # BSD: "  1.23 real  0.45 user  0.06 sys"
    m = re.search(r"([\d.]+)\s+real\s+([\d.]+)\s+user\s+([\d.]+)\s+sys", text)
    if m:
        out["realSeconds"] = float(m.group(1))
        out["userSeconds"] = float(m.group(2))
        out["sysSeconds"] = float(m.group(3))
    # BSD: "  12345678  maximum resident set size"  (bytes)
    m = re.search(r"(\d+)\s+maximum resident set size", text)
    if m:
        out["maxRssBytes"] = int(m.group(1))

    # GNU fallbacks.
    if "userSeconds" not in out:
        m = re.search(r"User time \(seconds\):\s*([\d.]+)", text)
        if m:
            out["userSeconds"] = float(m.group(1))
        m = re.search(r"System time \(seconds\):\s*([\d.]+)", text)
        if m:
            out["sysSeconds"] = float(m.group(1))
    if "maxRssBytes" not in out:
        m = re.search(r"Maximum resident set size \(kbytes\):\s*(\d+)", text)
        if m:
            out["maxRssBytes"] = int(m.group(1)) * 1024

    if "userSeconds" in out and "sysSeconds" in out:
        out["cpuSecondsLifetime"] = out["userSeconds"] + out["sysSeconds"]
    return out


def parse_resources(path):
    """Reduce the 1 Hz series to the measurement window's resource usage."""
    rows = []
    try:
        with open(path) as fh:
            for r in csv.DictReader(fh):
                try:
                    rows.append({
                        "elapsed": float(r["elapsed_s"]),
                        "cpu_total": float(r["cpu_seconds_total"]),
                        "cpu_pct": float(r["cpu_pct"]) if r["cpu_pct"] else None,
                        "rss": int(r["rss_bytes"]),
                    })
                except (ValueError, KeyError):
                    continue
    except OSError:
        return {}

    if len(rows) < 2:
        return {"samples": len(rows)}

    window = rows[-1]["elapsed"] - rows[0]["elapsed"]
    cpu_window = rows[-1]["cpu_total"] - rows[0]["cpu_total"]
    pcts = [r["cpu_pct"] for r in rows if r["cpu_pct"] is not None]
    rss = [r["rss"] for r in rows]

    return {
        "samples": len(rows),
        "windowSeconds": round(window, 3),
        # CPU consumed during the measured window only.
        "cpuSecondsWindow": round(cpu_window, 4),
        "cpuPctMean": round(cpu_window / window * 100, 2) if window > 0 else None,
        "cpuPctPeak": round(max(pcts), 2) if pcts else None,
        "rssBytesMean": int(statistics.fmean(rss)),
        "rssBytesPeak": max(rss),
        # Sustained positive drift across reps is a leak signal.
        "rssBytesDrift": rss[-1] - rss[0],
    }


def read_int(path, default=None):
    try:
        return int(open(path).read().strip())
    except (OSError, ValueError):
        return default


# ------------------------------------------------------------- server-side ----
#
# What the process said about itself over the same window. Everything here is
# differenced between the scrape at the start of the load window and the one at the
# end, so it describes the window and not the process's lifetime — see prom.py.


def parse_metrics_series(path):
    """Reduce metrics.csv to the gauge peaks and means that only exist as a series."""
    cols = {}
    try:
        with open(path) as fh:
            for r in csv.DictReader(fh):
                for k, v in r.items():
                    if v is None or not v.strip():
                        continue
                    try:
                        cols.setdefault(k, []).append(float(v))
                    except ValueError:
                        continue
    except OSError:
        return {}
    if not cols:
        return {}

    def peak(name):
        return max(cols[name]) if cols.get(name) else None

    def mean(name):
        return statistics.fmean(cols[name]) if cols.get(name) else None

    out = {
        "samples": len(cols.get("elapsed_s", [])),
        "inFlightMean": mean("in_flight_total"),
        "inFlightPeak": peak("in_flight_total"),
        "goroutinesMean": mean("goroutines"),
        "goroutinesPeak": peak("goroutines"),
        "heapAllocBytesPeak": peak("heap_alloc_bytes"),
        "rssBytesPeak": peak("rss_bytes"),
        "openFdsPeak": peak("open_fds"),
        # A run that went unready mid-window is a broken measurement, not a slow one.
        "readyThroughout": all(v == 1.0 for v in cols.get("ready", [])) if cols.get("ready") else None,
    }
    # Per-flow in-flight, so "which flow's workers were all busy" has an answer.
    out["inFlightPeakByFlow"] = {
        k[len("in_flight["):-1]: max(v)
        for k, v in cols.items()
        if k.startswith("in_flight[") and v
    }
    return out


def load_server_metrics(cell_dir):
    """The window as the runtime saw it, or {} when it served no metrics."""
    start = prom.parse_file(os.path.join(cell_dir, "metrics-start.prom"))
    end = prom.parse_file(os.path.join(cell_dir, "metrics-end.prom"))
    if not start or not end:
        return {}

    series = parse_metrics_series(os.path.join(cell_dir, "metrics.csv"))

    out = {"available": True, "series": series}

    # ---- outcomes. The reason this is worth having: k6 reports what the client
    # saw, and a message the runtime dropped after answering 200 is invisible there.
    outcomes = prom.counter_delta_by_label(start, end, "octo_flow_messages_total", "outcome")
    out["messages"] = {k: int(v) for k, v in outcomes.items()}
    out["messagesTotal"] = int(sum(outcomes.values())) if outcomes else None
    out["messagesByFlow"] = {
        k: int(v) for k, v in
        prom.counter_delta_by_label(start, end, "octo_flow_messages_total", "flow").items()
    }
    # Unhandled failures, attributed to the block they came from.
    out["errorsByBlock"] = {
        k or "(not a block)": int(v) for k, v in
        prom.counter_delta_by_label(start, end, "octo_flow_errors_total", "block").items()
        if v
    }

    # ---- server-side latency, from bucket deltas across the window.
    #
    # This is the runtime's own view of how long a message took, and it excludes
    # everything the client's number includes: connection handling, the loopback
    # path, and k6's own scheduling on a host it shares with the server. On this
    # lab's hardware that difference is not a rounding error.
    window = prom.histogram_window(start, end, "octo_flow_duration_seconds")
    if window:
        # The mean is exact — sum over count, both counters. The quantiles are only
        # as good as the buckets, so each carries how much its buckets support it.
        out["flowMeanMs"] = ms(prom.mean_seconds(window))
        out["flowObservations"] = int(window.get("count") or 0)
        for q, key in ((0.50, "flowP50"), (0.95, "flowP95"), (0.99, "flowP99")):
            d = prom.quantile_detail(window, q)
            out[key + "Ms"] = ms(d["seconds"])
            out[key + "Bound"] = d["bound"]
            out[key + "EdgeMs"] = ms(d["edge"])

    # Per flow, since a scenario with more than one root flow has more than one
    # latency, and averaging them together describes neither.
    per_flow = {}
    for flow in sorted(prom.by_label(end, "octo_flow_in_flight", "flow")):
        if not flow:
            continue
        w = prom.histogram_window(start, end, "octo_flow_duration_seconds", flow=flow)
        count = int((w or {}).get("count") or 0)
        d = prom.quantile_detail(w, 0.95)
        per_flow[flow] = {
            "messages": count,
            "meanMs": ms(prom.mean_seconds(w)),
            "p95Ms": ms(d["seconds"]),
            "p95Bound": d["bound"],
            "p95EdgeMs": ms(d["edge"]),
            "inFlightPeak": (series.get("inFlightPeakByFlow") or {}).get(flow),
        }
    out["flows"] = per_flow

    # ---- cost, from inside the process.
    cpu = prom.counter_delta(start, end, "process_cpu_seconds_total")
    out["cpuSecondsWindow"] = round(cpu, 4) if cpu is not None else None
    alloc = prom.counter_delta(start, end, "go_memstats_alloc_bytes_total")
    out["allocBytesWindow"] = int(alloc) if alloc is not None else None
    gc_time = prom.counter_delta(start, end, "go_gc_duration_seconds_sum")
    out["gcSecondsWindow"] = round(gc_time, 4) if gc_time is not None else None
    gc_count = prom.counter_delta(start, end, "go_gc_duration_seconds_count")
    out["gcCycles"] = int(gc_count) if gc_count is not None else None

    # ---- per-block timings, present only when the run asked for them.
    blocks = []
    for labels, _ in end.get("octo_block_duration_seconds_count") or []:
        path = labels.get("path", "")
        if not path:
            continue
        w = prom.histogram_window(start, end, "octo_block_duration_seconds", path=path)
        count = int((w or {}).get("count") or 0)
        if not count:
            continue
        d = prom.quantile_detail(w, 0.95)
        blocks.append({
            "path": path,
            "type": labels.get("type", ""),
            "flow": labels.get("flow", ""),
            "invocations": count,
            "meanMs": ms(prom.mean_seconds(w)),
            "p95Ms": ms(d["seconds"]),
            "p95Bound": d["bound"],
            "p95EdgeMs": ms(d["edge"]),
            "totalSeconds": round(w.get("sum") or 0, 4),
        })
    if blocks:
        blocks.sort(key=lambda b: b["totalSeconds"], reverse=True)
        out["blocks"] = blocks
        # Non-zero means the block counters above under-report, which is otherwise
        # indistinguishable from a genuine drop in traffic.
        dropped = prom.counter_delta(start, end, "octo_block_events_dropped_total")
        out["blockEventsDropped"] = int(dropped) if dropped is not None else None

    return out


def ms(seconds):
    return None if seconds is None else round(seconds * 1000, 4)


# ------------------------------------------------------------------- cells ----


def load_cell(cell_dir):
    """One (variant, test, rep) measurement."""
    summary = load_json(os.path.join(cell_dir, "summary.json"))
    if not summary:
        return None

    res = parse_resources(os.path.join(cell_dir, "resources.csv"))
    timing = parse_time_file(os.path.join(cell_dir, "time.txt"))

    # The container target has no /usr/bin/time; its cumulative counters come from
    # the Docker API and are already reflected in resources.csv.
    final_stats = load_json(os.path.join(cell_dir, "final-stats.json"), {})

    m = summary.get("metrics", {})
    reqs = (m.get("http_reqs") or {}).get("count", 0)
    duration = summary.get("durationSeconds") or 0

    achieved_rps = (m.get("http_reqs") or {}).get("rate")
    if achieved_rps is None and duration:
        achieved_rps = reqs / duration

    cpu_window = res.get("cpuSecondsWindow")
    cell = {
        "dir": os.path.basename(cell_dir),
        "knobs": load_json(os.path.join(cell_dir, "knobs.json"), {}),
        "coldStartMs": read_int(os.path.join(cell_dir, "cold-start-ms.txt")),
        "k6Exit": read_int(os.path.join(cell_dir, "k6-exit.txt"), 0),
        "offeredRate": summary.get("offeredRate"),
        "durationSeconds": duration,
        "requests": reqs,
        "achievedRps": achieved_rps,
        "droppedIterations": (m.get("dropped_iterations") or {}).get("count", 0),
        "failedRate": (m.get("http_req_failed") or {}).get("rate", 0.0),
        "duration": m.get("http_req_duration") or {},
        "waiting": m.get("http_req_waiting") or {},
        "dataReceivedRate": (m.get("data_received") or {}).get("rate"),
        "vusMax": (m.get("vus_max") or {}).get("value"),
        "checks": summary.get("checks", {}),
        "thresholds": summary.get("thresholds", {}),
        "resources": res,
        "timing": timing,
        "finalStats": final_stats,
        "server": load_server_metrics(cell_dir),
    }

    # Derived efficiency numbers — the point of the exercise.
    if cpu_window and reqs:
        cell["cpuMsPerRequest"] = round(cpu_window * 1000 / reqs, 4)
        cell["requestsPerCpuSecond"] = round(reqs / cpu_window, 1)
    else:
        cell["cpuMsPerRequest"] = None
        cell["requestsPerCpuSecond"] = None

    # The same two ratios as the process accounts for them. Kept separate from the
    # OS-sampled pair above rather than replacing them: they are measured by
    # different instruments and a gap between the two is itself information.
    srv = cell["server"]
    srv_cpu, srv_msgs = srv.get("cpuSecondsWindow"), srv.get("messagesTotal")
    if srv_cpu and srv_msgs:
        srv["cpuMsPerMessage"] = round(srv_cpu * 1000 / srv_msgs, 4)
    if srv.get("allocBytesWindow") is not None and srv_msgs:
        srv["allocBytesPerMessage"] = int(srv["allocBytesWindow"] / srv_msgs)
    if srv_msgs and duration:
        srv["messagesPerSecond"] = round(srv_msgs / duration, 1)

    peak_rss = res.get("rssBytesPeak") or timing.get("maxRssBytes")
    if peak_rss and achieved_rps:
        cell["rssBytesPer1kRps"] = int(peak_rss / (achieved_rps / 1000))
    else:
        cell["rssBytesPer1kRps"] = None

    return cell


def median_cell(cells):
    """The representative repetition: median achieved throughput."""
    ranked = sorted((c for c in cells if c.get("achievedRps") is not None),
                    key=lambda c: c["achievedRps"])
    if not ranked:
        return cells[0] if cells else None
    return ranked[len(ranked) // 2]


# ---------------------------------------------------------------- rendering ---


def fmt_bytes(n):
    """Byte sizes, signed — RSS drift is meaningfully negative when memory is released."""
    if n is None:
        return "n/a"
    sign = "-" if n < 0 else ""
    n = abs(n)
    for unit, div in (("GiB", 1073741824), ("MiB", 1048576), ("KiB", 1024)):
        if n >= div:
            return f"{sign}{n / div:.1f} {unit}"
    return f"{sign}{n} B"


def fmt_num(n, digits=1, suffix=""):
    if n is None:
        return "n/a"
    return f"{n:,.{digits}f}{suffix}"


def fmt_ms(n):
    return "n/a" if n is None else f"{n:,.2f} ms"


def fmt_quantile(server, key):
    """A histogram quantile, rendered as what the buckets can actually support.

    "< 5 ms" is a weaker claim than "2.51 ms" and a true one; the interpolated value
    inside the lowest bucket is neither. See prom.quantile_detail.
    """
    bound = server.get(key + "Bound")
    if bound == "below":
        return f"< {fmt_ms(server.get(key + 'EdgeMs'))}"
    if bound == "above":
        return f"> {fmt_ms(server.get(key + 'EdgeMs'))}"
    value = server.get(key + "Ms")
    return "n/a" if value is None else f"~{fmt_ms(value)}"


def delta(base, tuned, higher_is_better, digits=1, suffix=""):
    """Render tuned's change relative to baseline."""
    if base in (None, 0) or tuned is None:
        return "n/a"
    pct = (tuned - base) / base * 100
    good = pct > 0 if higher_is_better else pct < 0
    if abs(pct) < 0.5:
        mark = "≈"
    else:
        mark = "▲" if good else "▼"
    return f"{mark} {pct:+.1f}%"


def row(label, base, tuned, fmt, higher_is_better=None, **kw):
    b = fmt(base) if not isinstance(fmt, str) else base
    t = fmt(tuned) if not isinstance(fmt, str) else tuned
    d = delta(base, tuned, higher_is_better, **kw) if higher_is_better is not None else ""
    return f"| {label} | {b} | {t} | {d} |"


def render_server_side(a, base, tuned, bs, ts, knobs):
    """The section the lab could not write before the runtime exposed /metrics."""
    t = tuned or {}
    a("### What the runtime said about itself")
    a("")
    a("Read from the process's own `/metrics` over the same window, by differencing the "
      "scrape at the start of the load against the one at the end. These are not a second "
      "opinion on the numbers above — they answer questions the outside view cannot reach.")
    a("")

    # ---- saturation. The headline, because it is the signal that did not exist.
    a("#### Saturation")
    a("")
    a("| Metric | Baseline | Tuned | Change |")
    a("|---|---|---|---|")
    bser, tser = bs.get("series") or {}, ts.get("series") or {}
    a(row("In-flight mean", bser.get("inFlightMean"), tser.get("inFlightMean"),
          lambda v: fmt_num(v, 2)))
    a(row("In-flight peak", bser.get("inFlightPeak"), tser.get("inFlightPeak"),
          lambda v: fmt_num(v, 0)))
    a(row("Goroutines peak", bser.get("goroutinesPeak"), tser.get("goroutinesPeak"),
          lambda v: fmt_num(v, 0)))
    a(row("Open file descriptors peak", bser.get("openFdsPeak"), tser.get("openFdsPeak"),
          lambda v: fmt_num(v, 0)))
    a("")
    a("`octo_flow_in_flight` is how many messages a flow is processing at the instant of "
      "the scrape. It is the difference between a flow that is quiet and one that is "
      "stuck: pinned at the worker count with flat throughput means every worker is "
      "blocked on something, which from outside the process looks like an idle service.")
    a("")

    # Only worth saying when the numbers support it, and only when the arm's own
    # worker count is known — a baseline arm has the tunables stripped, so its
    # worker count is whatever the runtime defaults to and the harness cannot claim
    # to know it.
    for name, s, ser in (("Baseline", bs, bser), ("Tuned", ts, tser)):
        peak = ser.get("inFlightPeak")
        declared = knobs.get("workers") if name == "Tuned" else None
        if peak and declared and str(declared).isdigit() and peak >= int(declared):
            a(f"> **{name} in-flight peaked at {peak:,.0f}, its declared `workers` value.** "
              "Every worker was occupied at that moment; more offered load cannot become more "
              "throughput without more workers or a faster block.")
            a("")

    # ---- latency as the runtime measured it.
    a("#### Latency, server-side")
    a("")
    a("| Metric | Baseline | Tuned | Change |")
    a("|---|---|---|---|")
    a(row("Flow duration mean", bs.get("flowMeanMs"), ts.get("flowMeanMs"),
          fmt_ms, higher_is_better=False))
    a(f"| Observations | {fmt_num(bs.get('flowObservations'), 0)} | "
      f"{fmt_num(ts.get('flowObservations'), 0)} | |")
    for key, label in (("flowP50", "Flow duration p50"),
                       ("flowP95", "Flow duration p95"),
                       ("flowP99", "Flow duration p99")):
        b_txt, t_txt = fmt_quantile(bs, key), fmt_quantile(ts, key)
        # No delta column: two values that are both "< 5 ms" have no percentage
        # between them, and computing one over interpolated bucket noise would be
        # the exact false precision this formatting exists to avoid.
        a(f"| {label} | {b_txt} | {t_txt} | |")
    a("")

    # The mean is the number to read here, and saying so matters because the
    # quantiles look more precise than they are.
    bounded = [k for k in ("flowP50", "flowP95", "flowP99") if bs.get(k + "Bound") == "below"]
    if bounded:
        edge = bs.get("flowP95EdgeMs") or bs.get("flowP50EdgeMs")
        a(f"> **The percentiles above are bounded, not measured.** Octo's flow-duration "
          f"histogram uses Prometheus's default buckets, whose lowest edge is "
          f"{fmt_ms(edge)} — and this flow completes well inside it, so every observation "
          "lands in the first bucket. Interpolating there yields a number that is a function "
          "of the bucket width and nothing else, so it is reported as a bound. **The mean is "
          "exact** (a counter sum over a counter count) and is the server-side latency figure "
          "to use for this scenario.")
        a("")

    client_p95 = (base.get("duration") or {}).get("p95")
    mean_ms = bs.get("flowMeanMs")
    if client_p95 and mean_ms:
        saturated = bool(base.get("droppedIterations")) or base.get("k6Exit")
        if saturated:
            a(f"Client p95 was {fmt_ms(client_p95)} while the runtime's mean flow duration was "
              f"{fmt_ms(mean_ms)}. **That gap is queueing at the load generator, not work in the "
              "runtime** — this run shed iterations, so k6's latency includes time requests spent "
              "waiting to be issued. Two figures that used to be indistinguishable from a slow "
              "server are now separable: the runtime was fast and the offered rate was too high.")
        else:
            a(f"Client p95 was {fmt_ms(client_p95)} against a mean flow duration of "
              f"{fmt_ms(mean_ms)} inside the runtime. The difference is everything outside the "
              "flow — the loopback path, connection handling, and k6's own scheduling on a host "
              "it shares with the server. The lab has always had to treat that as unquantified.")
        a("")

    # ---- cost, as the process accounts for it.
    a("#### Cost, from inside the process")
    a("")
    a("| Metric | Baseline | Tuned | Change |")
    a("|---|---|---|---|")
    a(row("CPU-ms per message", bs.get("cpuMsPerMessage"), ts.get("cpuMsPerMessage"),
          lambda v: fmt_num(v, 3, " ms"), higher_is_better=False))
    a(row("Allocated per message", bs.get("allocBytesPerMessage"), ts.get("allocBytesPerMessage"),
          fmt_bytes, higher_is_better=False))
    a(row("Allocated over window", bs.get("allocBytesWindow"), ts.get("allocBytesWindow"),
          fmt_bytes))
    a(row("GC cycles", bs.get("gcCycles"), ts.get("gcCycles"), lambda v: fmt_num(v, 0)))
    a(row("GC time", bs.get("gcSecondsWindow"), ts.get("gcSecondsWindow"),
          lambda v: fmt_num((v or 0) * 1000, 1, " ms"), higher_is_better=False))
    a(row("Heap in use peak", bser.get("heapAllocBytesPeak"), tser.get("heapAllocBytesPeak"),
          fmt_bytes))
    a("")
    a("**Allocation per message is the number to watch when tuning.** A knob that buys "
      "throughput by allocating more per message has moved the cost rather than removed "
      "it, and RSS will not show that while the collector keeps up.")
    a("")

    # Where the two instruments disagree. Not a footnote: on the container target the
    # outside view is the Docker VM's accounting of a whole container.
    os_cpu = (base.get("resources") or {}).get("cpuSecondsWindow")
    srv_cpu = bs.get("cpuSecondsWindow")
    if os_cpu and srv_cpu:
        drift = (srv_cpu - os_cpu) / os_cpu * 100
        a(f"*CPU over the window: {fmt_num(os_cpu, 2, ' s')} sampled from outside the process, "
          f"{fmt_num(srv_cpu, 2, ' s')} reported by it ({drift:+.1f}%). The two are measured by "
          "different instruments over slightly different intervals; a large divergence is worth "
          "explaining before either number is published.*")
        a("")

    # ---- per flow. A scenario with more than one root flow has more than one latency.
    flows = bs.get("flows") or {}
    if len(flows) > 1:
        a("#### Per flow")
        a("")
        a("| Flow | Messages | Mean | p95 | In-flight peak |")
        a("|---|---|---|---|---|")
        for name, f in sorted(flows.items(), key=lambda kv: kv[1]["messages"], reverse=True):
            a(f"| `{name}` | {fmt_num(f['messages'], 0)} | {fmt_ms(f.get('meanMs'))} | "
              f"{fmt_quantile(f, 'p95')} | {fmt_num(f.get('inFlightPeak'), 0)} |")
        a("")
        idle = [n for n, f in flows.items() if not f["messages"]]
        if idle:
            a(f"*{', '.join(f'`{n}`' for n in idle)} took no traffic in the window. A flow that "
              "exports at zero is visible rather than missing, which is how a flow that "
              "silently failed to receive work can be told apart from one that was never "
              "exercised.*")
            a("")

    # ---- errors, attributed.
    errs = bs.get("errorsByBlock") or {}
    if errs:
        a("#### Unhandled failures by block")
        a("")
        a("| Block | Failures |")
        a("|---|---|")
        for blk, n in sorted(errs.items(), key=lambda kv: kv[1], reverse=True):
            a(f"| `{blk}` | {fmt_num(n, 0)} |")
        a("")
        a("*Recovered failures are reported as `completed` and do not appear here, so this is "
          "an error rate rather than an incident count.*")
        a("")

    # ---- per block, when the run asked for it.
    blocks = bs.get("blocks") or ts.get("blocks") or []
    if blocks:
        a("#### Per block")
        a("")
        a("| Block | Type | Invocations | Mean | p95 | Total |")
        a("|---|---|---|---|---|---|")
        for b in blocks:
            a(f"| `{b['path']}` | `{b['type']}` | {fmt_num(b['invocations'], 0)} | "
              f"{fmt_ms(b.get('meanMs'))} | {fmt_quantile(b, 'p95')} | "
              f"{fmt_num(b['totalSeconds'], 2, ' s')} |")
        a("")
        a("> **A composite's duration includes its children's**, and each child times itself, "
          "so the Total column double-counts across nested paths. Read it to rank siblings, "
          "not to sum to the flow's time.")
        a("")
        dropped = bs.get("blockEventsDropped") or ts.get("blockEventsDropped")
        if dropped:
            a(f"> ⚠ **{dropped:,} block event(s) were dropped**, so the counts above "
              "under-report. Per-block delivery is at-most-once by design — a full queue "
              "sheds telemetry rather than slowing the flow — and this is the only way to "
              "tell that from a genuine drop in traffic.")
            a("")


def render(run_dir, env, footprint, groups, test):
    """groups: {variant: {"cells": [...], "median": cell}}"""
    hw = env.get("hardware", {})
    osi = env.get("os", {})
    base = groups.get("baseline", {}).get("median")
    tuned = groups.get("tuned", {}).get("median")

    run_id = os.path.basename(run_dir.rstrip("/"))
    lines = []
    a = lines.append

    # Jekyll front matter: the report is already publishable if Pages is enabled.
    a("---")
    a(f'title: "{run_id}"')
    a(f'scenario: "{env.get("scenario", "")}"')
    a(f'target: "{env.get("target", "")}"')
    a(f'octoVersion: "{env.get("versionUnderTest", "")}"')
    a(f'date: {env.get("capturedAt", "")}')
    a("---")
    a("")
    a(f"# {run_id}")
    a("")
    a(f"Scenario **{env.get('scenario')}** on target **{env.get('target')}**, "
      f"Octo **{env.get('versionUnderTest')}**, captured {env.get('capturedAt')}.")
    a("")
    a("> Generated by `lab/bin/report.py`. Do not edit by hand — re-run the benchmark instead.")
    a("")

    # ---- environment ----
    a("## Environment")
    a("")
    a("| | |")
    a("|---|---|")
    a(f"| Host profile | `{env.get('hostProfile')}` |")
    a(f"| CPU | {hw.get('cpuModel')} ({hw.get('arch')}) |")
    cores = f"{hw.get('logicalCores')} logical"
    if hw.get("performanceCores"):
        cores += f" — {hw['performanceCores']} performance + {hw.get('efficiencyCores', 0)} efficiency"
    a(f"| Cores | {cores} |")
    a(f"| Memory | {fmt_bytes(hw.get('memoryBytes'))} |")
    a(f"| OS | {osi.get('name')} {osi.get('version')} ({osi.get('build')}) |")
    if hw.get("cloudMachineType"):
        a(f"| Machine type | {hw['cloudMachineType']} |")
    a(f"| Octo version under test | **{env.get('versionUnderTest')}** |")
    bld = env.get("build") or {}
    src = bld.get("source") or {}
    if bld.get("channel") == "dev":
        a(f"| Build channel | **dev** — built from source, not a published release |")
        a(f"| Source commit | `{src.get('shortCommit')}` on `{src.get('branch')}`"
          f"{' — **dirty tree**' if src.get('dirty') else ''} |")
        if src.get("subject"):
            a(f"| Commit subject | {src['subject']} |")
        a(f"| Source path | `{src.get('path')}` |")
        a(f"| Go | {bld.get('goVersion')}"
          f"{', tags `' + bld['buildTags'] + '`' if bld.get('buildTags') else ''} |")
    else:
        a(f"| Build channel | release |")
    rt = env.get("runtime", {})
    a(f"| Runtime artifact | `{rt.get('artifact')}` ({fmt_bytes(rt.get('artifactBytes'))}) |")
    if env.get("container"):
        c = env["container"]
        a(f"| Image digest | `{c.get('digest')}` |")
        a(f"| Container envelope | {c.get('allocatedCpus')} CPUs, "
          f"{fmt_bytes(c.get('allocatedMemoryBytes'))} (Docker {c.get('dockerServer')}) |")
    a(f"| k6 | {env.get('tooling', {}).get('k6')} |")
    obs = env.get("observability") or {}
    if obs:
        if obs.get("adminPortAvailable"):
            bits = ["probes"]
            if obs.get("metrics"):
                bits.append("metrics")
            if obs.get("metricsBlocks"):
                bits.append(f"per-block metrics (`{obs['metricsBlocks']}`)")
            a(f"| Runtime observability | {', '.join(bits)} on the admin port |")
        else:
            a("| Runtime observability | none — this build serves no admin port |")
        a(f"| Readiness detected by | {'`/readyz`' if obs.get('readinessMethod') == 'admin' else 'polling a business route'} |")
    lab = env.get("tooling", {})
    a(f"| Lab commit | `{lab.get('labCommit')}`{' (dirty)' if lab.get('labDirty') else ''} |")
    a("")

    # Rule 1 is "no version, no result". The version above is what the harness asked
    # the artifact before starting it; identity is what the process reported once it
    # was running. Stated only when they disagree, which is when it matters.
    ident = env.get("runtimeIdentity") or {}
    if ident.get("version") and ident["version"] not in str(env.get("versionUnderTest", "")):
        a(f"> ⚠ **The running process reported version `{ident['version']}`, but the artifact "
          f"under test was recorded as `{env.get('versionUnderTest')}`.** Something other than "
          "the intended build answered — a stale process on the port, or an image whose tag "
          "moved. Do not publish this run.")
        a("")
    if ident.get("servicesModule"):
        a(f"*The process reported itself as Octo {ident['version']}, services module "
          f"`{ident['servicesModule']}`, with {fmt_num(ident.get('connectors'), 0)} connector(s) "
          f"and {fmt_num(ident.get('flows'), 0)} flow(s) loaded.*")
        a("")
    if env.get("loadGenerator", {}).get("colocatedWithServer"):
        a("> **Load generator shares this host with the server under test.** k6 and Octo compete "
          "for the same cores, which compresses the absolute ceiling. Both variants pay this cost "
          "equally, so the baseline↔tuned comparison stands; treat absolute numbers as a floor.")
        a("")
    if bld.get("channel") == "dev":
        if src.get("dirty"):
            a("> **Built from a source tree with uncommitted changes.** Nobody can check out what "
              "produced this number, so it is a working measurement and not a citable one. Commit "
              "the tree and re-run before quoting it.")
        else:
            a(f"> **Built from source at `{src.get('shortCommit')}`, not from a release.** Compare it "
              "against a release run of the same scenario on the same host to see what the change "
              "did; do not quote it as the runtime's performance, since the code has not shipped.")
        a("")
    if env.get("target") == "docker":
        a("> **Container numbers include Docker Desktop's network path.** Ports are published "
          "through a userland proxy into a Linux VM, so this measures that path as much as it "
          "measures Octo. Compare within this target only — see [METHODOLOGY.md](../../METHODOLOGY.md).")
        a("")

    # ---- footprint ----
    if footprint:
        a("## Runtime footprint")
        a("")
        a("What the runtime costs before it serves a single request.")
        a("")
        a("| | |")
        a("|---|---|")
        a(f"| Artifact size | {fmt_bytes(footprint.get('artifactBytes'))} |")
        a(f"| Cold start (launch → first 200) | {fmt_num(footprint.get('coldStartMs'), 0, ' ms')} |")
        a(f"| Idle RSS (after {footprint.get('idleHoldSeconds')}s idle) | "
          f"{fmt_bytes(footprint.get('idleRssBytesMean'))} "
          f"(peak {fmt_bytes(footprint.get('idleRssBytesMax'))}) |")
        a(f"| Idle CPU | {fmt_num(footprint.get('idleCpuPctMean'), 2, '%')} "
          f"(peak {fmt_num(footprint.get('idleCpuPctMax'), 2, '%')}) |")
        # From inside the process, when it could be asked. RSS says how much memory
        # the OS gave the process; the heap says how much of it the runtime is
        # actually using, and the two differ by however much the Go allocator is
        # holding in reserve.
        if footprint.get("idleHeapBytesMean") is not None:
            a(f"| Idle heap in use | {fmt_bytes(footprint.get('idleHeapBytesMean'))} |")
        if footprint.get("idleGoroutinesMean") is not None:
            a(f"| Idle goroutines | {fmt_num(footprint.get('idleGoroutinesMean'), 1)} "
              f"(peak {fmt_num(footprint.get('idleGoroutinesMax'), 0)}) |")
        if footprint.get("idleAllocBytesPerSecond") is not None:
            a(f"| Idle allocation rate | {fmt_bytes(footprint.get('idleAllocBytesPerSecond'))}/s |")
        a("")
        if footprint.get("readinessMethod") == "admin":
            a("*Cold start is launch → `/readyz` reporting `ready`, meaning every connector and "
              "flow started. Runs recorded before the runtime had an admin port measured launch → "
              "first 200 from a business route, which is a later moment; the two are not "
              "directly comparable.*")
            a("")
        if footprint.get("idleAllocBytesPerSecond"):
            a("*A non-zero idle allocation rate is partly the metrics scrape itself: rendering "
              "the exposition allocates, once per sample interval, on the observed process. It "
              "is a floor on what the runtime allocates while serving nothing, not a measurement "
              "of it.*")
            a("")

    if not base:
        a("## Results")
        a("")
        a("No baseline measurement was recorded — nothing to compare.")
        return "\n".join(lines) + "\n"

    reps = len(groups.get("baseline", {}).get("cells", []))
    a("## Results")
    a("")
    a(f"Test `{test}`, {reps} repetition(s) per variant, median repetition shown. "
      f"Offered rate {fmt_num(base.get('offeredRate'), 0)} req/s for "
      f"{fmt_num(base.get('durationSeconds'), 0, 's')}.")
    a("")

    # Knob names are per-scenario, so render whatever the run actually recorded
    # rather than assuming the root-flow trio.
    knobs = (tuned or {}).get("knobs", {}) or (base or {}).get("knobs", {})
    names = list(knobs.keys())
    a(f"**Baseline** — {', '.join(f'`{n}`' for n in names) or 'the tunables'} stripped from the "
      "config, so the runtime uses whatever it defaults to.  ")
    if tuned:
        tk = tuned.get("knobs", {})
        a("**Tuned** — " + ", ".join(f"{n} `{tk.get(n, 'as declared')}`" for n in names) + ".")
    a("")

    # ---- validity first: the methodology says read these before anything else ----
    a("### Validity")
    a("")
    a("| Check | Baseline | Tuned | |")
    a("|---|---|---|---|")
    a(row("Dropped iterations", base.get("droppedIterations"),
          (tuned or {}).get("droppedIterations"), lambda v: fmt_num(v, 0)))
    a(row("Failed requests", base.get("failedRate"),
          (tuned or {}).get("failedRate"), lambda v: fmt_num((v or 0) * 100, 3, "%")))
    a(row("k6 threshold exit", base.get("k6Exit"),
          (tuned or {}).get("k6Exit"), lambda v: "pass" if v == 0 else f"breach ({v})"))

    # Server-side outcomes. The client cannot see these: a message the runtime
    # dropped or failed after the response went out is a 200 to k6.
    bs, ts = base.get("server", {}), (tuned or {}).get("server", {})
    if bs.get("available") or ts.get("available"):
        a(row("Messages failed (server)", (bs.get("messages") or {}).get("failed"),
              (ts.get("messages") or {}).get("failed"), lambda v: fmt_num(v, 0)))
        a(row("Messages dropped (server)", (bs.get("messages") or {}).get("dropped"),
              (ts.get("messages") or {}).get("dropped"), lambda v: fmt_num(v, 0)))
        a(row("Ready throughout window", (bs.get("series") or {}).get("readyThroughout"),
              (ts.get("series") or {}).get("readyThroughout"),
              lambda v: "n/a" if v is None else ("yes" if v else "**no**")))
    a("")

    warn_bits = []
    if base.get("droppedIterations") or (tuned or {}).get("droppedIterations"):
        warn_bits.append("non-zero dropped iterations — the offered rate exceeded what the server "
                         "could start, so these latency figures describe a **saturated** system")
    if base.get("failedRate") or (tuned or {}).get("failedRate"):
        warn_bits.append("non-zero failure rate — explain before publishing")

    # The case worth catching: the client saw nothing wrong and the runtime did.
    for name, s, client_failed in (("baseline", bs, base.get("failedRate")),
                                   ("tuned", ts, (tuned or {}).get("failedRate"))):
        msgs = s.get("messages") or {}
        server_bad = (msgs.get("failed") or 0) + (msgs.get("dropped") or 0)
        if server_bad and not client_failed:
            warn_bits.append(
                f"**{name} reported {server_bad:,} failed or dropped message(s) server-side while "
                "k6 saw a 100% success rate** — the runtime shed work the client was told had "
                "succeeded, which no client-side metric can show")
    for name, s in (("baseline", bs), ("tuned", ts)):
        if (s.get("series") or {}).get("readyThroughout") is False:
            warn_bits.append(f"**{name} went not-ready during the measured window** — the run "
                             "describes a runtime that stopped serving, not a slow one")
    if warn_bits:
        a("> ⚠ " + "; ".join(warn_bits) + ".")
        a("")

    # ---- throughput and latency ----
    a("### Throughput and latency")
    a("")
    a("| Metric | Baseline | Tuned | Change |")
    a("|---|---|---|---|")
    t = tuned or {}
    a(row("Achieved throughput", base.get("achievedRps"), t.get("achievedRps"),
          lambda v: fmt_num(v, 1, " req/s"), higher_is_better=True))
    a(row("Requests", base.get("requests"), t.get("requests"), lambda v: fmt_num(v, 0)))
    for key, label in (("med", "Latency p50"), ("p90", "Latency p90"),
                       ("p95", "Latency p95"), ("p99", "Latency p99"), ("max", "Latency max")):
        a(row(label, base["duration"].get(key), t.get("duration", {}).get(key),
              fmt_ms, higher_is_better=False))
    a(row("Server think time p95", base["waiting"].get("p95"), t.get("waiting", {}).get("p95"),
          fmt_ms, higher_is_better=False))
    a(row("Data received", base.get("dataReceivedRate"), t.get("dataReceivedRate"),
          lambda v: "n/a" if v is None else f"{v / 1048576:.2f} MiB/s"))
    a(row("Peak VUs", base.get("vusMax"), t.get("vusMax"), lambda v: fmt_num(v, 0)))
    a("")

    # ---- cost ----
    a("### Resource cost")
    a("")
    a("Measured over the load window only, so start-up and warm-up are excluded.")
    a("")
    a("| Metric | Baseline | Tuned | Change |")
    a("|---|---|---|---|")
    br, tr = base.get("resources", {}), t.get("resources", {})
    a(row("**CPU-ms per request**", base.get("cpuMsPerRequest"), t.get("cpuMsPerRequest"),
          lambda v: fmt_num(v, 3, " ms"), higher_is_better=False))
    a(row("Requests per CPU-second", base.get("requestsPerCpuSecond"), t.get("requestsPerCpuSecond"),
          lambda v: fmt_num(v, 0), higher_is_better=True))
    a(row("CPU consumed", br.get("cpuSecondsWindow"), tr.get("cpuSecondsWindow"),
          lambda v: fmt_num(v, 2, " s")))
    a(row("CPU mean", br.get("cpuPctMean"), tr.get("cpuPctMean"),
          lambda v: fmt_num(v, 1, "%")))
    a(row("CPU peak", br.get("cpuPctPeak"), tr.get("cpuPctPeak"),
          lambda v: fmt_num(v, 1, "%")))
    a(row("RSS mean", br.get("rssBytesMean"), tr.get("rssBytesMean"), fmt_bytes))
    a(row("RSS peak", br.get("rssBytesPeak"), tr.get("rssBytesPeak"),
          fmt_bytes, higher_is_better=False))
    a(row("RSS drift over run", br.get("rssBytesDrift"), tr.get("rssBytesDrift"), fmt_bytes))
    a(row("RSS per 1k req/s", base.get("rssBytesPer1kRps"), t.get("rssBytesPer1kRps"),
          fmt_bytes, higher_is_better=False))
    a("")
    a("*100% CPU = one fully saturated core.*")
    a("")

    # ---- what the process said about itself ----
    if bs.get("available") or ts.get("available"):
        render_server_side(a, base, tuned, bs, ts, knobs)

    # ---- spread across reps ----
    a("### Repetition spread")
    a("")
    a("| Variant | Rep | Achieved req/s | p95 | CPU-ms/req | Peak RSS |")
    a("|---|---|---|---|---|---|")
    for variant in ("baseline", "tuned"):
        g = groups.get(variant)
        if not g:
            continue
        for i, c in enumerate(g["cells"], 1):
            marker = " ←median" if c is g["median"] else ""
            a(f"| {variant}{marker} | {i} | {fmt_num(c.get('achievedRps'), 1)} | "
              f"{fmt_ms(c['duration'].get('p95'))} | {fmt_num(c.get('cpuMsPerRequest'), 3)} | "
              f"{fmt_bytes(c.get('resources', {}).get('rssBytesPeak'))} |")
    a("")

    a("---")
    a("")
    a("Read [METHODOLOGY.md](../../METHODOLOGY.md) for metric definitions and caveats. "
      "Raw artifacts for every repetition are in this directory.")
    a("")
    return "\n".join(lines)


# -------------------------------------------------------------------- main ----


def main():
    if len(sys.argv) < 2:
        print("usage: report.py <run-dir>", file=sys.stderr)
        return 2
    run_dir = os.path.abspath(sys.argv[1])

    env = load_json(os.path.join(run_dir, "env.json"), {})
    footprint = load_json(os.path.join(run_dir, "footprint.json"), {})

    # What the running process said it was, captured during the smoke gate. Folded
    # into env so the report has one place to read provenance from.
    identity = load_json(os.path.join(run_dir, "runtime-identity.json"))
    if identity:
        env["runtimeIdentity"] = identity

    # Collect cells: <variant>-<test>-rep<N>
    pattern = re.compile(r"^(?P<variant>[a-z0-9]+)-(?P<test>[a-z0-9]+)-rep(?P<rep>\d+)$")
    groups, test = {}, None
    for name in sorted(os.listdir(run_dir)):
        m = pattern.match(name)
        if not m:
            continue
        cell = load_cell(os.path.join(run_dir, name))
        if not cell:
            continue
        test = m.group("test")
        groups.setdefault(m.group("variant"), {"cells": []})["cells"].append(cell)

    for g in groups.values():
        g["median"] = median_cell(g["cells"])

    md = render(run_dir, env, footprint, groups, test or "unknown")
    with open(os.path.join(run_dir, "REPORT.md"), "w") as fh:
        fh.write(md)

    # Machine-readable headline for results/index.md and regression tracking.
    base = groups.get("baseline", {}).get("median") or {}
    tuned = groups.get("tuned", {}).get("median") or {}
    result = {
        "runId": os.path.basename(run_dir),
        "generatedAt": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "scenario": env.get("scenario"),
        "target": env.get("target"),
        "hostProfile": env.get("hostProfile"),
        "octoVersion": env.get("versionUnderTest"),
        "buildChannel": env.get("buildChannel", "release"),
        "build": env.get("build"),
        "test": test,
        "reps": len(groups.get("baseline", {}).get("cells", [])),
        "footprint": footprint,
        "baseline": {k: base.get(k) for k in
                     ("achievedRps", "cpuMsPerRequest", "rssBytesPer1kRps",
                      "droppedIterations", "failedRate")},
        "tuned": {k: tuned.get(k) for k in
                  ("achievedRps", "cpuMsPerRequest", "rssBytesPer1kRps",
                   "droppedIterations", "failedRate")},
    }
    result["baseline"]["p95Ms"] = base.get("duration", {}).get("p95")
    result["tuned"]["p95Ms"] = tuned.get("duration", {}).get("p95")
    result["tuned"]["knobs"] = tuned.get("knobs")

    # Server-side headlines, so the regression view can track the runtime's own view
    # of itself across versions rather than only the client's. Kept to the figures
    # that mean the same thing in every scenario.
    result["observability"] = env.get("observability")
    result["runtimeIdentity"] = env.get("runtimeIdentity")
    for name, cell in (("baseline", base), ("tuned", tuned)):
        srv = cell.get("server") or {}
        if not srv.get("available"):
            continue
        result[name]["server"] = {
            "messagesPerSecond": srv.get("messagesPerSecond"),
            "cpuMsPerMessage": srv.get("cpuMsPerMessage"),
            "allocBytesPerMessage": srv.get("allocBytesPerMessage"),
            "flowP95Ms": srv.get("flowP95Ms"),
            "inFlightPeak": (srv.get("series") or {}).get("inFlightPeak"),
            "messagesFailed": (srv.get("messages") or {}).get("failed"),
            "messagesDropped": (srv.get("messages") or {}).get("dropped"),
        }

    with open(os.path.join(run_dir, "result.json"), "w") as fh:
        json.dump(result, fh, indent=2)
        fh.write("\n")

    print(f"  report -> {os.path.join(run_dir, 'REPORT.md')}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
