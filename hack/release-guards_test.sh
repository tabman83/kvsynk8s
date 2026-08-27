#!/usr/bin/env bash
#
# release-guards_test.sh — exercise the two release guards over the cases they
# exist for.
#
# Neither guard can be tried out by running the release workflow: getting it
# wrong means either a broken release or a silently overwritten one. Both
# scripts therefore take their inputs as arguments and read the world through
# two seams ($REPO_DIR for the git tags, $IMAGE_PROBE for the registry), so the
# whole decision table can be driven from a shell. This is that table.
#
# usage: hack/release-guards_test.sh
#
# Exits 0 if every case behaves as documented, 1 otherwise.

set -euo pipefail

hack_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LATEST="$hack_dir/check-latest-eligible.sh"
OVERWRITE="$hack_dir/check-release-overwrite.sh"

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

passed=0
failures=()

# --- harness ----------------------------------------------------------------

# A git repository with the given tags, each on its own commit. Prints its path.
make_repo() { # make_repo <name> [tag...]
  local name="$1"; shift
  local dir="$workdir/$name"
  mkdir -p "$dir"
  git -C "$dir" init -q
  git -C "$dir" config user.email test@example.com
  git -C "$dir" config user.name Test
  git -C "$dir" commit -q --allow-empty -m base
  local tag
  for tag in "$@"; do
    git -C "$dir" commit -q --allow-empty -m "$tag"
    git -C "$dir" tag "$tag"
  done
  printf '%s\n' "$dir"
}

commit_of() { # commit_of <repo> <rev>
  git -C "$1" rev-parse "$2"
}

# A stand-in for the registry. Prints whatever answer the case needs, and
# records that it was called so a case can assert it was NOT.
make_probe() { # make_probe <answer...>
  local script="$workdir/probe.sh"
  {
    echo '#!/usr/bin/env bash'
    echo "touch '$workdir/probe-called'"
    printf 'printf %%s\\\\n %q\n' "$*"
  } > "$script"
  chmod +x "$script"
  rm -f "$workdir/probe-called"
  printf '%s\n' "$script"
}

# check <name> <expected-exit> <expected-substring> -- <command...>
check() {
  local name="$1" want_exit="$2" want_text="$3"
  shift 4 # name, exit, text, "--"
  local out rc=0
  out="$("$@" 2>&1)" || rc=$?
  local problem=""
  if [ "$rc" -ne "$want_exit" ]; then
    problem="expected exit $want_exit, got $rc"
  elif [ -n "$want_text" ] && [[ "$out" != *"$want_text"* ]]; then
    problem="output did not contain '$want_text'"
  fi
  if [ -n "$problem" ]; then
    failures+=("$name: $problem")
    printf 'FAIL %s\n     %s\n' "$name" "$problem" >&2
    printf '%s\n' "$out" | sed 's/^/     | /' >&2
  else
    passed=$((passed + 1))
    printf 'ok   %s\n' "$name"
  fi
}

# --- check-latest-eligible.sh -----------------------------------------------

echo "== check-latest-eligible.sh =="

empty_repo="$(make_repo latest-empty)"
two_repo="$(make_repo latest-two v0.1.0 v0.2.0)"
double_digit_repo="$(make_repo latest-doubledigit v0.9.0 v0.10.0)"
prerelease_only_repo="$(make_repo latest-prerelease-only v0.2.0-rc1)"
junk_repo="$(make_repo latest-junk v0.1.0 v1.2 vfoo1)"

# The very first release must take :latest even though nothing came before it.
check "first-ever release takes :latest" 0 "true" -- \
  env REPO_DIR="$empty_repo" "$LATEST" 0.1.0
check "first-ever release says why" 0 "no stable release exists yet" -- \
  env REPO_DIR="$empty_repo" "$LATEST" 0.1.0

# The normal case: a new version, newer than everything released.
check "newer version takes :latest" 0 "true" -- \
  env REPO_DIR="$two_repo" "$LATEST" 0.3.0

