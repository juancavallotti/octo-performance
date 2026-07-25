#!/usr/bin/env bash
# Capture the hardware, OS, and version-under-test fingerprint for a run.
#
# Usage: capture-env.sh <out.json>
#
# Reads TARGET, HOST, SCENARIO_ID from the environment. Branches on uname so the
# same script works on the macOS lab host and on a Linux/GCE runner later.

. "$(dirname "${BASH_SOURCE[0]}")/common.sh"

OUT="${1:?usage: capture-env.sh <out.json>}"
TARGET="${TARGET:-native}"
load_host "${HOST:-local}"

# ------------------------------------------------------------- hardware ------

case "$OS" in
  Darwin)
    CPU_MODEL="$(sysctl -n machdep.cpu.brand_string 2>/dev/null || echo unknown)"
    CPU_LOGICAL="$(sysctl -n hw.logicalcpu 2>/dev/null || echo 0)"
    CPU_PHYSICAL="$(sysctl -n hw.physicalcpu 2>/dev/null || echo 0)"
    CPU_PERF="$(sysctl -n hw.perflevel0.logicalcpu 2>/dev/null || echo 0)"
    CPU_EFF="$(sysctl -n hw.perflevel1.logicalcpu 2>/dev/null || echo 0)"
    MEM_BYTES="$(sysctl -n hw.memsize 2>/dev/null || echo 0)"
    OS_NAME="macOS"
    OS_VERSION="$(sw_vers -productVersion 2>/dev/null || echo unknown)"
    OS_BUILD="$(sw_vers -buildVersion 2>/dev/null || echo unknown)"
    ARCH="$(uname -m)"
    CLOUD_MACHINE_TYPE=""
    ;;
  Linux)
    CPU_MODEL="$(awk -F': ' '/^model name/{print $2; exit}' /proc/cpuinfo 2>/dev/null || echo unknown)"
    [ -n "$CPU_MODEL" ] || CPU_MODEL="$(awk -F': ' '/^Model/{print $2; exit}' /proc/cpuinfo 2>/dev/null || echo unknown)"
    CPU_LOGICAL="$(nproc 2>/dev/null || echo 0)"
    CPU_PHYSICAL="$(lscpu -p=Core,Socket 2>/dev/null | grep -vc '^#' || echo 0)"
    CPU_PERF=0
    CPU_EFF=0
    MEM_BYTES="$(awk '/^MemTotal/{print $2*1024; exit}' /proc/meminfo 2>/dev/null || echo 0)"
    OS_NAME="$(. /etc/os-release 2>/dev/null && echo "$NAME" || echo Linux)"
    OS_VERSION="$(. /etc/os-release 2>/dev/null && echo "$VERSION_ID" || uname -r)"
    OS_BUILD="$(uname -r)"
    ARCH="$(uname -m)"
    # GCE metadata server, when present, names the machine type.
    CLOUD_MACHINE_TYPE="$(curl -s --max-time 1 -H 'Metadata-Flavor: Google' \
      http://metadata.google.internal/computeMetadata/v1/instance/machine-type 2>/dev/null | awk -F/ '{print $NF}' || true)"
    ;;
  *)
    die "unsupported OS '$OS'"
    ;;
esac

FD_LIMIT="$(ulimit -n)"

# ------------------------------------------- version under test + envelope ----

OCTO_VERSION=""
OCTO_ARTIFACT=""
OCTO_ARTIFACT_BYTES=0
IMAGE_REF=""
IMAGE_DIGEST=""
DOCKER_NCPU=0
DOCKER_MEM_BYTES=0
DOCKER_SERVER=""

case "$TARGET" in
  native)
    OCTO_VERSION="$(octo_version)"
    OCTO_ARTIFACT="$(octo_bin)"
    if [ -n "$OCTO_ARTIFACT" ] && [ -f "$OCTO_ARTIFACT" ]; then
      OCTO_ARTIFACT_BYTES="$(wc -c < "$OCTO_ARTIFACT" | tr -d ' ')"
    fi
    ;;
  docker)
    IMAGE_REF="${OCTO_IMAGE:-juancavallotti/octo-runtime:latest}"
    OCTO_VERSION="$(docker_image_version "$IMAGE_REF")"
    IMAGE_DIGEST="$(docker image inspect "$IMAGE_REF" --format '{{index .RepoDigests 0}}' 2>/dev/null || true)"
    [ -n "$IMAGE_DIGEST" ] || IMAGE_DIGEST="$(docker image inspect "$IMAGE_REF" --format '{{.Id}}' 2>/dev/null || true)"
    OCTO_ARTIFACT_BYTES="$(docker image inspect "$IMAGE_REF" --format '{{.Size}}' 2>/dev/null || echo 0)"
    OCTO_ARTIFACT="$IMAGE_REF"
    DOCKER_NCPU="$(docker info --format '{{.NCPU}}' 2>/dev/null || echo 0)"
    DOCKER_MEM_BYTES="$(docker info --format '{{.MemTotal}}' 2>/dev/null || echo 0)"
    DOCKER_SERVER="$(docker info --format '{{.ServerVersion}}' 2>/dev/null || echo unknown)"
    ;;
