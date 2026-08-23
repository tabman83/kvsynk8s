#!/usr/bin/env bash
#
# check-render.sh — assertions that must hold for ANY render of the chart,
# whatever values are used. Give it one or more rendered manifest files:
#
#   helm template ... > out.yaml && hack/check-render.sh out.yaml
#
# Checks:
#   1. the render is non-empty and every document parses as YAML;
#   2. no document is a Secret; no <SET-ME>-style placeholder survives into the
#      render; and no value sits under a credential-shaped key or looks like an
#      embedded private key (FR-013, SC-005, Constitution I). This is a tripwire
#      for the chart accidentally growing a place to put a secret, not a general
#      secret scanner — the real guarantee is structural: the only code that
#      ever writes a secret value into a Kubernetes object is
#      internal/sync/writer.go, and this chart templates no Secret at all;
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
PEM = re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----")

# Keys that should never carry a literal value in a chart that is configuration
# only. Keys ending in Name/File/Path/Ref/Dir name a reference, not a value, and
# are allowed — that is how bearerTokenFile and secretName pass.
CREDENTIAL_KEY = re.compile(
    r"(password|passwd|pwd|client[-_]?secret|api[-_]?key|access[-_]?key"
    r"|private[-_]?key|sas[-_]?token|connection[-_]?string|credential)",
    re.IGNORECASE,
)
REFERENCE_KEY = re.compile(r"(name|file|path|ref|dir|id)$", re.IGNORECASE)

failures = []


def scan_for_credentials(path, node, trail=""):
    """Walk the parsed render looking for a literal value under a credential-shaped key."""
    if isinstance(node, dict):
        for key, value in node.items():
            here = "%s.%s" % (trail, key)
            if (
                isinstance(key, str)
                and CREDENTIAL_KEY.search(key)
                and not REFERENCE_KEY.search(key)
                and isinstance(value, (str, int, float))
                and str(value).strip() != ""
            ):
                failures.append(
                    "%s: %s carries a literal value — the chart must not hold credentials" % (path, here)
                )
            scan_for_credentials(path, value, here)
    elif isinstance(node, list):
        for i, value in enumerate(node):
            scan_for_credentials(path, value, "%s[%d]" % (trail, i))


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

    before = len(failures)

    for doc in docs:
        name = doc.get("metadata", {}).get("name", "<unnamed>")
        if doc.get("kind") == "Secret":
            failures.append("%s: renders a Secret (%s) — the chart must never carry secret values" % (path, name))
        # The CRD body is controller-gen's schema, full of descriptions that
        # talk about secrets; scanning it produces only noise.
        if doc.get("kind") != "CustomResourceDefinition":
            scan_for_credentials(path, doc, doc.get("kind", "?"))

    for hit in set(PLACEHOLDER.findall(text)):
        failures.append("%s: render contains the placeholder %r" % (path, hit))

    if PEM.search(text):
        failures.append("%s: render contains an embedded private key" % path)

    if len(failures) == before:
        print(
            "%s: %d resources, no Secret, no duplicate keys, no placeholders, no credential-shaped values"
            % (path, len(docs))
        )
    else:
        print("%s: %d resources, %d problem(s)" % (path, len(docs), len(failures) - before))

if failures:
    print("\nrender check FAILED:", file=sys.stderr)
    for f in failures:
        print("  * %s" % f, file=sys.stderr)
    sys.exit(1)
PY