# HOLE 1 itself: re-running an older stable release must leave :latest alone.
check "older version does not take :latest" 0 "false" -- \
  env REPO_DIR="$two_repo" "$LATEST" 0.1.0
check "older version warns about the newer one" 0 "older than the newest stable release, v0.2.0" -- \
  env REPO_DIR="$two_repo" "$LATEST" 0.1.0

# Re-running the newest release: :latest already points here, so this is a
# no-op rather than something to refuse.
check "equal to newest takes :latest" 0 "true" -- \
  env REPO_DIR="$two_repo" "$LATEST" 0.2.0

# Ordering must be semver-aware. Lexicographically 0.9.1 beats 0.10.0.
check "0.9.1 is older than 0.10.0" 0 "false" -- \
  env REPO_DIR="$double_digit_repo" "$LATEST" 0.9.1
check "0.11.0 is newer than 0.10.0" 0 "true" -- \
  env REPO_DIR="$double_digit_repo" "$LATEST" 0.11.0

# Prereleases and dev builds never own :latest, whatever the ordering says.
check "prerelease never takes :latest" 0 "false" -- \
  env REPO_DIR="$two_repo" "$LATEST" 0.3.0-rc1
check "dev build never takes :latest" 0 "is a prerelease" -- \
  env REPO_DIR="$two_repo" "$LATEST" 0.2.1-dev.42

# A repository holding only prerelease tags has no stable release to be older
# than, so the first stable one still takes :latest.
check "only prerelease tags counts as no stable release" 0 "true" -- \
  env REPO_DIR="$prerelease_only_repo" "$LATEST" 0.2.0

# Tags the release pipeline could never have created are not releases.
check "hand-made tags are ignored" 0 "true" -- \
  env REPO_DIR="$junk_repo" "$LATEST" 0.2.0
check "hand-made tags are reported" 0 "ignoring tag 'v1.2'" -- \
  env REPO_DIR="$junk_repo" "$LATEST" 0.2.0

# Malformed input makes no decision at all.
check "leading v is refused" 2 "is not a version" -- \
  env REPO_DIR="$two_repo" "$LATEST" v0.3.0
check "two-part version is refused" 2 "is not a version" -- \
  env REPO_DIR="$two_repo" "$LATEST" 1.2
check "build metadata is refused" 2 "is not a version" -- \
  env REPO_DIR="$two_repo" "$LATEST" "1.2.3+build"
check "no arguments is refused" 2 "usage:" -- \
  env REPO_DIR="$two_repo" "$LATEST"

# GITHUB_OUTPUT gets the decision the workflow gates on.
github_output="$workdir/github_output"
: > "$github_output"
GITHUB_OUTPUT="$github_output" REPO_DIR="$two_repo" "$LATEST" 0.1.0 >/dev/null 2>&1
check "writes latest=false to GITHUB_OUTPUT" 0 "latest=false" -- cat "$github_output"

# --- check-release-overwrite.sh ---------------------------------------------

echo "== check-release-overwrite.sh =="

tagged_repo="$(make_repo overwrite-tagged v0.1.0)"
released_commit="$(commit_of "$tagged_repo" v0.1.0)"
other_commit="$(commit_of "$tagged_repo" HEAD~1)"
untagged_repo="$(make_repo overwrite-untagged)"
head_commit="$(commit_of "$untagged_repo" HEAD)"

# A genuinely new version: no tag, nothing in the registry.
absent_probe="$(make_probe absent)"
check "new version with nothing published" 0 "is a new version" -- \
  env REPO_DIR="$untagged_repo" IMAGE_PROBE="$absent_probe" \
  "$OVERWRITE" 0.1.0 "$head_commit"

# The documented re-run: the tag exists and points at the commit being built.
check "tag on the same commit is a re-run" 0 "treating this as a re-run" -- \
  env REPO_DIR="$tagged_repo" IMAGE_PROBE="$absent_probe" \
  "$OVERWRITE" 0.1.0 "$released_commit"

