#!/usr/bin/env bash
#
# check-doc-versions.sh — refuse a hardcoded release version in the docs'
# copy-paste install commands.
#
# The version lives only in a git tag, so every version string written into a
# doc starts rotting the moment the next release goes out. That is not a
# hypothetical: PR #47 rewrote both README.md and CLAUDE.md a full day after
# v0.2.0 shipped and left every install command pinned to v0.1.0, which sent
# anyone following the README to a release two fixes behind. A human reviewer
# demonstrably does not catch this, so a grep does.
#
# The fix the docs adopted is to name no version at all where a stable pointer
# exists: `releases/latest/download/install.yaml` (kept honest by make_latest,
# see hack/check-latest-eligible.sh) and `helm install` with no --version
# (Helm resolves the newest stable and skips the dev prereleases). Those two
# shapes are exactly what this script enforces.
#
# Deliberately NOT checked: prose that names an old version on purpose — the
# release history in CLAUDE.md, the "pin one instead" examples, the dev-build
# example. Only URLs and the chart install command are matched, because those
# are the lines someone copies and runs.
#
# Env seams (for the test harness):
#   DOCS  space-separated list of files to check. Defaults to the two docs.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

DOCS="${DOCS:-README.md CLAUDE.md}"

failures=()

# Rule 1: a release asset URL must not pin a version. The one accepted
# exception is the literal placeholder the uninstall instructions use, where
# naming a version is the honest thing to do — you must delete the manifest you
# actually applied, not the newest one.
for doc in $DOCS; do
  [ -f "$doc" ] || continue
  while IFS= read -r hit; do
    failures+=("$doc:$hit")
    failures+=("    use releases/latest/download/install.yaml, or the v<version-you-installed> placeholder")
  done < <(grep -n 'releases/download/v[0-9]' "$doc" || true)
done

# Rule 2: the chart install example must not pin a STABLE --version. The
# command is written across backslash-continued lines, so the check joins those
# into one logical line first — a same-line grep would miss the exact shape
# that went stale. A prerelease pin (`--version 0.2.1-dev.42`) is allowed and
# must stay allowed: the dev-build section has no stable pointer to offer, so
# naming a version there is the only thing it can do.
for doc in $DOCS; do
  [ -f "$doc" ] || continue
  while IFS= read -r hit; do
    failures+=("$doc:$hit")
    failures+=("    drop --version from the copy-paste install; Helm resolves the newest stable on its own")
  done < <(awk '
    {
      if (buf == "") start = NR
      buf = buf $0
      if (sub(/\\$/, "", buf)) next
      if (buf ~ /oci:\/\/ghcr\.io\/tabman83\/charts\/kvsynk8s/ &&
          buf ~ /--version[ \t]+[0-9]+\.[0-9]+\.[0-9]+([ \t]|$)/) {
        sub(/^[ \t]+/, "", buf)
        print start ": " buf
      }
      buf = ""
    }
  ' "$doc")
done

if [ ${#failures[@]} -gt 0 ]; then
  echo "check-doc-versions: a pinned release version is in a copy-paste install command:" >&2
  printf '%s\n' "${failures[@]}" >&2
  exit 1
fi

echo "check-doc-versions: no pinned release versions in the install commands."
