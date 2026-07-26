#!/usr/bin/env python3
"""Render one variant of a scenario's integration from its single source of truth.

Usage:
    render-config.py <integration.yaml> baseline [--tunables "workers buffer pool"] [-o out.yaml]
    render-config.py <integration.yaml> tuned  [--tunables ...] [--set workers=128] ... [-o out.yaml]

A scenario declares ONE config, `octo/integration.yaml`, carrying its tunable
knobs as plain integers. Both benchmark arms are derived from it:

  baseline — every declared tunable is STRIPPED, so the runtime falls back to
             whatever it defaults to.
  tuned    — the named knobs are rewritten to the requested values.

TUNABLES ARE PER-SCENARIO. The root-flow trio (`workers`, `buffer`, `pool`) is the
default set, but a scenario that exercises other machinery declares its own — a
database scenario adds `maxOpenConns`/`maxIdleConns`, a queue scenario adds
`listeners`. The matching is indent-agnostic, so a knob nested in connector or
source `settings` is handled the same way as one on a root flow.

Two design notes worth keeping in view.

Baseline strips rather than hardcoding documented defaults because this lab exists
to catch regressions across Octo versions. If a future version changes a default,
a hardcoded baseline would keep measuring the old value and never notice. This is
not hypothetical: `workers` is documented as defaulting to 8, but declaring
`workers: 8` costs ~90x on p95 versus omitting the key, so the real default is
nothing like 8. A hardcoded baseline would have measured a pathological config and
labelled it "out of the box".

Tuned rewrites the knobs in place rather than driving them through `${ENV}`
substitution at run time. That used to be forced — substitution did not reach
root-flow fields — and is now a choice: the rendered file is archived next to the
result as `config.yaml`, so a knob written there as a literal is provenance. A
placeholder would record only that a value came from somewhere, and the run's
environment is not part of the artifact.

Scenarios may still write `${...}` anywhere, including root-flow fields; the
matching below accepts any value, so such a knob is stripped for baseline and
rewritten for tuned exactly like a literal.
"""

import argparse
import re
import sys

DEFAULT_TUNABLES = ("workers", "buffer", "pool")


def knob_re(names):
    """`    maxOpenConns: 25` for any declared tunable, at any indent."""
    alt = "|".join(re.escape(n) for n in names)
    return re.compile(rf"^(?P<indent>\s*)(?P<key>{alt}):\s*(?P<value>\S+)\s*$")


def render_baseline(lines, rx):
    return [ln for ln in lines if not rx.match(ln)]


def render_tuned(lines, rx, values):
    """Rewrite each requested knob; leave unrequested ones as written."""
    out = []
    for line in lines:
        m = rx.match(line)
        if m and values.get(m["key"]) is not None:
            out.append(f'{m["indent"]}{m["key"]}: {values[m["key"]]}')
        else:
            out.append(line)
    return out


def banner(source, variant, note):
    name = source.replace("\\", "/").split("/")[-1]
    return [
        "# GENERATED — do not edit. Edit the scenario's integration.yaml instead.",
        f"# Rendered from {name} as the {variant.upper()} variant.",
        f"# {note}",
        "",
    ]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("source")
    ap.add_argument("variant", choices=("baseline", "tuned"))
    ap.add_argument("--tunables", default=" ".join(DEFAULT_TUNABLES),
                    help="space-separated knob names this scenario exposes")
    ap.add_argument("--set", action="append", default=[], metavar="KEY=VALUE",
                    help="knob value for the tuned variant; repeatable")
    ap.add_argument("-o", "--out")
    args = ap.parse_args()

    tunables = [t for t in args.tunables.split() if t]
    if not tunables:
        print("error: no tunables declared", file=sys.stderr)
        return 1
    rx = knob_re(tunables)

    with open(args.source) as fh:
        lines = fh.read().splitlines()

    declared = {m["key"] for m in (rx.match(ln) for ln in lines) if m}

    if args.variant == "baseline":
        body = render_baseline(lines, rx)
        rendered = banner(args.source, "baseline",
                          f"stripped: {', '.join(sorted(declared)) or 'nothing'} "
                          f"— the runtime uses its own defaults.") + body
        # The absence of the knobs is the entire point of this arm; verify it.
        for n, line in enumerate(rendered, 1):
            if rx.match(line):
                print(f"error: baseline still declares a tunable at line {n}: "
                      f"{line.strip()}", file=sys.stderr)
                return 1
    else:
        values = {}
        for item in args.set:
            if "=" not in item:
                print(f"error: --set expects KEY=VALUE, got {item!r}", file=sys.stderr)
                return 1
            k, v = item.split("=", 1)
            if not v.strip():
                continue  # empty means "leave as written"
            if k not in tunables:
                print(f"error: {k!r} is not a declared tunable for this scenario "
                      f"({', '.join(tunables)})", file=sys.stderr)
                return 1
            if k not in declared:
                print(f"error: {args.source} does not declare {k!r}, so it cannot be "
                      f"tuned. Add it as a literal integer.", file=sys.stderr)
                return 1
            values[k] = v.strip()

        if not declared:
            print(f"error: {args.source} declares none of its tunables "
                  f"({', '.join(tunables)}), so there is nothing to tune.", file=sys.stderr)
            return 1

        body = render_tuned(lines, rx, values)
        chosen = ", ".join(f"{k}={values[k]}" for k in sorted(values)) or "all as written"
        rendered = banner(args.source, "tuned", f"knobs: {chosen}.") + body

    text = "\n".join(rendered) + "\n"
    if args.out:
        with open(args.out, "w") as fh:
            fh.write(text)
    else:
        sys.stdout.write(text)
    return 0


if __name__ == "__main__":
    sys.exit(main())
