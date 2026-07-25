#!/usr/bin/env python3
"""Rank a sweep's combinations and name a winner.

Usage: sweep-report.py <sweep-dir>

Ranking is lexicographic and deliberately conservative:
  1. valid combinations first (no dropped iterations, no failures)
  2. then higher achieved throughput
  3. then lower p95 latency
  4. then lower CPU-ms per request

Throughput leads because that is what the offered rate is probing, but a
combination that buys throughput with a materially worse CPU cost is visible in
the table rather than hidden behind the winner.
"""

import json
import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from report import load_cell, load_json, fmt_bytes, fmt_num  # noqa: E402

CELL_RE = re.compile(r"^w(?P<w>\d+)-b(?P<b>\d+)-p(?P<p>\d+)$")


def main():
    if len(sys.argv) < 2:
        print("usage: sweep-report.py <sweep-dir>", file=sys.stderr)
        return 2
    sweep_dir = os.path.abspath(sys.argv[1])
    env = load_json(os.path.join(sweep_dir, "env.json"), {})

    cells = []
    for name in sorted(os.listdir(sweep_dir)):
        m = CELL_RE.match(name)
        if not m:
            continue
        c = load_cell(os.path.join(sweep_dir, name))
        if not c:
            continue
        c["w"], c["b"], c["p"] = int(m["w"]), int(m["b"]), int(m["p"])
        c["valid"] = not c.get("droppedIterations") and not c.get("failedRate")
        cells.append(c)

    if not cells:
        print("error: no completed combinations in the sweep", file=sys.stderr)
        return 1

    cells.sort(key=lambda c: (
        not c["valid"],
        -(c.get("achievedRps") or 0),
        c["duration"].get("p95") or float("inf"),
        c.get("cpuMsPerRequest") or float("inf"),
    ))
    winner = cells[0]

    lines = []
    a = lines.append
    a("---")
    a(f'title: "Sweep {os.path.basename(sweep_dir)}"')
    a("---")
    a("")
    a(f"# Tuning sweep — {os.path.basename(sweep_dir)}")
    a("")
    a(f"Scenario **{env.get('scenario')}**, target **{env.get('target')}**, "
      f"Octo **{env.get('versionUnderTest')}** on `{env.get('hostProfile')}`.")
    a("")
    a("One repetition per combination — this is a search, not a publication. Re-run "
      "`task bench` with the winning knobs to produce numbers worth publishing.")
    a("")
    a("## Winner")
    a("")
    a(f"```")
    a(f"workers: {winner['w']}   buffer: {winner['b']}   pool: {winner['p']}")
    a(f"```")
    a("")
    a(f"{fmt_num(winner.get('achievedRps'), 1, ' req/s')}, "
      f"p95 {fmt_num(winner['duration'].get('p95'), 2, ' ms')}, "
      f"{fmt_num(winner.get('cpuMsPerRequest'), 3, ' CPU-ms/req')}.")
    a("")
    a("Run it with:")
    a("")
    a("```bash")
    a(f"task bench SCENARIO={env.get('scenario')} TARGET={env.get('target')} \\")
    a(f"  TUNED_WORKERS={winner['w']} TUNED_BUFFER={winner['b']} TUNED_POOL={winner['p']}")
    a("```")
    a("")
    a("## All combinations")
    a("")
    a("| # | workers | buffer | pool | req/s | p95 | p99 | CPU-ms/req | Peak RSS | Dropped | Failed |")
    a("|---|---|---|---|---|---|---|---|---|---|---|")
    for i, c in enumerate(cells, 1):
        flag = "" if c["valid"] else " ⚠"
        a(f"| {i}{flag} | {c['w']} | {c['b']} | {c['p']} | "
          f"{fmt_num(c.get('achievedRps'), 1)} | "
          f"{fmt_num(c['duration'].get('p95'), 2, ' ms')} | "
          f"{fmt_num(c['duration'].get('p99'), 2, ' ms')} | "
          f"{fmt_num(c.get('cpuMsPerRequest'), 3)} | "
          f"{fmt_bytes(c.get('resources', {}).get('rssBytesPeak'))} | "
          f"{fmt_num(c.get('droppedIterations'), 0)} | "
          f"{fmt_num((c.get('failedRate') or 0) * 100, 2, '%')} |")
    a("")
    if any(not c["valid"] for c in cells):
        a("⚠ = dropped iterations or failed requests: the server did not keep up with the "
          "offered rate, so that row's latency describes a saturated system.")
        a("")

    with open(os.path.join(sweep_dir, "SWEEP.md"), "w") as fh:
        fh.write("\n".join(lines))

    with open(os.path.join(sweep_dir, "winner.json"), "w") as fh:
        json.dump({
            "workers": winner["w"], "buffer": winner["b"], "pool": winner["p"],
            "achievedRps": winner.get("achievedRps"),
            "p95Ms": winner["duration"].get("p95"),
            "cpuMsPerRequest": winner.get("cpuMsPerRequest"),
        }, fh, indent=2)
        fh.write("\n")

    print(f"  winner: workers={winner['w']} buffer={winner['b']} pool={winner['p']} "
          f"({fmt_num(winner.get('achievedRps'), 1)} req/s, "
          f"p95 {fmt_num(winner['duration'].get('p95'), 2)} ms)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
