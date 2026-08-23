#!/usr/bin/env bash
#
# check-render.sh — assertions that must hold for ANY render of the chart,
# whatever values are used. Give it one or more rendered manifest files:
#
#   helm template ... > out.yaml && hack/check-render.sh out.yaml
#
# Checks:
#   1. the render is non-empty and every document parses as YAML;
#   2. no document is a Secret, and no value in the render looks like a
#      credential or a <SET-ME>-style placeholder (FR-013, SC-005, Constitution I);
#   3. no mapping has a duplicate key — YAML parsers silently keep the last one,
#      so a template that emits the same label twice is a real bug that reading
#      the parsed output cannot catch.
#
# Requires: python3 with PyYAML.

set -euo pipefail

[ "$#" -ge 1 ] || { echo "usage: check-render.sh <rendered.yaml> [...]" >&2; exit 1; }

python3 - "$@" <<'PY'
import re
import sys
import yaml

PLACEHOLDER = re.compile(r"<\s*SET[-_ ]?ME\s*>", re.IGNORECASE)

failures = []


class DupCheckLoader(yaml.SafeLoader):
    """SafeLoader that refuses duplicate mapping keys instead of keeping the last."""


def _no_duplicates(loader, node, deep=False):
    seen = set()
    for key_node, _ in node.value:
        key = loader.construct_object(key_node, deep=deep)
        if key in seen:
            raise yaml.constructor.ConstructorError(
                None, None, "duplicate key %r" % (key,), key_node.start_mark
            )
        seen.add(key)
    return yaml.SafeLoader.construct_mapping(loader, node, deep)


DupCheckLoader.add_constructor(
    yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _no_duplicates
)

for path in sys.argv[1:]:
    with open(path) as fh:
        text = fh.read()

    try:
        docs = [d for d in yaml.load_all(text, Loader=DupCheckLoader) if d]
    except yaml.YAMLError as exc:
        failures.append("%s: %s" % (path, exc))
        continue

    if not docs:
        failures.append("%s: render is empty" % path)
        continue

    for doc in docs:
        name = doc.get("metadata", {}).get("name", "<unnamed>")
        if doc.get("kind") == "Secret":
            failures.append("%s: renders a Secret (%s) — the chart must never carry secret values" % (path, name))

    for hit in set(PLACEHOLDER.findall(text)):
        failures.append("%s: render contains the placeholder %r" % (path, hit))

    print("%s: %d resources, no Secret, no duplicate keys, no placeholders" % (path, len(docs)))

if failures:
    print("\nrender check FAILED:", file=sys.stderr)
    for f in failures:
        print("  * %s" % f, file=sys.stderr)
    sys.exit(1)
PY
