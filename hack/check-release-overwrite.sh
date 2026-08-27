#!/usr/bin/env bash
#
# check-release-overwrite.sh — refuse to rebuild an already published version
# from a different commit.
#
# The release workflow creates the git tag LAST, in the final `release` job, so
# that a failed release leaves no dangling tag behind. The image and the chart
# are pushed to GHCR well before that. That leaves a state a tag-only guard
# cannot see: version X.Y.Z fully published to the registry, with no tag. If the
# release job fails and the operator then dispatches the same version again from
# a newer master HEAD — instead of re-running the failed run, which is the
# remedy the release contract prescribes (specs/002-helm-chart/research.md R9,
# and the "pipeline fails after the image is pushed" edge case in spec.md) —
# the image, the chart and :latest are silently replaced by a different
# commit's build under a version number people already have.
#
# So the guard asks both witnesses: the git tag, and the registry itself. The
# image carries org.opencontainers.image.revision (set from the Dockerfile's
# GIT_REVISION build arg) precisely so the registry can say WHICH commit it was
# built from, which is what separates the legitimate same-commit re-run from
# the overwrite.
#
# usage: check-release-overwrite.sh <version> <commit-sha>
#
#   <version>     the version being released, no leading v.
#   <commit-sha>  the commit this run is building (GITHUB_SHA).
#
# Exit 0 = go ahead, 1 = refused, 2 = bad usage/input.
#
# Env:
#   IMAGE        image repository to probe. Defaults to the release image.
#   REPO_DIR     git repository whose tags are consulted. Test seam.
#   IMAGE_PROBE  command run as `$IMAGE_PROBE <image-ref>` instead of the
#                built-in docker probe. Test seam for the decision table. It
#                must exit 0 and print exactly one of: "absent", "unreachable",
#                "norevision", "revision <sha>".
#   IMAGE_INSPECT_JSON
#                file holding what `imagetools inspect` would have printed,
#                used instead of calling docker. Test seam for the label
#                reading itself, which is the half IMAGE_PROBE stubs out.
#
# Requires: docker (with buildx) and python3, unless $IMAGE_PROBE replaces them.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_DIR="${REPO_DIR:-$repo_root}"
IMAGE="${IMAGE:-ghcr.io/tabman83/kvsynk8s}"

usage() {
  echo "usage: check-release-overwrite.sh <version> <commit-sha>" >&2
}

[ "$#" -eq 2 ] || { usage; exit 2; }
version="$1"
commit="$2"

if ! printf '%s' "$version" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$'; then
  echo "check-release-overwrite: '$version' is not a version. Expected X.Y.Z or X.Y.Z-suffix, no leading v." >&2
  exit 2
fi

if ! printf '%s' "$commit" | grep -qE '^[0-9a-f]{7,40}$'; then
  echo "check-release-overwrite: '$commit' is not a commit sha." >&2
  exit 2
fi

tag="v$version"

refuse() { # refuse <line...>
  printf 'check-release-overwrite: REFUSED\n' >&2
  local line
  for line in "$@"; do
    printf '  %s\n' "$line" >&2
  done
  if [ -n "${GITHUB_ACTIONS:-}" ]; then
    echo "::error title=Refusing to overwrite $tag::$1"
  fi
  exit 1
}

# Reads the revision label back off whatever the registry holds. `imagetools
# inspect` prints the config of every platform in the manifest list, so the
# label is looked for at any depth rather than at a shape that depends on
# whether the image is multi-arch.
default_probe() { # default_probe <image-ref>
  local ref="$1" out rc=0
  if [ -n "${IMAGE_INSPECT_JSON:-}" ]; then
    out="$(cat "$IMAGE_INSPECT_JSON")"
  else
    out="$(docker buildx imagetools inspect "$ref" --format '{{json .Image}}' 2>&1)" || rc=$?
  fi
  if [ "$rc" -ne 0 ]; then
    case "$out" in
      *"manifest unknown"*|*MANIFEST_UNKNOWN*|*"not found"*|*NAME_UNKNOWN*|*"no such manifest"*)
        echo absent ;;
      *)
        echo unreachable ;;
    esac
    return 0
  fi
  printf '%s' "$out" | python3 -c '
