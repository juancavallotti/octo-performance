#!/usr/bin/env python3
"""What the running process says it is. Reads exposition on stdin, writes JSON.

Usage: curl -s $ADMIN/metrics | metrics-identity.py > runtime-identity.json

Rule 1 of AGENTS.md is "no version, no result", and it was satisfied by asking the
artifact its version before starting it — which answers "what did the harness intend
to run". This answers a different question: what is actually running, according to
the process itself.

They agree on every healthy run, which is the point. They come apart exactly when
something is wrong — a stale process still holding the port, an image whose tag
moved, a container built from a different tree than the binary beside it — and those
are the cases where a provenance record is worth having. report.py compares the two
and says so when they disagree.

services_module is here for a second reason. AGENTS.md notes that the build tag
decides which services provider is compiled in, and that the native and container
dev builds deliberately differ on it. Before this, nothing verified the artifact
under test carried the provider the run assumed.
"""

import json
import sys

import prom


def main():
    parsed = prom.parse(sys.stdin.read())
    if not parsed:
        print("metrics-identity: nothing to read on stdin", file=sys.stderr)
        return 1

    build = prom.labels_of(parsed, "octo_build_info")
    if not build:
        print("metrics-identity: no octo_build_info in the exposition", file=sys.stderr)
        return 1

    identity = {
        "source": "octo_build_info",
        "version": build.get("version"),
        "buildDate": build.get("build_date"),
        # Which runtime-services provider the binary was compiled with — standalone
        # or k8s. The container image ships k8s; the native binary does not.
        "servicesModule": build.get("services_module"),
        # The size of the configuration the process actually loaded. A scenario that
        # silently failed to load a flow reads as a smaller number here than the
        # config declares.
        "flows": prom.first(parsed, "octo_flows"),
        "connectors": prom.first(parsed, "octo_connectors"),
        "ready": prom.first(parsed, "octo_ready"),
        # The flows by name, which is what a per-flow report is keyed on.
        "flowNames": sorted(
            n for n in prom.by_label(parsed, "octo_flow_in_flight", "flow") if n
        ),
    }
    json.dump(identity, sys.stdout, indent=2)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