# The tag settles it on its own; the registry is not even consulted.
rm -f "$workdir/probe-called"
env REPO_DIR="$tagged_repo" IMAGE_PROBE="$absent_probe" \
  "$OVERWRITE" 0.1.0 "$released_commit" >/dev/null 2>&1
check "tag on the same commit skips the registry probe" 1 "" -- \
  test -e "$workdir/probe-called"

# The original tag-based guard, unchanged.
check "tag on a different commit is refused" 1 "not at $other_commit" -- \
  env REPO_DIR="$tagged_repo" IMAGE_PROBE="$absent_probe" \
  "$OVERWRITE" 0.1.0 "$other_commit"

# HOLE 2, the safe half: the release job failed, so there is no tag, but the
# image is published from this same commit. Re-running must still work.
same_commit_probe="$(make_probe "revision $head_commit")"
check "published from the same commit is a re-run" 0 "release run failed after publishing" -- \
  env REPO_DIR="$untagged_repo" IMAGE_PROBE="$same_commit_probe" \
  "$OVERWRITE" 0.1.0 "$head_commit"

# HOLE 2 itself: no tag, but the registry already holds this version built
# from a different commit.
foreign_commit="0000000000000000000000000000000000000abc"
foreign_probe="$(make_probe "revision $foreign_commit")"
check "published from a different commit is refused" 1 "built from commit $foreign_commit" -- \
  env REPO_DIR="$untagged_repo" IMAGE_PROBE="$foreign_probe" \
  "$OVERWRITE" 0.1.0 "$head_commit"
check "refusal points at the original run" 1 "Re-run the original failed run" -- \
  env REPO_DIR="$untagged_repo" IMAGE_PROBE="$foreign_probe" \
  "$OVERWRITE" 0.1.0 "$head_commit"

# Published, but the revision cannot be read: fail closed.
norevision_probe="$(make_probe norevision)"
check "published without a revision label is refused" 1 "no readable org.opencontainers.image.revision" -- \
  env REPO_DIR="$untagged_repo" IMAGE_PROBE="$norevision_probe" \
  "$OVERWRITE" 0.1.0 "$head_commit"

# Registry unreachable: also fail closed.
unreachable_probe="$(make_probe unreachable)"
check "unreachable registry is refused" 1 "Refusing to publish rather than risk overwriting" -- \
  env REPO_DIR="$untagged_repo" IMAGE_PROBE="$unreachable_probe" \
  "$OVERWRITE" 0.1.0 "$head_commit"

# A probe that answers something else is a bug, not a verdict.
nonsense_probe="$(make_probe "who knows")"
check "unknown probe answer is an error" 2 "not an answer I understand" -- \
  env REPO_DIR="$untagged_repo" IMAGE_PROBE="$nonsense_probe" \
  "$OVERWRITE" 0.1.0 "$head_commit"

# Malformed input.
check "bad version is refused" 2 "is not a version" -- \
  env REPO_DIR="$untagged_repo" IMAGE_PROBE="$absent_probe" \
  "$OVERWRITE" v0.1.0 "$head_commit"
check "bad commit is refused" 2 "is not a commit sha" -- \
  env REPO_DIR="$untagged_repo" IMAGE_PROBE="$absent_probe" \
  "$OVERWRITE" 0.1.0 not-a-sha
check "missing arguments is refused" 2 "usage:" -- \
  env REPO_DIR="$untagged_repo" IMAGE_PROBE="$absent_probe" "$OVERWRITE" 0.1.0

# --- reading the revision label off a real manifest shape --------------------
#
# The cases above stub the registry out entirely, so they never exercise the
# part that reads org.opencontainers.image.revision back off the image. These
# feed the built-in probe the shape `docker buildx imagetools inspect --format
# '{{json .Image}}'` really produces for the multi-arch manifest list this
# project publishes: a map of platform to image config.

