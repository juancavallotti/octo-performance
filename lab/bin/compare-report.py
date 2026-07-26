#!/usr/bin/env python3
"""Put two runs of the same scenario side by side and state the delta.

Usage: compare-report.py <run-dir> <run-dir> [<run-dir> ...]

Written for the released-versus-source-build question, but it does not care why
two runs differ: it takes the first as the reference and expresses every later
one against it.

The one thing it refuses to do is present a delta as a result without saying how
much of it could be noise. Each arm's repetitions give a spread; a delta inside
that spread is labelled as such, because three repetitions on a laptop that
throttles cannot resolve a few percent, and a table that prints "+3.1%" with no
qualifier will be read as an improvement.
"""

import json
import os
import statistics
import sys

# metric key -> (label, better direction, digits, suffix)
METRICS = [
    ("achievedRps",      "Throughput (req/s)",  "up",   1, ""),
    ("p95Ms",            "p95 latency",         "down", 2, " ms"),
    ("cpuMsPerRequest",  "CPU-ms per request",  "down", 3, ""),
    ("rssBytesPer1kRps", "RSS per 1k req/s",    "down", 0, " B"),
]

FOOTPRINT = [
    ("coldStartMs",       "Cold start",   "down", 0, " ms"),
    ("idleRssBytesMean",  "Idle RSS",     "down", 0, " B"),
    ("artifactBytes",     "Artifact size","down", 0, " B"),
]


def load(path, default=None):
    try:
        with open(path) as fh:
            return json.load(fh)
    except (OSError, json.JSONDecodeError):
        return default


def fmt(v, digits=1, suffix=""):
    if v is None:
        return "n/a"
    if suffix == " B":
        return f"{v / 1048576:.1f} MiB"
    return f"{v:,.{digits}f}{suffix}"


def delta(ref, new, better):
    """Percent change, signed so that a positive number always means better."""
    if not ref or new is None:
        return None, "n/a"
    pct = (new - ref) / ref * 100
    shown = pct if better == "up" else -pct
    return shown, f"{pct:+.1f}%"


def rep_rates(run_dir, variant):
    """Every repetition's achieved rate, so the noise floor can be stated."""
    rates = []
    for name in sorted(os.listdir(run_dir)):
        if not name.startswith(f"{variant}-") or "-rep" not in name:
            continue
        s = load(os.path.join(run_dir, name, "summary.json"))
        if not s:
            continue
        r = ((s.get("metrics") or {}).get("http_reqs") or {}).get("rate")
        if r:
            rates.append(r)
    return rates


def spread_pct(rates):
    """Rep-to-rep spread as a percentage of the median."""
    if len(rates) < 2:
        return None
    med = statistics.median(rates)
    if not med:
        return None
    return (max(rates) - min(rates)) / med * 100


def arm_label(env, result):
    """How this arm is named in the tables — build channel plus commit."""
    version = result.get("octoVersion") or env.get("versionUnderTest") or "unknown"
    bld = env.get("build") or result.get("build") or {}
    if (bld.get("channel") or result.get("buildChannel")) == "dev":
        src = bld.get("source") or {}
        d = " (dirty)" if src.get("dirty") else ""
        return f"dev `{src.get('shortCommit', '?')}`{d}", version
    return "release", version


