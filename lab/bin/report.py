#!/usr/bin/env python3
"""Turn a run directory into REPORT.md plus a machine-readable result.json.

Usage: report.py <run-dir>

Reads env.json, footprint.json, and every <variant>-<test>-rep<N>/ cell. Picks the
median repetition by achieved throughput and renders the baseline-vs-tuned
comparison the methodology calls for.

CPU cost is computed from the *measurement window* — the delta of the cumulative
CPU counter across resources.csv — not from whole-process totals, which would also
include start-up and warm-up and so overstate per-request cost.
"""

import csv
import json
import os
import re
import statistics
import sys
from datetime import datetime, timezone

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
    }

    # Derived efficiency numbers — the point of the exercise.
    if cpu_window and reqs:
        cell["cpuMsPerRequest"] = round(cpu_window * 1000 / reqs, 4)
        cell["requestsPerCpuSecond"] = round(reqs / cpu_window, 1)
    else:
        cell["cpuMsPerRequest"] = None
        cell["requestsPerCpuSecond"] = None

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
    rt = env.get("runtime", {})
    a(f"| Runtime artifact | `{rt.get('artifact')}` ({fmt_bytes(rt.get('artifactBytes'))}) |")
    if env.get("container"):
        c = env["container"]
        a(f"| Image digest | `{c.get('digest')}` |")
        a(f"| Container envelope | {c.get('allocatedCpus')} CPUs, "
          f"{fmt_bytes(c.get('allocatedMemoryBytes'))} (Docker {c.get('dockerServer')}) |")
    a(f"| k6 | {env.get('tooling', {}).get('k6')} |")
    lab = env.get("tooling", {})
    a(f"| Lab commit | `{lab.get('labCommit')}`{' (dirty)' if lab.get('labDirty') else ''} |")
    a("")
    if env.get("loadGenerator", {}).get("colocatedWithServer"):
        a("> **Load generator shares this host with the server under test.** k6 and Octo compete "
          "for the same cores, which compresses the absolute ceiling. Both variants pay this cost "
          "equally, so the baseline↔tuned comparison stands; treat absolute numbers as a floor.")
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
    a("")
    warn_bits = []
    if base.get("droppedIterations") or (tuned or {}).get("droppedIterations"):
        warn_bits.append("non-zero dropped iterations — the offered rate exceeded what the server "
                         "could start, so these latency figures describe a **saturated** system")
    if base.get("failedRate") or (tuned or {}).get("failedRate"):
        warn_bits.append("non-zero failure rate — explain before publishing")
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

    with open(os.path.join(run_dir, "result.json"), "w") as fh:
        json.dump(result, fh, indent=2)
        fh.write("\n")

    print(f"  report -> {os.path.join(run_dir, 'REPORT.md')}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
