#!/usr/bin/env bash
#
# compare-helm-kustomize.sh — assert the chart's default render is equivalent to
# the kustomize output that produces install.yaml (SC-002, FR-004).
#
# "Equivalent" is defined by specs/002-helm-chart/contracts/rendered-resources.md:
#
#   * the sorted kind/namespace/name inventory matches, ignoring the Namespace
#     object (Helm creates the namespace with --create-namespace instead);
#   * the Deployment's replicas, serviceAccountName, selector labels, pod and
#     container securityContext, args, ports, probes, resources,
#     terminationGracePeriodSeconds and the kubectl.kubernetes.io/default-container
#     pod annotation all match;
#   * the manager ClusterRole's rules match byte-for-byte.
#
# Differences the contract tolerates: the container image (kustomize renders the
# dev placeholder controller:latest), Helm vs kustomize release metadata labels,
# and the <SET-ME> workload-identity placeholder that install.yaml carries but
# the chart only renders when azure.clientID is set.
#
# Requires: helm, kustomize (bin/kustomize by default), python3 with PyYAML.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

HELM="${HELM:-helm}"
KUSTOMIZE="${KUSTOMIZE:-bin/kustomize}"

command -v "$HELM" >/dev/null 2>&1 || { echo "compare: helm not found (set \$HELM)" >&2; exit 1; }
[ -x "$KUSTOMIZE" ] || command -v "$KUSTOMIZE" >/dev/null 2>&1 || {
  echo "compare: kustomize not found at '$KUSTOMIZE' (set \$KUSTOMIZE, or run 'make kustomize')" >&2
  exit 1
}
python3 -c 'import yaml' 2>/dev/null || {
  echo "compare: python3 with PyYAML is required (pip install pyyaml)" >&2
  exit 1
}

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

"$HELM" template kvsynk8s charts/kvsynk8s --namespace kvsynk8s > "$tmpdir/helm.yaml"
"$KUSTOMIZE" build config/default > "$tmpdir/kustomize.yaml"

python3 - "$tmpdir/helm.yaml" "$tmpdir/kustomize.yaml" <<'PY'
import sys
import yaml

helm_path, kustomize_path = sys.argv[1], sys.argv[2]


def load(path):
    with open(path) as fh:
        return [d for d in yaml.safe_load_all(fh) if d]


def key(doc):
    md = doc.get("metadata", {})
    return (doc["kind"], md.get("namespace", ""), md["name"])


helm_docs = load(helm_path)
# The Namespace object is Helm's job (--create-namespace), so it is excluded.
kust_docs = [d for d in load(kustomize_path) if d["kind"] != "Namespace"]

failures = []


def check(label, got, want):
    if got != want:
        failures.append(
            "%s\n    chart:     %r\n    kustomize: %r" % (label, got, want)
        )


# --- 1. resource inventory -------------------------------------------------
helm_keys = sorted(key(d) for d in helm_docs)
kust_keys = sorted(key(d) for d in kust_docs)

if helm_keys != kust_keys:
    only_helm = [k for k in helm_keys if k not in kust_keys]
    only_kust = [k for k in kust_keys if k not in helm_keys]
    lines = ["resource inventory differs"]
    for k in only_helm:
        lines.append("    + only in chart:     %s %s/%s" % (k[0], k[1] or "<cluster>", k[2]))
    for k in only_kust:
        lines.append("    - only in kustomize: %s %s/%s" % (k[0], k[1] or "<cluster>", k[2]))
    failures.append("\n".join(lines))

by_key_helm = {key(d): d for d in helm_docs}
by_key_kust = {key(d): d for d in kust_docs}

