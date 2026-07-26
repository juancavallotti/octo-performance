#!/usr/bin/env python3
"""Read Prometheus exposition, and difference two scrapes of it.

Shared by scrape-metrics.py (which collects), metrics-identity.py (which reads the
build labels) and report.py (which differences the window). One parser rather than
three, because the three have to agree about what a sample means.

Deliberately minimal: this reads one endpoint whose shape is known. It handles what
that endpoint emits and ignores the parts of the specification it does not use
(timestamps, exemplars) rather than pretending to be a general implementation.

The differencing half is the reason this file exists. Counters and histograms on
/metrics are cumulative since the process started, so a single scrape describes
everything the process has ever done — including the smoke gate and the warm-up.
Subtracting the scrape at the start of the load window from the one at the end is
what makes a number describe the window, and it does so exactly rather than by
sampling.
"""

import math

# ------------------------------------------------------------------ parsing ---


def parse_value(raw):
    """A sample value. Infinities are real in exposition format; NaN is not data."""
    try:
        v = float(raw.strip())
    except ValueError:
        return None
    return None if math.isnan(v) else v


def parse_labels(text):
    """`a="1",b="x,y"` -> {"a": "1", "b": "x,y"}.

    Splits on commas outside quotes, because a block address is a legitimate label
    value and `--metrics-blocks` takes a comma-separated list of them.
    """
    labels, key, buf, in_quotes, reading_key = {}, "", "", False, True
    for ch in text:
        if reading_key:
            if ch == "=":
                key, reading_key = buf.strip(), False
                buf = ""
            elif ch == ",":
                buf = ""
            else:
                buf += ch
            continue
        if ch == '"':
            in_quotes = not in_quotes
            continue
        if ch == "," and not in_quotes:
            labels[key] = buf
            buf, reading_key = "", True
            continue
        buf += ch
    if key and not reading_key:
        labels[key] = buf
    return labels


def parse(text):
    """Exposition text -> {metric_name: [(labels_dict, value), ...]}."""
    out = {}
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        # Split the value off the end: a label value may contain spaces, so this
        # cannot split on the first separator.
        try:
            head, raw_value = line.rsplit(" ", 1)
        except ValueError:
            continue
        value = parse_value(raw_value)
        if value is None:
            continue
        head = head.strip()
        if head.endswith("}") and "{" in head:
            name, _, label_text = head.partition("{")
            labels = parse_labels(label_text[:-1])
        else:
            name, labels = head, {}
        out.setdefault(name.strip(), []).append((labels, value))
    return out


def parse_file(path):
    try:
        with open(path) as fh:
            return parse(fh.read())
    except OSError:
        return None


# ------------------------------------------------------------------ reading ---


def first(parsed, name):
    """The value of the first (usually only) series of a metric."""
    samples = parsed.get(name) or []
    return samples[0][1] if samples else None


def labels_of(parsed, name):
    """The label set of the first series — how octo_build_info is read."""
    samples = parsed.get(name) or []
    return dict(samples[0][0]) if samples else {}


def total(parsed, name, **match):
    """Sum a metric's series, optionally filtered by exact label values."""
    samples = parsed.get(name) or []
    if not samples:
        return None
    out = 0.0
    for labels, v in samples:
        if all(labels.get(k) == want for k, want in match.items()):
            out += v
    return out


def by_label(parsed, name, label):
    """{label_value: summed_value} for one metric."""
    out = {}
    for labels, v in parsed.get(name) or []:
        out[labels.get(label, "")] = out.get(labels.get(label, ""), 0.0) + v
    return out


def series_key(labels, ignore=()):
    """A hashable identity for one series, so two scrapes can be lined up."""
    return tuple(sorted((k, v) for k, v in labels.items() if k not in ignore))


# ---------------------------------------------------------------- windowing ---


def counter_delta(start, end, name, **match):
    """How much a counter advanced between two scrapes.

    None when either scrape lacks the metric. A negative result means the process
    restarted between the scrapes — counters do not go backwards — and is returned
    as None rather than as a negative rate, because a restart mid-window is a
    broken measurement and should read as missing, not as an anomaly.
    """
    a, b = total(start, name, **match), total(end, name, **match)
    if a is None or b is None:
        return None
    return None if b < a else b - a


