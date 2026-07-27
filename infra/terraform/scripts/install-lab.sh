# Fetch and unpack the lab release. Sourced by the runner and deps startup scripts.
#
# One archive holds `perf`, `labbackend`, every scenario and every campaign spec, so the
# harness and the scenarios it runs cannot come from different commits. A runner that
# curled a binary from a release and cloned scenarios from git could have the two
# disagree, and a scenario that loads is not the same as a scenario the harness was
# written against.
#
# Both machines unpack to the same path, and that is load-bearing rather than tidy: a
# scenario's setup.sh is addressed by the path the harness knows it by, and the harness
# runs on the runner. Different paths would mean the deps host is asked to execute a
# file it does not have.
#
# Expects: LAB_VERSION, LAB_REPO, LAB_ROOT, LAB_USER.

install_lab() {
  if [ -z "$LAB_VERSION" ]; then
    # Deliberately not fatal, and deliberately loud. The first campaign of a new
    # deployment runs before any release exists, and the operator stages the archive
    # by hand; a boot that died here would leave a machine that never finishes
    # starting and says nothing about why.
    echo "NOTE: no harness_version pinned. Stage the lab yourself:" >&2
    echo "      tar xzf octo-perf-lab_<v>_linux_<arch>.tar.gz -C $LAB_ROOT" >&2
    return 0
  fi

  local arch tag base url_archive url_sums tmp
  case "$(dpkg --print-architecture)" in
    amd64) arch=amd64 ;;
    arm64) arch=arm64 ;;
    *) echo "ERROR: unsupported architecture $(dpkg --print-architecture)" >&2; return 1 ;;
  esac

  tag="harness/v${LAB_VERSION}"
  base="octo-perf-lab_${LAB_VERSION}_linux_${arch}.tar.gz"
  url_archive="https://github.com/${LAB_REPO}/releases/download/${tag}/${base}"
  url_sums="https://github.com/${LAB_REPO}/releases/download/${tag}/checksums.txt"

  tmp="$(mktemp -d)"
  echo "fetching $url_archive"
  curl -fsSL --retry 5 --retry-delay 3 "$url_archive" -o "$tmp/$base" || {
    echo "ERROR: could not fetch $url_archive" >&2
    return 1
  }

  # Verified, not merely downloaded. A truncated transfer produces a tarball that
  # unpacks partially — half a scenario tree, and a campaign that fails at whichever
  # cell first needs the missing file, hours in.
  if curl -fsSL --retry 3 "$url_sums" -o "$tmp/checksums.txt"; then
    ( cd "$tmp" && grep " ${base}\$" checksums.txt | sha256sum -c - ) || {
      echo "ERROR: checksum mismatch for $base" >&2
      return 1
    }
    echo "checksum ok"
  else
    echo "WARNING: no checksums.txt published for $tag; unpacking unverified" >&2
  fi

  tar xzf "$tmp/$base" -C "$LAB_ROOT"
  chown -R "$LAB_USER:$LAB_USER" "$LAB_ROOT"
  rm -rf "$tmp"

  # perf on PATH so the operator's invocation is the one the terraform output prints.
  if [ -x "$LAB_ROOT/perf" ]; then
    ln -sf "$LAB_ROOT/perf" /usr/local/bin/perf
  fi
  echo "lab ${LAB_VERSION} unpacked into $LAB_ROOT"
}