# --- 2. Deployment field equivalence ---------------------------------------
dep_key = ("Deployment", "kvsynk8s", "kvsynk8s-operator")
if dep_key in by_key_helm and dep_key in by_key_kust:
    h = by_key_helm[dep_key]["spec"]
    k = by_key_kust[dep_key]["spec"]

    check("Deployment .spec.replicas", h.get("replicas"), k.get("replicas"))
    check(
        "Deployment .spec.selector.matchLabels",
        h.get("selector", {}).get("matchLabels"),
        k.get("selector", {}).get("matchLabels"),
    )

    hp, kp = h["template"], k["template"]
    check(
        "pod annotation kubectl.kubernetes.io/default-container",
        hp["metadata"].get("annotations", {}).get("kubectl.kubernetes.io/default-container"),
        kp["metadata"].get("annotations", {}).get("kubectl.kubernetes.io/default-container"),
    )
    # The pod must keep carrying the selector labels, or the Deployment cannot
    # adopt it and the metrics Service / NetworkPolicy stop matching.
    for label in ("control-plane", "app.kubernetes.io/name"):
        check(
            "pod label %s" % label,
            hp["metadata"].get("labels", {}).get(label),
            kp["metadata"].get("labels", {}).get(label),
        )

    hs, ks = hp["spec"], kp["spec"]
    check("pod .securityContext", hs.get("securityContext"), ks.get("securityContext"))
    check("pod .serviceAccountName", hs.get("serviceAccountName"), ks.get("serviceAccountName"))
    check(
        "pod .terminationGracePeriodSeconds",
        hs.get("terminationGracePeriodSeconds"),
        ks.get("terminationGracePeriodSeconds"),
    )

    hc = {c["name"]: c for c in hs["containers"]}
    kc = {c["name"]: c for c in ks["containers"]}
    check("container names", sorted(hc), sorted(kc))

    if "manager" in hc and "manager" in kc:
        a, b = hc["manager"], kc["manager"]
        for field in (
            "args",
            "command",
            "ports",
            "securityContext",
            "livenessProbe",
            "readinessProbe",
            "resources",
        ):
            check("container manager .%s" % field, a.get(field), b.get(field))
else:
    failures.append("Deployment %s missing from one of the renders" % (dep_key,))

# --- 3. manager ClusterRole rules ------------------------------------------
role_key = ("ClusterRole", "", "kvsynk8s-manager-role")
if role_key in by_key_helm and role_key in by_key_kust:
    check(
        "ClusterRole kvsynk8s-manager-role .rules",
        by_key_helm[role_key].get("rules"),
        by_key_kust[role_key].get("rules"),
    )
else:
    failures.append("ClusterRole kvsynk8s-manager-role missing from one of the renders")

# --- 4. bindings reference the same role/subject pairs ---------------------
for name in ("kvsynk8s-manager-rolebinding", "kvsynk8s-metrics-auth-rolebinding"):
    bk = ("ClusterRoleBinding", "", name)
    if bk in by_key_helm and bk in by_key_kust:
        check("%s .roleRef" % name, by_key_helm[bk].get("roleRef"), by_key_kust[bk].get("roleRef"))
        check("%s .subjects" % name, by_key_helm[bk].get("subjects"), by_key_kust[bk].get("subjects"))
    else:
        failures.append("ClusterRoleBinding %s missing from one of the renders" % name)

# --- 5. metrics Service ----------------------------------------------------
svc_key = ("Service", "kvsynk8s", "kvsynk8s-controller-manager-metrics-service")
if svc_key in by_key_helm and svc_key in by_key_kust:
    check(
        "metrics Service .spec.ports",
        by_key_helm[svc_key]["spec"].get("ports"),
        by_key_kust[svc_key]["spec"].get("ports"),
    )
    check(
        "metrics Service .spec.selector",
        by_key_helm[svc_key]["spec"].get("selector"),
        by_key_kust[svc_key]["spec"].get("selector"),
    )
else:
    failures.append("metrics Service missing from one of the renders")

# --- 6. the CRD is the same object -----------------------------------------
crd_key = ("CustomResourceDefinition", "", "secretsyncs.kvsynk8s.io")
if crd_key in by_key_helm and crd_key in by_key_kust:
    check("CRD .spec", by_key_helm[crd_key].get("spec"), by_key_kust[crd_key].get("spec"))
else:
    failures.append("SecretSync CRD missing from one of the renders")

if failures:
    print("chart render is NOT equivalent to the kustomize output:\n", file=sys.stderr)
    for f in failures:
        print("  * %s\n" % f, file=sys.stderr)
    sys.exit(1)

print("chart render is equivalent to kustomize build config/default (%d resources)" % len(helm_docs))
PY