esac

K6_VERSION="$(k6_version)"
LAB_COMMIT="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo uncommitted)"
LAB_DIRTY="$(git -C "$REPO_ROOT" status --porcelain 2>/dev/null | head -1 >/dev/null && \
             [ -n "$(git -C "$REPO_ROOT" status --porcelain 2>/dev/null)" ] && echo true || echo false)"

# ------------------------------------------------------------------ emit ------

export CPU_MODEL CPU_LOGICAL CPU_PHYSICAL CPU_PERF CPU_EFF MEM_BYTES \
       OS_NAME OS_VERSION OS_BUILD ARCH FD_LIMIT CLOUD_MACHINE_TYPE \
       OCTO_VERSION OCTO_ARTIFACT OCTO_ARTIFACT_BYTES IMAGE_REF IMAGE_DIGEST \
       DOCKER_NCPU DOCKER_MEM_BYTES DOCKER_SERVER K6_VERSION LAB_COMMIT LAB_DIRTY \
       TARGET OUT

mkdir -p "$(dirname "$OUT")"
python3 - <<'PY'
import json, os, subprocess

def i(k, d=0):
    try: return int(os.environ.get(k, d) or d)
    except ValueError: return d

target = os.environ["TARGET"]

env = {
    "capturedAt": subprocess.run(["date","-u","+%Y-%m-%dT%H:%M:%SZ"],
                                 capture_output=True, text=True).stdout.strip(),
    "hostProfile": os.environ.get("HOST_PROFILE", "unknown"),
    "target": target,
    "scenario": os.environ.get("SCENARIO_ID", ""),
    "versionUnderTest": os.environ.get("OCTO_VERSION", "unknown"),
    "hardware": {
        "cpuModel":     os.environ.get("CPU_MODEL", "unknown"),
        "arch":         os.environ.get("ARCH", "unknown"),
        "logicalCores": i("CPU_LOGICAL"),
        "physicalCores":i("CPU_PHYSICAL"),
        "performanceCores": i("CPU_PERF") or None,
        "efficiencyCores":  i("CPU_EFF") or None,
        "memoryBytes":  i("MEM_BYTES"),
        "cloudMachineType": os.environ.get("CLOUD_MACHINE_TYPE") or None,
    },
    "os": {
        "name":    os.environ.get("OS_NAME", "unknown"),
        "version": os.environ.get("OS_VERSION", "unknown"),
        "build":   os.environ.get("OS_BUILD", "unknown"),
        "fileDescriptorLimit": os.environ.get("FD_LIMIT", "unknown"),
    },
    "runtime": {
        "artifact":      os.environ.get("OCTO_ARTIFACT", ""),
        "artifactBytes": i("OCTO_ARTIFACT_BYTES"),
    },
    "tooling": {
        "k6":        os.environ.get("K6_VERSION", "unknown"),
        "labCommit": os.environ.get("LAB_COMMIT", "unknown"),
        "labDirty":  os.environ.get("LAB_DIRTY", "false") == "true",
    },
    "loadGenerator": {
        "baseUrl": os.environ.get("BASE_URL", ""),
        # True when k6 and the server share a host: contention is a known limitation.
        "colocatedWithServer": any(h in os.environ.get("BASE_URL", "")
                                   for h in ("localhost", "127.0.0.1")),
    },
}

if target == "docker":
    env["container"] = {
        "image":         os.environ.get("IMAGE_REF", ""),
        "digest":        os.environ.get("IMAGE_DIGEST", ""),
        "imageBytes":    i("OCTO_ARTIFACT_BYTES"),
        "dockerServer":  os.environ.get("DOCKER_SERVER", ""),
        # The envelope the container runtime was actually given — usually smaller
        # than the host, which is why cross-target comparison is only indicative.
        "allocatedCpus":        i("DOCKER_NCPU"),
        "allocatedMemoryBytes": i("DOCKER_MEM_BYTES"),
    }

with open(os.environ["OUT"], "w") as f:
    json.dump(env, f, indent=2)
    f.write("\n")
PY

dim "  env -> $OUT"
