#!/usr/bin/env python3
"""Sample the server under test at a fixed interval and write a CSV time series.

Usage:
    sample-resources.py --target native --id <pid> --out resources.csv [--interval 1.0]
    sample-resources.py --target docker --id <cid> --out resources.csv [--interval 1.0]

Runs until SIGTERM/SIGINT, flushing each row so a killed sampler still leaves
usable data.

CPU is derived by differentiating a *cumulative* counter between samples rather
than by reading an instantaneous percentage. On macOS `ps -o %cpu` is a decaying
average over the whole process lifetime, which would systematically understate a
short benchmark window; `cputime` deltas give the exact CPU-seconds consumed in
each interval. The container target uses cpu_stats.cpu_usage.total_usage from the
Docker Engine API for the same reason.

Columns:
    ts_unix           wall clock at the sample
    elapsed_s         seconds since sampling began
    cpu_seconds_total cumulative CPU seconds consumed by the server
    cpu_pct           CPU over the interval; 100% == one fully saturated core
    rss_bytes         resident set size
"""

import argparse
import csv
import json
import os
import signal
import subprocess
import sys
import time

LAB_BIN = os.path.dirname(os.path.abspath(__file__))

_running = True


def _stop(_signum, _frame):
    global _running
    _running = False


def parse_cputime(s):
    """Parse ps cputime — '[DD-]HH:MM:SS[.ss]' or 'MM:SS.ss' — into seconds."""
    s = s.strip()
    if not s:
        return None
    days = 0
    if "-" in s:
        d, s = s.split("-", 1)
        days = int(d)
    parts = s.split(":")
    try:
        parts = [float(p) for p in parts]
    except ValueError:
        return None
    total = 0.0
    for p in parts:
        total = total * 60 + p
    return total + days * 86400


def sample_native(pid):
    """-> (cpu_seconds_total, rss_bytes) or None when the process is gone."""
    try:
        out = subprocess.run(
            ["ps", "-o", "cputime=,rss=", "-p", str(pid)],
            capture_output=True, text=True, timeout=5,
        ).stdout.strip()
    except (subprocess.SubprocessError, OSError):
        return None
    if not out:
        return None
    fields = out.split()
    if len(fields) < 2:
        return None
    cpu = parse_cputime(fields[0])
    if cpu is None:
        return None
    return cpu, int(fields[1]) * 1024  # ps reports RSS in KiB


def sample_docker(cid):
    """-> (cpu_seconds_total, rss_bytes) or None when the container is gone."""
    try:
        out = subprocess.run(
            [os.path.join(LAB_BIN, "docker-stats.sh"), cid],
            capture_output=True, text=True, timeout=10,
        ).stdout.strip()
    except (subprocess.SubprocessError, OSError):
        return None
    if not out:
        return None
    try:
        d = json.loads(out)
    except json.JSONDecodeError:
        return None
    if "cpuNanos" not in d:
        return None
    return d["cpuNanos"] / 1e9, d.get("rssBytes", 0)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--target", required=True, choices=("native", "docker"))
    ap.add_argument("--id", required=True, help="pid for native, container id for docker")
    ap.add_argument("--out", required=True)
    ap.add_argument("--interval", type=float, default=1.0)
    args = ap.parse_args()

    signal.signal(signal.SIGTERM, _stop)
    signal.signal(signal.SIGINT, _stop)

    read = sample_native if args.target == "native" else sample_docker

    os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)
    with open(args.out, "w", newline="") as fh:
        w = csv.writer(fh)
        w.writerow(["ts_unix", "elapsed_s", "cpu_seconds_total", "cpu_pct", "rss_bytes"])
        fh.flush()

        t0 = time.time()
        prev_cpu = None
        prev_t = None

        while _running:
            now = time.time()
            got = read(args.id)
            if got is not None:
                cpu_total, rss = got
                if prev_cpu is not None and now > prev_t:
                    cpu_pct = (cpu_total - prev_cpu) / (now - prev_t) * 100.0
                else:
                    cpu_pct = ""  # first sample has no interval to differentiate over
                w.writerow([
                    round(now, 3),
                    round(now - t0, 3),
                    round(cpu_total, 4),
                    round(cpu_pct, 2) if cpu_pct != "" else "",
                    int(rss),
                ])
                fh.flush()
                prev_cpu, prev_t = cpu_total, now

            # Sleep in slices so a signal is honoured promptly.
            deadline = now + args.interval
            while _running and time.time() < deadline:
                time.sleep(min(0.1, max(0.0, deadline - time.time())))

    return 0


if __name__ == "__main__":
    sys.exit(main())