def counter_delta_by_label(start, end, name, label):
    """counter_delta, split by one label."""
    a, b = by_label(start, name, label), by_label(end, name, label)
    out = {}
    for key, end_v in b.items():
        start_v = a.get(key, 0.0)
        if end_v >= start_v:
            out[key] = end_v - start_v
    return out


def histogram_window(start, end, name, **match):
    """A histogram's buckets over the window: {le: count}, plus sum and count.

    Bucket-by-bucket subtraction, which is what makes a quantile computed from the
    result describe the measured window instead of the process's whole lifetime.
    """
    def buckets(parsed):
        out = {}
        for labels, v in parsed.get(name + "_bucket") or []:
            if all(labels.get(k) == want for k, want in match.items()):
                le = labels.get("le")
                if le is None:
                    continue
                out[le] = out.get(le, 0.0) + v
        return out

    b_start, b_end = buckets(start), buckets(end)
    if not b_end:
        return None

    delta = {}
    for le, end_v in b_end.items():
        start_v = b_start.get(le, 0.0)
        delta[le] = max(0.0, end_v - start_v)

    return {
        "buckets": delta,
        "sum": counter_delta(start, end, name + "_sum", **match),
        "count": counter_delta(start, end, name + "_count", **match),
    }


def quantile_detail(window, q):
    """A quantile out of a windowed histogram, with how much the buckets support it.

    Returns {"seconds": float|None, "bound": str, "edge": float|None} where bound is:

        "interpolated"  the quantile fell between two finite edges that both carry
                        real counts. This is histogram_quantile's normal case: a
                        linear interpolation inside the containing bucket.
        "below"         it fell inside the LOWEST bucket, so every observation that
                        matters is somewhere in (0, edge] and the histogram cannot
                        say where. Report it as "< edge", never as a number.
        "above"         it fell in +Inf, past the last finite edge. Unquantifiable.

    The "below" case is not a corner case for this lab, it is the common one. Octo's
    flow-duration histogram uses Prometheus's default buckets, whose lowest edge is
    5 ms, and most flows here complete in well under a millisecond. Interpolating
    inside that bucket yields 2.5 ms for a p50 and 4.75 ms for a p95 — numbers that
    are a function of the bucket width and nothing else, and that would read as
    precise measurements. Distinguishing the case is the difference between a report
    that is honest and one that invents its headline figure.
    """
    empty = {"seconds": None, "bound": "unknown", "edge": None}
    if not window:
        return empty
    counts = window.get("buckets") or {}
    total_count = counts.get("+Inf")
    if not total_count:
        return empty

    edges = sorted(
        ((float(le), c) for le, c in counts.items() if le != "+Inf"),
        key=lambda p: p[0],
    )
    if not edges:
        return empty

    target = q * total_count
    prev_edge, prev_count = 0.0, 0.0
    for i, (edge, cumulative) in enumerate(edges):
        if cumulative >= target:
            if i == 0:
                # Everything up to the quantile is in the first bucket. The true
                # value is in (0, edge] and the histogram has no more to say.
                return {"seconds": None, "bound": "below", "edge": edge}
            span = cumulative - prev_count
            if span <= 0:
                return {"seconds": edge, "bound": "interpolated", "edge": edge}
            seconds = prev_edge + (edge - prev_edge) * (target - prev_count) / span
            return {"seconds": seconds, "bound": "interpolated", "edge": edge}
        prev_edge, prev_count = edge, cumulative
    return {"seconds": None, "bound": "above", "edge": edges[-1][0]}


def quantile(window, q):
    """The interpolated quantile in seconds, or None when the buckets cannot say."""
    return quantile_detail(window, q)["seconds"]


def mean_seconds(window):
    """A histogram's mean over the window — exact, unlike an interpolated quantile."""
    if not window:
        return None
    n, s = window.get("count"), window.get("sum")
    if not n or s is None:
        return None
    return s / n