import json
import sys

LABEL = "org.opencontainers.image.revision"
found = set()


def walk(node):
    if isinstance(node, dict):
        labels = node.get("config", {})
        if isinstance(labels, dict):
            value = (labels.get("Labels") or {}).get(LABEL)
            if value:
                found.add(value)
        for value in node.values():
            walk(value)
    elif isinstance(node, list):
        for value in node:
            walk(value)


try:
    walk(json.load(sys.stdin))
except Exception:
    print("unreachable")
    sys.exit(0)

# More than one distinct revision across the platforms of one manifest list
# means the answer is not trustworthy, which is the same as not knowing.
print("revision " + found.pop() if len(found) == 1 else "norevision")
'
}

probe() { # probe <image-ref>
  if [ -n "${IMAGE_PROBE:-}" ]; then
    # shellcheck disable=SC2086  # unquoted on purpose: IMAGE_PROBE may carry arguments.
    $IMAGE_PROBE "$1"
  else
    default_probe "$1"
  fi
}

# --- witness 1: the git tag -------------------------------------------------
#
# When the tag exists the release completed, because the tag is written last.
# Same commit is a deliberate re-run and converges by design; any other commit
# is a different build wearing a published version number.
if tag_commit="$(git -C "$REPO_DIR" rev-parse -q --verify "refs/tags/$tag^{commit}" 2>/dev/null)"; then
  if [ "$tag_commit" = "$commit" ]; then
    echo "check-release-overwrite: tag $tag already exists and points at this commit; treating this as a re-run of the same release." >&2
    exit 0
  fi
  refuse \
    "tag $tag already exists and points at $tag_commit, not at $commit." \
    "Refusing to rebuild a released version from a different commit." \
    "Pick a new version, or delete the tag first if you really mean to replace the release."
fi

# --- witness 2: the registry ------------------------------------------------
#
# No tag, so either this version was never released, or its release run died
# after publishing the image and the chart.
result="$(probe "$IMAGE:$tag")"

case "$result" in
  absent)
    echo "check-release-overwrite: $IMAGE:$tag is not published yet; v$version is a new version." >&2
    exit 0
    ;;
  "revision $commit")
    echo "check-release-overwrite: $IMAGE:$tag is already published from this same commit but has no git tag, so its release run failed after publishing. Re-running it from the same commit is the documented remedy; continuing." >&2
    exit 0
    ;;
  revision\ *)
    refuse \
      "$IMAGE:$tag is already published, built from commit ${result#revision } — but no git tag $tag exists, so that release run failed after publishing the image and the chart." \
      "This run is building $commit, so continuing would overwrite a published version with a different commit's build." \
      "Re-run the original failed run instead of dispatching v$version again from a newer commit."
    ;;
  norevision)
    # Fail closed. An image with no readable revision and no git tag cannot be
    # told apart from someone else's build of the same version number, and
    # guessing in the permissive direction is exactly the hole this closes.
    refuse \
      "$IMAGE:$tag is already published but carries no readable org.opencontainers.image.revision, and no git tag $tag exists." \
      "There is no way to tell whether this run would republish the same commit or overwrite a different one." \
      "Re-run the original failed run, or delete $IMAGE:$tag from the registry if you are sure it should be replaced."
    ;;
  unreachable)
    # Also fail closed, and it costs nothing: if the registry cannot be read
    # here, `docker buildx build --push` a few minutes later cannot write to it
    # either, so no release that would have succeeded is being blocked.
    refuse \
      "could not read $IMAGE:$tag from the registry, so it is unknown whether v$version is already published." \
      "Refusing to publish rather than risk overwriting a released version." \
      "Check the registry is reachable and the run has permission to read the package, then try again."
    ;;
  *)
    echo "check-release-overwrite: image probe returned '$result', which is not an answer I understand." >&2
    exit 2
    ;;
esac
