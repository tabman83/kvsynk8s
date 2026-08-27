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
#   IMAGE     image repository whose published tags are the second witness.
#             Defaults to the release image.
#   REGISTRY_TAGS
#             command run instead of the built-in registry read. It must exit 0
#             and print either one tag per line, nothing at all (no package
#             published yet), or the single word "unreachable". Test seam.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_DIR="${REPO_DIR:-$repo_root}"
IMAGE="${IMAGE:-ghcr.io/tabman83/kvsynk8s}"

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

# Only STABLE versions take part in the comparison, for two reasons: a
# prerelease is not a release :latest could be pointing at, and `sort -V` sorts
# 1.0.0-rc1 AFTER 1.0.0, the opposite of what semver says. Filtering them out
# keeps every remaining comparison a plain X.Y.Z one, where `sort -V` is right.
stable=()

# collect_stable reads tags on stdin and keeps the released versions. noisy
# says whether a tag that is neither a release nor a prerelease is worth
# reporting: a hand-made git tag is (someone made it deliberately, in this
# repository), a stray registry tag is not (the registry also holds sha256-*
# attestation tags and every dev build ever pushed).
collect_stable() { # collect_stable <noisy:true|false>
  local noisy="$1" tag candidate
  while IFS= read -r tag; do
    [ -n "$tag" ] || continue
    candidate="${tag#v}"
    if printf '%s' "$candidate" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$'; then
      stable+=("$candidate")
    elif [ "$noisy" = true ] && [ "${candidate#*-}" = "$candidate" ]; then
      # Not a prerelease and not X.Y.Z, so the release pipeline cannot have
      # created it — it was tagged by hand and is not a release to be newer than.
      echo "check-latest-eligible: ignoring tag '$tag', not a released version" >&2
    fi
  done
}

# --- witness 1: the git tags ------------------------------------------------
collect_stable true < <(git -C "$REPO_DIR" tag -l 'v[0-9]*' 2>/dev/null || true)

# --- witness 2: the registry ------------------------------------------------
#
# The tag is written by the LAST job of the release workflow, so a run that
# died after publishing leaves a fully released version with no tag behind it.
# A tag-only comparison cannot see that version, and would happily hand :latest
# to an older one dispatched afterwards — pointing every `docker pull :latest`
# and every `releases/latest` install back at the older operator. The image is
# pushed as v$VERSION by nothing but this pipeline, so the registry's own tag
# list is the second witness for "what has actually been released".
#
# Read anonymously: the package is public (README's install instructions rely
# on that), and doing it without credentials keeps this step working wherever
# it runs. Only stable versions are being looked for, so this is skipped
# entirely for prereleases and dev builds — they returned above.
default_registry_tags() {
  local host="${IMAGE%%/*}" repo="${IMAGE#*/}" body code token
  if [ "$host" != ghcr.io ]; then
    echo unreachable
    return 0
  fi
  body="$(mktemp)"
  # shellcheck disable=SC2064  # expand $body now: it is the file to remove.
  trap "rm -f '$body'" RETURN

  code="$(curl -sS -o "$body" -w '%{http_code}' \
    "https://${host}/token?service=${host}&scope=repository:${repo}:pull" 2>/dev/null || echo 000)"
  [ "$code" = 200 ] || { echo unreachable; return 0; }
  token="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("token",""))' "$body" 2>/dev/null || true)"
  [ -n "$token" ] || { echo unreachable; return 0; }

  # n=1000 is far above the number of tags this package will ever hold, so the
  # list comes back in one page and no Link-header paging is needed.
  code="$(curl -sS -o "$body" -w '%{http_code}' -H "Authorization: Bearer $token" \
    "https://${host}/v2/${repo}/tags/list?n=1000" 2>/dev/null || echo 000)"
  case "$code" in
    200) ;;
    404) return 0 ;;  # nothing published under this name yet
    *) echo unreachable; return 0 ;;
  esac
  python3 -c '
import json
import sys

for tag in json.load(open(sys.argv[1])).get("tags") or []:
    print(tag)
' "$body" 2>/dev/null || echo unreachable
}

registry_tags() {
  if [ -n "${REGISTRY_TAGS:-}" ]; then
    # shellcheck disable=SC2086  # unquoted on purpose: REGISTRY_TAGS may carry arguments.
    $REGISTRY_TAGS
  else
    default_registry_tags
  fi
}

published="$(registry_tags)"
if [ "$published" = unreachable ]; then
  # Fail closed, and it costs nothing: check-release-overwrite.sh runs moments
  # later in the same job and refuses outright when it cannot read the
  # registry, so no release that would have succeeded is losing :latest here.
  decide false "could not read the published tags of $IMAGE, so it is unknown whether a newer version is already released"
fi
collect_stable false <<< "$published"

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
