#!/usr/bin/env python3
"""Scrape the runtime's own Prometheus endpoint for the duration of a measurement.

Usage:
    scrape-metrics.py --url http://localhost:39999/metrics --out <cell>/metrics.csv
                      [--interval 1.0]

Writes three artifacts beside --out, and the split between them is the point:

    metrics.csv         gauges over time — the shape of the window
    metrics-start.prom  the first successful scrape, verbatim
    metrics-end.prom    the last successful scrape, verbatim

Counters and histograms are cumulative since the process started, so a single
scrape cannot answer "what happened during the load window" — it answers "what has
happened since boot", which for this harness includes the smoke gate and the
warm-up. Differencing the two snapshots is what makes the window exact, and it is
exact rather than sampled: report.py subtracts bucket from bucket, so a server-side
p95 describes the measured window and nothing else.

Gauges cannot be differenced — in-flight, RSS and goroutine counts only mean
anything as a series — which is what the CSV is for.

Runs until SIGTERM/SIGINT. A scrape that fails is recorded as a gap rather than
retried: this process is an observer, and a scraper that blocks on a wedged runtime
would delete the evidence that it was wedged.
"""

import argparse
import csv
import os
import signal
import sys
import time
import urllib.error
import urllib.request

import prom

_running = True


def _stop(_signum, _frame):
    global _running
    _running = False


# ---------------------------------------------------------------- scraping ----


def scrape(url, timeout):
    try:
        with urllib.request.urlopen(url, timeout=timeout) as resp:
            if resp.status != 200:
                return None
            return resp.read().decode("utf-8", "replace")
    except (urllib.error.URLError, OSError, ValueError):
        return None


def flow_names(parsed):
    """Flows the runtime exports, in a stable order.

    Read from in_flight because it carries exactly one series per flow — the
    counters carry one per flow *and outcome*, and a flow that has taken no traffic
    still exports at zero, so the set is complete from the first scrape.
    """
    return sorted(n for n in prom.by_label(parsed, "octo_flow_in_flight", "flow") if n)


# CSV columns that do not depend on the config, and where each comes from. Every
# one of these is a gauge: the counters live in the .prom snapshots instead.
GAUGES = (
    ("ready",             "octo_ready"),
    ("cpu_seconds_total", "process_cpu_seconds_total"),
    ("rss_bytes",         "process_resident_memory_bytes"),
    ("open_fds",          "process_open_fds"),
    ("goroutines",        "go_goroutines"),
    ("heap_alloc_bytes",  "go_memstats_heap_alloc_bytes"),
    ("heap_objects",      "go_memstats_heap_objects"),
    # Cumulative, but carried in the series too: allocation *rate* over the window
    # is the interesting form and needs more than two points to be believable.
    ("alloc_bytes_total", "go_memstats_alloc_bytes_total"),
    ("gc_seconds_total",  "go_gc_duration_seconds_sum"),
)


def row_for(parsed, flows, now, t0):
    row = [round(now, 3), round(now - t0, 3)]
    for _, metric in GAUGES:
        v = prom.first(parsed, metric)
        row.append("" if v is None else round(v, 6))
    # In-flight: the signal that distinguishes "quiet" from "stuck". Total first,
    # then per flow, so a single-flow scenario reads the same as a multi-flow one.
    in_flight = prom.by_label(parsed, "octo_flow_in_flight", "flow")
    row.append(sum(in_flight.values()) if in_flight else "")
    for f in flows:
        row.append(in_flight.get(f, ""))
    # Messages so far, so the CSV alone shows whether throughput went flat.
    row.append(prom.total(parsed, "octo_flow_messages_total") or 0)
    return row


def write_snapshot(path, text):
    """Write a snapshot so a reader never sees a half-written file."""
    tmp = path + ".tmp"
    with open(tmp, "w") as fh:
        fh.write(text)
    os.replace(tmp, path)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", required=True)
    ap.add_argument("--out", required=True, help="CSV path; snapshots are written beside it")
    ap.add_argument("--interval", type=float, default=1.0)
    ap.add_argument("--timeout", type=float, default=2.0)
    args = ap.parse_args()

    signal.signal(signal.SIGTERM, _stop)
    signal.signal(signal.SIGINT, _stop)

    out_dir = os.path.dirname(os.path.abspath(args.out))
    os.makedirs(out_dir, exist_ok=True)
    stem = os.path.join(out_dir, os.path.basename(args.out).rsplit(".", 1)[0])
    start_snap, end_snap = stem + "-start.prom", stem + "-end.prom"

    t0 = time.time()
    flows, writer, fh = None, None, None
    gaps = 0

    try:
        while _running:
            now = time.time()
            text = scrape(args.url, args.timeout)
            if text is None:
                gaps += 1
            else:
                parsed = prom.parse(text)
                if flows is None:
                    # The header cannot be written before the first scrape: the
                    # per-flow columns are named by the running configuration.
                    flows = flow_names(parsed)
                    fh = open(args.out, "w", newline="")
                    writer = csv.writer(fh)
                    writer.writerow(
                        ["ts_unix", "elapsed_s"]
                        + [name for name, _ in GAUGES]
                        + ["in_flight_total"]
                        + [f"in_flight[{f}]" for f in flows]
                        + ["messages_total"]
                    )
                    write_snapshot(start_snap, text)
                writer.writerow(row_for(parsed, flows, now, t0))
                fh.flush()
                # Rewritten every scrape rather than once at exit, so a scraper
                # that is killed rather than signalled still leaves a usable
                # window end.
                write_snapshot(end_snap, text)

            deadline = now + args.interval
            while _running and time.time() < deadline:
                time.sleep(min(0.1, max(0.0, deadline - time.time())))
    finally:
        if fh is not None:
            fh.close()

    if flows is None:
        print(f"scrape-metrics: no successful scrape of {args.url}", file=sys.stderr)
        return 1
    if gaps:
        print(f"scrape-metrics: {gaps} failed scrape(s) of {args.url}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