echo "== reading the revision label =="

inspect_json() { # inspect_json <name> <amd64-revision-or-empty> <arm64-revision-or-empty>
  local path="$workdir/$1.json"
  python3 - "$path" "$2" "$3" <<'PY'
import json
import sys

path, amd64, arm64 = sys.argv[1], sys.argv[2], sys.argv[3]


def platform(arch, revision):
    labels = {"org.opencontainers.image.source": "https://github.com/tabman83/kvsynk8s"}
    if revision:
        labels["org.opencontainers.image.revision"] = revision
    return {
        "created": "2026-08-25T06:16:08.891355104Z",
        "architecture": arch,
        "os": "linux",
        "config": {"User": "65532:65532", "Entrypoint": ["/manager"], "Labels": labels},
        "rootfs": {"type": "layers", "diff_ids": ["sha256:" + "0" * 64]},
    }


with open(path, "w") as fh:
    json.dump({"linux/amd64": platform("amd64", amd64), "linux/arm64": platform("arm64", arm64)}, fh)
PY
  printf '%s\n' "$path"
}

matching_json="$(inspect_json matching "$head_commit" "$head_commit")"
check "revision label read off a multi-arch manifest" 0 "release run failed after publishing" -- \
  env REPO_DIR="$untagged_repo" IMAGE_INSPECT_JSON="$matching_json" \
  "$OVERWRITE" 0.1.0 "$head_commit"

foreign_json="$(inspect_json foreign "$foreign_commit" "$foreign_commit")"
check "revision label naming another commit is refused" 1 "built from commit $foreign_commit" -- \
  env REPO_DIR="$untagged_repo" IMAGE_INSPECT_JSON="$foreign_json" \
  "$OVERWRITE" 0.1.0 "$head_commit"

# What ghcr.io actually holds for v0.1.0 today: source label only, built before
# the revision label existed.
unlabelled_json="$(inspect_json unlabelled "" "")"
check "manifest with no revision label reads as norevision" 1 "no readable org.opencontainers.image.revision" -- \
  env REPO_DIR="$untagged_repo" IMAGE_INSPECT_JSON="$unlabelled_json" \
  "$OVERWRITE" 0.1.0 "$head_commit"

# Platforms disagreeing means the answer is not trustworthy, which is the same
# as not knowing, so it must not be read as a match.
disagreeing_json="$(inspect_json disagreeing "$head_commit" "$foreign_commit")"
check "platforms disagreeing is not a match" 1 "no readable org.opencontainers.image.revision" -- \
  env REPO_DIR="$untagged_repo" IMAGE_INSPECT_JSON="$disagreeing_json" \
  "$OVERWRITE" 0.1.0 "$head_commit"

# Only one platform carrying the label is still a single unambiguous answer.
partial_json="$(inspect_json partial "$head_commit" "")"
check "one platform carrying the label is enough" 0 "release run failed after publishing" -- \
  env REPO_DIR="$untagged_repo" IMAGE_INSPECT_JSON="$partial_json" \
  "$OVERWRITE" 0.1.0 "$head_commit"

# Unparseable output is "cannot tell", not "nothing published".
printf 'ERROR: something went wrong\n' > "$workdir/garbage.json"
check "unparseable inspect output fails closed" 1 "Refusing to publish rather than risk overwriting" -- \
  env REPO_DIR="$untagged_repo" IMAGE_INSPECT_JSON="$workdir/garbage.json" \
  "$OVERWRITE" 0.1.0 "$head_commit"

# --- result -----------------------------------------------------------------

echo
if [ "${#failures[@]}" -gt 0 ]; then
  echo "release guards: ${#failures[@]} case(s) FAILED, $passed passed" >&2
  printf '  * %s\n' "${failures[@]}" >&2
  exit 1
fi
echo "release guards: all $passed cases behave as documented"
