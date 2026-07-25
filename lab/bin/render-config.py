#!/usr/bin/env python3
"""Render one variant of a scenario's integration from its single source of truth.

Usage:
    render-config.py <integration.yaml> baseline [-o out.yaml]
    render-config.py <integration.yaml> tuned [--workers N] [--buffer N] [--pool N] [-o out.yaml]

A scenario declares ONE config, `octo/integration.yaml`, carrying the tuning knobs
on its root flow as plain integers. Both benchmark arms are derived from it:

  baseline — `workers`, `buffer` and `pool` are STRIPPED, so the runtime falls back
             to whatever its own compiled-in defaults are.
  tuned    — the knobs are rewritten to the requested values.

Two design notes worth keeping in view.

Baseline strips rather than hardcoding the documented defaults (8/64/8) because
this lab exists to catch regressions across Octo versions. If a future version
changes what `workers` defaults to, a hardcoded baseline would keep measuring the
old number and never notice; a stripped one follows the runtime automatically,
which is what "out of the box" is supposed to mean.

Tuned rewrites literal integers rather than using Octo's `${ENV}` substitution,
because substitution does not reach root-flow fields. Verified against Octo 0.4.2:
`workers: ${FLOW_WORKERS}` fails at load with

    parse config: yaml: unmarshal errors:
      line 17: cannot unmarshal !!str `${FLOW_...` into int

whether or not the variable is declared, defaulted, or supplied in the OS
environment. `${...}` inside connector/block `settings` works normally.
"""

import argparse
import re
import sys

# A tuning knob on a root flow: `    workers: 8`.
KNOB = re.compile(r"^(?P<indent>\s*)(?P<key>workers|buffer|pool):\s*(?P<value>\S+)\s*$")
# Legacy env-substitution form, plus its `env:` declaration — stripped if present
# so an older scenario still renders.
ENV_ENTRY = re.compile(r"^(\s*)-\s*name:\s*FLOW_(?:WORKERS|BUFFER|POOL)\s*$")
EMPTY_ENV = re.compile(r"^\s*env:\s*$")

KNOB_KEYS = ("workers", "buffer", "pool")


def indent_of(line):
    return len(line) - len(line.lstrip())


def drop_env_entries(lines):
    """Remove FLOW_* env declarations and an `env:` key left with no children."""
    out, i = [], 0
    while i < len(lines):
        line = lines[i]
        if ENV_ENTRY.match(line):
            base = indent_of(line)
            i += 1
            while i < len(lines):
                nxt = lines[i]
                if nxt.strip() and indent_of(nxt) <= base:
                    break
                i += 1
            continue
        out.append(line)
        i += 1

    # Second pass: drop an `env:` that now has no entries.
    final, i = [], 0
    while i < len(out):
        line = out[i]
        if EMPTY_ENV.match(line):
            base = indent_of(line)
            has_child = False
            for nxt in out[i + 1:]:
                if not nxt.strip():
                    continue
                if indent_of(nxt) <= base:
                    break
                has_child = True
                break
            if not has_child:
                i += 1
                continue
        final.append(line)
        i += 1
    return final


def render_baseline(lines):
    kept = [ln for ln in lines if not KNOB.match(ln)]
    return drop_env_entries(kept)


def render_tuned(lines, values):
    """Rewrite each knob to its requested value; leave unrequested knobs as written."""
    out = []
    for line in lines:
        m = KNOB.match(line)
        if m and values.get(m["key"]) is not None:
            out.append(f'{m["indent"]}{m["key"]}: {values[m["key"]]}')
        else:
            out.append(line)
    return drop_env_entries(out)


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
    ap.add_argument("--workers", type=int)
    ap.add_argument("--buffer", type=int)
    ap.add_argument("--pool", type=int)
    ap.add_argument("-o", "--out")
    args = ap.parse_args()

    with open(args.source) as fh:
        lines = fh.read().splitlines()

    declared = {m["key"] for m in (KNOB.match(ln) for ln in lines) if m}

    if args.variant == "baseline":
        body = render_baseline(lines)
        head = banner(args.source, "baseline",
                      "workers/buffer/pool stripped — the runtime uses its own defaults.")
        rendered = head + body
        # The absence of the knobs is the entire point of this arm; verify it.
        for n, line in enumerate(rendered, 1):
            if KNOB.match(line):
                print(f"error: baseline still declares a tuning knob at line {n}: "
                      f"{line.strip()}", file=sys.stderr)
                return 1
    else:
        missing = [k for k in KNOB_KEYS if k not in declared]
        if missing:
            print(f"error: {args.source} declares no {', '.join(missing)} on its root flow, "
                  f"so there is nothing to tune. Add the knob(s) as literal integers.",
                  file=sys.stderr)
            return 1
        values = {"workers": args.workers, "buffer": args.buffer, "pool": args.pool}
        body = render_tuned(lines, values)
        chosen = ", ".join(
            f"{k}={values[k] if values[k] is not None else 'as written'}" for k in KNOB_KEYS
        )
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