def main():
    dirs = [os.path.abspath(d) for d in sys.argv[1:]]
    if len(dirs) < 2:
        print("  need at least two run directories", file=sys.stderr)
        return 1

    arms = []
    for d in dirs:
        result = load(os.path.join(d, "result.json"))
        env = load(os.path.join(d, "env.json"), {})
        if not result:
            print(f"  {d} has no result.json — was the run completed?", file=sys.stderr)
            return 1
        channel, version = arm_label(env, result)
        arms.append({
            "dir": d, "id": os.path.basename(d), "result": result, "env": env,
            "channel": channel, "version": version,
            "reps": {v: rep_rates(d, v) for v in ("baseline", "tuned")},
        })

    ref = arms[0]
    r0 = ref["result"]
    scenario = r0.get("scenario", "")
    target = r0.get("target", "")
    host = r0.get("hostProfile", "")

    # Comparing runs of different scenarios or targets would produce a table that
    # looks meaningful and is not.
    for a in arms[1:]:
        r = a["result"]
        if (r.get("scenario"), r.get("target"), r.get("hostProfile")) != (scenario, target, host):
            print(f"  {a['id']} is a different scenario/target/host than the reference — "
                  "these are not comparable", file=sys.stderr)
            return 1

    versions = "-vs-".join(a["version"] for a in arms)
    out_dir = os.path.join(os.path.dirname(dirs[0]),
                           f"compare-{scenario}-{target}-{versions}")
    n = 2
    base_out = out_dir
    while os.path.exists(out_dir):
        out_dir = f"{base_out}-{n}"
        n += 1
    os.makedirs(out_dir)

    lines = []
    a = lines.append
    a("---")
    a(f'title: "Compare — {scenario} · {target}"')
    a(f'scenario: "{scenario}"')
    a(f'target: "{target}"')
    a("---")
    a("")
    a(f"# {scenario} · {target} — {' vs '.join(x['version'] for x in arms)}")
    a("")
    a(f"Same scenario, same target, same host (`{host}`), measured back to back in one "
      f"invocation of `task compare`. The arms differ in the build under test and "
      f"nothing else.")
    a("")

    # ---- what is being compared ----
    a("## Arms")
    a("")
    a("| | Build | Version | Source | Run |")
    a("|---|---|---|---|---|")
    for i, x in enumerate(arms):
        bld = (x["env"].get("build") or {}).get("source") or {}
        where = f"`{bld.get('shortCommit')}` on `{bld.get('branch')}`" if bld else "published release"
        role = "reference" if i == 0 else "compared"
        a(f"| {role} | {x['channel']} | `{x['version']}` | {where} | "
          f"[`{x['id']}`](../{x['id']}/REPORT.md) |")
    a("")
    for x in arms:
        bld = (x["env"].get("build") or {}).get("source") or {}
        if bld.get("subject"):
            a(f"- `{bld.get('shortCommit')}` — {bld['subject']}")
    if any(((x["env"].get("build") or {}).get("source") or {}).get("dirty") for x in arms):
        a("")
        a("> **One of these arms was built from a tree with uncommitted changes.** The delta is "
          "real for this working copy and reproducible by nobody else. Commit and re-run before "
          "quoting it.")
    a("")

    # ---- the noise floor ----
    a("## Noise floor")
    a("")
    a("Rep-to-rep spread within each arm, as a percentage of that arm's median "
      "throughput. A delta smaller than this is not evidence of anything.")
    a("")
    a("| Arm | Variant | Reps | Spread |")
    a("|---|---|---|---|")
    floor = 2.0  # never claim resolution finer than this on a laptop
    for x in arms:
        for variant in ("baseline", "tuned"):
            rates = x["reps"].get(variant) or []
            sp = spread_pct(rates)
            if sp is not None:
                floor = max(floor, sp)
            a(f"| {x['channel']} | {variant} | {len(rates)} | "
              f"{'n/a' if sp is None else f'{sp:.1f}%'} |")
    a("")
    a(f"Deltas below **{floor:.1f}%** are reported as *within noise* below.")
    a("")

    # ---- the comparison ----
    for variant in ("baseline", "tuned"):
        if not any((x["result"].get(variant) or {}).get("achievedRps") for x in arms):
            continue
        a(f"## {variant.capitalize()}")
        a("")
        header = "| Metric | " + " | ".join(x["channel"] for x in arms) + " | Change |"
        a(header)
        a("|---" * (len(arms) + 2) + "|")
        for key, label, better, digits, suffix in METRICS:
            vals = [(x["result"].get(variant) or {}).get(key) for x in arms]
            cells = " | ".join(fmt(v, digits, suffix) for v in vals)
            signed, shown = delta(vals[0], vals[-1], better)
            if signed is None:
                verdict = "n/a"
            elif abs(signed) < floor:
                verdict = f"{shown} · within noise"
            else:
                verdict = f"{shown} · {'better' if signed > 0 else 'worse'}"
            a(f"| {label} | {cells} | {verdict} |")
        a("")

        # Correctness before speed: an arm that shed load is not faster.
        for x in arms:
            v = x["result"].get(variant) or {}
            if (v.get("failedRate") or 0) > 0.001 or (v.get("droppedIterations") or 0) > 0:
                a(f"> **{x['channel']} / {variant} did not serve the full offered load** — "
                  f"{(v.get('failedRate') or 0) * 100:.2f}% failed, "
                  f"{v.get('droppedIterations') or 0:,.0f} dropped iterations. Its throughput "
                  "number describes a server past its limit, not a faster one.")
                a("")

    # ---- footprint ----
    a("## Footprint")
    a("")
    a("Properties of the artifact rather than of the load: what the build costs before "
      "it serves anything.")
    a("")
    a("| Metric | " + " | ".join(x["channel"] for x in arms) + " | Change |")
    a("|---" * (len(arms) + 2) + "|")
    for key, label, better, digits, suffix in FOOTPRINT:
        vals = [(x["result"].get("footprint") or {}).get(key) for x in arms]
        cells = " | ".join(fmt(v, digits, suffix) for v in vals)
        _, shown = delta(vals[0], vals[-1], better)
        a(f"| {label} | {cells} | {shown} |")
    a("")
    a("> Artifact size is not a like-for-like comparison when one arm is a release and the "
      "other a local build: the released binary is stripped and the `go build` here is not. "
      "A difference of tens of megabytes on that row is the build flags, not the code.")
    a("")

    a("---")
    a("")
    a("Generated by `lab/bin/compare-report.py`. Do not edit by hand. Numbers are the median "
      "repetition of each arm; see [METHODOLOGY.md](../../METHODOLOGY.md).")
    a("")

    with open(os.path.join(out_dir, "REPORT.md"), "w") as fh:
        fh.write("\n".join(lines))

    with open(os.path.join(out_dir, "result.json"), "w") as fh:
        json.dump({
            "runId": os.path.basename(out_dir),
            "test": "compare",
            "scenario": scenario,
            "target": target,
            "hostProfile": host,
            "noiseFloorPct": round(floor, 2),
            "arms": [{
                "runId": x["id"],
                "channel": x["channel"],
                "octoVersion": x["version"],
                "build": x["env"].get("build"),
                "baseline": x["result"].get("baseline"),
                "tuned": x["result"].get("tuned"),
                "footprint": x["result"].get("footprint"),
            } for x in arms],
        }, fh, indent=2)
        fh.write("\n")

    print(f"  compare -> {os.path.join(out_dir, 'REPORT.md')}")

    # A short console summary, so the terminal answers the question too.
    for variant in ("baseline", "tuned"):
        vals = [(x["result"].get(variant) or {}).get("achievedRps") for x in arms]
        if not vals[0] or vals[-1] is None:
            continue
        signed, shown = delta(vals[0], vals[-1], "up")
        note = " (within noise)" if abs(signed) < floor else ""
        print(f"    {variant:9s} {fmt(vals[0])} -> {fmt(vals[-1])} req/s  {shown}{note}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
