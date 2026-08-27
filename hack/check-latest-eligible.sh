#!/usr/bin/env bash
#
# check-latest-eligible.sh — decide whether releasing <version> is allowed to
# move the :latest image tag.
#
# The release workflow used to gate that on "is this a prerelease?" alone,
# which says nothing about ordering. Re-running an older stable release —
# something the workflow explicitly invites, because the flaky steps worth
# re-running live inside build-and-push and every upload uses overwrite:true so
# that "Re-run all jobs" converges — re-executed `imagetools create --tag
# :latest ...:vOLD` and quietly pointed :latest at the older operator until the
# next stable release came out. The concurrency group only serialises runs; it
# gives no version ordering. So ordering has to be checked here.
#
# usage: check-latest-eligible.sh <version>
#
#   <version>  the version being released, no leading v (0.2.0, 0.2.0-rc1).
#
# Prints "true" or "false" on stdout — the decision — plus the reason on
# stderr, and exits 0. Exit 2 means the input was not a version at all and no
# decision was made. When $GITHUB_OUTPUT is set it also appends
# "latest=<decision>" so a workflow step can gate on it.
#
# Env:
#   REPO_DIR  git repository whose v* tags are the set of existing releases.
#             Defaults to this repository. Only a test seam.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_DIR="${REPO_DIR:-$repo_root}"

usage() {
  echo "usage: check-latest-eligible.sh <version>   (version without a leading v)" >&2
}

[ "$#" -eq 1 ] || { usage; exit 2; }
version="$1"

# Same shape the release workflow accepts: X.Y.Z with an optional prerelease
# suffix, no leading v, no + build metadata.
if ! printf '%s' "$version" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$'; then
  echo "check-latest-eligible: '$version' is not a version. Expected X.Y.Z or X.Y.Z-suffix, no leading v." >&2
  exit 2
fi

decide() { # decide <true|false> <reason>
  local answer="$1" reason="$2"
  if [ "$answer" = true ]; then
    echo "check-latest-eligible: :latest WILL move to v$version — $reason" >&2
  else
    # Loud on purpose: skipping this silently is how someone ends up believing
    # :latest tracks the release they just ran.
    echo "check-latest-eligible: :latest will NOT move — $reason" >&2
    if [ -n "${GITHUB_ACTIONS:-}" ]; then
      echo "::warning title=latest tag left untouched::v$version will not take the :latest image tag, which is left pointing at the newer version. $reason"
    fi
  fi
  [ -z "${GITHUB_OUTPUT:-}" ] || echo "latest=$answer" >> "$GITHUB_OUTPUT"
  printf '%s\n' "$answer"
  exit 0
}

# A prerelease never owns :latest: it would be handed to everyone running
# `docker pull ...:latest` or installing the previous release's install.yaml.
# Every dev build carries a suffix by construction, so this covers them too.
case "$version" in
  *-*) decide false "v$version is a prerelease" ;;
esac

# Only STABLE tags take part in the comparison, for two reasons: a prerelease
# is not a release :latest could be pointing at, and `sort -V` sorts
# 1.0.0-rc1 AFTER 1.0.0, the opposite of what semver says. Filtering them out
# keeps every remaining comparison a plain X.Y.Z one, where `sort -V` is right.
stable=()
while IFS= read -r tag; do
  [ -n "$tag" ] || continue
  candidate="${tag#v}"
  if printf '%s' "$candidate" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$'; then
    stable+=("$candidate")
  elif [ "${candidate#*-}" = "$candidate" ]; then
    # Not a prerelease and not X.Y.Z, so the release pipeline cannot have
    # created it — it was tagged by hand and is not a release to be newer than.
    echo "check-latest-eligible: ignoring tag '$tag', not a released version" >&2
  fi
done < <(git -C "$REPO_DIR" tag -l 'v[0-9]*' 2>/dev/null || true)

if [ "${#stable[@]}" -eq 0 ]; then
  decide true "no stable release exists yet, so this is the first one"
fi

newest="$(printf '%s\n' "${stable[@]}" | sort -V | tail -1)"

if [ "$version" = "$newest" ]; then
  # The tag exists and equals the version being built: a re-run of the current
  # newest release. :latest already points here, so re-pointing it is a no-op.
  decide true "v$version is the newest stable release"
fi

if [ "$(printf '%s\n%s\n' "$version" "$newest" | sort -V | head -1)" = "$newest" ]; then
  decide true "v$version is newer than the newest stable release, v$newest"
fi

decide false "v$version is older than the newest stable release, v$newest"
