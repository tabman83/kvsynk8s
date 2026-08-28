#!/usr/bin/env bash
#
# doc-guards_test.sh — drive hack/check-doc-versions.sh over its decision
# table.
#
# The guard exists because a stale install command in the README sends users to
# an old release, and that is invisible in a diff review. A guard nobody tests
# is worth about as much, so every shape it must accept and every shape it must
# refuse is pinned here — including the two exact forms that actually shipped
# stale (a pinned releases/download/vX.Y.Z link, and --version on the chart
# install written across backslash-continued lines).
#
# Kept separate from release-guards_test.sh: that file drives the two release
# guards, this one drives a docs guard, and they share no fixtures.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

guard="hack/check-doc-versions.sh"
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

pass=0
fail=0

# check <name> <expected exit> <expected substring in output> -- <cmd...>
check() {
  local name="$1" want_exit="$2" want_out="$3"; shift 3
  [ "$1" = "--" ] && shift
  local out status=0
  out="$("$@" 2>&1)" || status=$?
  if [ "$status" != "$want_exit" ]; then
    echo "FAIL: $name — expected exit $want_exit, got $status"
    echo "$out" | sed 's/^/      /'
    fail=$((fail + 1))
    return
  fi
  if [ -n "$want_out" ] && ! grep -qF -- "$want_out" <<<"$out"; then
    echo "FAIL: $name — output did not contain: $want_out"
    echo "$out" | sed 's/^/      /'
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

fixture() { # fixture <name> <<'EOF' ... EOF
  local name="$1"
  cat > "$workdir/$name.md"
  echo "$workdir/$name.md"
}

# == the two forms that actually shipped stale ==

pinned_link="$(fixture pinned_link <<'EOF'
Option B — release manifest

```bash
kubectl apply -f https://github.com/tabman83/kvsynk8s/releases/download/v0.1.0/install.yaml
```
EOF
)"
check "a pinned releases/download link is refused" 1 "releases/latest/download/install.yaml" \
  -- env DOCS="$pinned_link" "$guard"

pinned_chart="$(fixture pinned_chart <<'EOF'
Option A — Helm

```bash
helm install kvsynk8s oci://ghcr.io/tabman83/charts/kvsynk8s \
  --version 0.1.0 \
  --namespace kvsynk8s --create-namespace
```
EOF
)"
check "a stable --version pinned across continued lines is refused" 1 "drop --version" \
  -- env DOCS="$pinned_chart" "$guard"

# == the forms the docs actually use, which must not be false positives ==

stable_pointers="$(fixture stable_pointers <<'EOF'
```bash
kubectl apply -f https://github.com/tabman83/kvsynk8s/releases/latest/download/install.yaml
```

```bash
helm install kvsynk8s oci://ghcr.io/tabman83/charts/kvsynk8s \
  --namespace kvsynk8s --create-namespace
```
EOF
)"
check "the latest-download link and an unpinned helm install pass" 0 "no pinned release versions" \
  -- env DOCS="$stable_pointers" "$guard"

placeholder="$(fixture placeholder <<'EOF'
```bash
# the version you installed, not necessarily the newest one
kubectl delete -f https://github.com/tabman83/kvsynk8s/releases/download/v<version-you-installed>/install.yaml
```
EOF
)"
check "the v<version-you-installed> placeholder passes" 0 "no pinned release versions" \
  -- env DOCS="$placeholder" "$guard"

devbuild="$(fixture devbuild <<'EOF'
```bash
helm install kvsynk8s oci://ghcr.io/tabman83/charts/kvsynk8s \
  --version 0.2.1-dev.42 --namespace kvsynk8s --create-namespace
```
EOF
)"
check "a dev-build prerelease pin is allowed" 0 "no pinned release versions" \
  -- env DOCS="$devbuild" "$guard"

prose="$(fixture prose <<'EOF'
T024's follow-up was confirmed back on v0.1.0, which carried install.yaml.
Add `--version X.Y.Z` when you want to pin one instead.
The newest release is v0.2.0 (2026-08-26).
EOF
)"
check "prose naming an old version on purpose passes" 0 "no pinned release versions" \
  -- env DOCS="$prose" "$guard"

# == the repository's own docs must satisfy the guard ==

check "README.md and CLAUDE.md pass as they stand" 0 "no pinned release versions" \
  -- "$guard"

echo
echo "doc guards: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
