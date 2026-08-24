#!/usr/bin/env bash
#
# check-values.sh — assert every documented value lands where
# specs/002-helm-chart/contracts/values.md says it does, and that every toggle
# actually turns its resources on and off.
#
# This is the contract's CI test #2. helm lint and the equivalence check only
# ever look at the default render, so without this a template could stop
# honouring a value and nothing would fail.
#
# Requires: helm, python3 with PyYAML.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

HELM="${HELM:-helm}"
CHART="charts/kvsynk8s"

command -v "$HELM" >/dev/null 2>&1 || { echo "check-values: helm not found (set \$HELM)" >&2; exit 1; }

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

render() { # render <outfile> [helm args...]
  local out="$1"; shift
  "$HELM" template kvsynk8s "$CHART" --namespace kvsynk8s "$@" > "$tmpdir/$out"
}

render default.yaml
render nondefault.yaml -f "$CHART/ci/nondefault-values.yaml"
render nometrics.yaml --set metrics.enabled=false
render nocrd.yaml --set crds.install=false
render nokeep.yaml --set crds.keep=false
render netpol.yaml --set networkPolicy.enabled=true

# serviceMonitor without metrics must be a render error with a useful message.
sm_err=0
"$HELM" template kvsynk8s "$CHART" --namespace kvsynk8s \
  --set metrics.enabled=false --set serviceMonitor.enabled=true \
  > "$tmpdir/sm.out" 2> "$tmpdir/sm.err" || sm_err=$?

python3 - "$tmpdir" "$sm_err" <<'PY'
import os
import sys
import yaml

tmpdir, sm_err = sys.argv[1], int(sys.argv[2])
failures = []


def load(name):
    with open(os.path.join(tmpdir, name)) as fh:
        return [d for d in yaml.safe_load_all(fh) if d]


def index(docs):
    return {(d["kind"], d["metadata"]["name"]): d for d in docs}


def want(cond, msg):
    if not cond:
        failures.append(msg)


def deployment(docs):
    for d in docs:
        if d["kind"] == "Deployment":
            return d
    raise AssertionError("no Deployment in render")


def container(docs):
    return deployment(docs)["spec"]["template"]["spec"]["containers"][0]


def podspec(docs):
    return deployment(docs)["spec"]["template"]["spec"]


def podlabels(docs):
    return deployment(docs)["spec"]["template"]["metadata"].get("labels", {})


# --- defaults --------------------------------------------------------------
d = load("default.yaml")
di = index(d)
args = container(d)["args"]

want("--metrics-bind-address=:8443" in args, "defaults: metrics arg missing")
want(("Service", "kvsynk8s-controller-manager-metrics-service") in di, "defaults: metrics Service missing")
want(("ClusterRole", "kvsynk8s-metrics-auth-role") in di, "defaults: metrics-auth ClusterRole missing")
want(("ClusterRole", "kvsynk8s-metrics-reader") in di, "defaults: metrics-reader ClusterRole missing")
want(not any(k[0] == "ServiceMonitor" for k in di), "defaults: ServiceMonitor should be off")
want(not any(k[0] == "NetworkPolicy" for k in di), "defaults: NetworkPolicy should be off")
want(not any(k[0] == "Namespace" for k in di), "defaults: chart must not render a Namespace")

# FR-011: with azure.clientID unset, none of the three markers may render.
want(not any(a.startswith("--azure-client-id") for a in args), "defaults: --azure-client-id must not render")
want("azure.workload.identity/use" not in podlabels(d), "defaults: WI pod label must not render")
sa = di[("ServiceAccount", "kvsynk8s-controller-manager")]
want(
    "azure.workload.identity/client-id" not in (sa["metadata"].get("annotations") or {}),
    "defaults: WI ServiceAccount annotation must not render",
)
want(not any(a.startswith("--queue-url") for a in args), "defaults: --queue-url must not render")
want(not any(a.startswith("--reconcile-interval") for a in args), "defaults: --reconcile-interval must not render")

# replicas is not a value and is always 1 (FR-009).
want(deployment(d)["spec"]["replicas"] == 1, "defaults: replicas must be 1")

crd = di[("CustomResourceDefinition", "secretsyncs.kvsynk8s.io")]
want(
    crd["metadata"]["annotations"].get("helm.sh/resource-policy") == "keep",
    "defaults: CRD must carry helm.sh/resource-policy: keep",
)

# --- every documented value set to a non-default ---------------------------
n = load("nondefault.yaml")
ni = index(n)
nargs = container(n)["args"]
nc = container(n)
nps = podspec(n)

want(
    "--queue-url=https://examplestorage.queue.core.windows.net/kv-events" in nargs,
    "nondefault: operator.queueURL did not land in args",
)
want("--reconcile-interval=30m" in nargs, "nondefault: operator.reconcileInterval did not land in args")
want(
    "--azure-client-id=00000000-0000-0000-0000-000000000000" in nargs,
    "nondefault: azure.clientID did not land in args",
)
# FR-011: all three workload-identity markers render together.
want(podlabels(n).get("azure.workload.identity/use") == "true", "nondefault: WI pod label missing")
nsa = ni[("ServiceAccount", "kvsynk8s-ci")]
want(
    nsa["metadata"]["annotations"].get("azure.workload.identity/client-id")
    == "00000000-0000-0000-0000-000000000000",
    "nondefault: WI ServiceAccount annotation missing or wrong",
)

want(nc["image"] == "ghcr.io/tabman83/kvsynk8s:ci-test", "nondefault: image.tag did not land (%s)" % nc["image"])
want(nc["imagePullPolicy"] == "Always", "nondefault: image.pullPolicy did not land")
want(nps["serviceAccountName"] == "kvsynk8s-ci", "nondefault: serviceAccount.name did not land on the pod")
want(nc["resources"]["requests"]["cpu"] == "25m", "nondefault: resources did not land")
want(nc["resources"]["limits"]["memory"] == "256Mi", "nondefault: resource limits did not land")
want(nps.get("nodeSelector") == {"kubernetes.io/os": "linux"}, "nondefault: nodeSelector did not land")
want(nps.get("tolerations"), "nondefault: tolerations did not land")
want(nps.get("affinity"), "nondefault: affinity did not land")
want(any(k[0] == "ServiceMonitor" for k in ni), "nondefault: ServiceMonitor missing")
want(any(k[0] == "NetworkPolicy" for k in ni), "nondefault: NetworkPolicy missing")

# The ClusterRoleBinding subjects must follow the renamed ServiceAccount, or
# the operator loses its permissions.
for binding in ("kvsynk8s-manager-rolebinding", "kvsynk8s-metrics-auth-rolebinding"):
    subj = ni[("ClusterRoleBinding", binding)]["subjects"][0]
    want(
        subj["name"] == "kvsynk8s-ci" and subj["namespace"] == "kvsynk8s",
        "nondefault: %s does not point at the overridden ServiceAccount" % binding,
    )

# --- metrics.enabled=false -------------------------------------------------
m = load("nometrics.yaml")
mi = index(m)
margs = container(m)["args"]
want(not any(a.startswith("--metrics-bind-address") for a in margs), "metrics off: arg still rendered")
want(not any(k[0] == "Service" for k in mi), "metrics off: Service still rendered")
want(("ClusterRole", "kvsynk8s-metrics-auth-role") not in mi, "metrics off: metrics-auth ClusterRole still rendered")
want(("ClusterRole", "kvsynk8s-metrics-reader") not in mi, "metrics off: metrics-reader ClusterRole still rendered")
want(
    ("ClusterRoleBinding", "kvsynk8s-metrics-auth-rolebinding") not in mi,
    "metrics off: metrics-auth binding still rendered",
)

# --- serviceMonitor without metrics ----------------------------------------
with open(os.path.join(tmpdir, "sm.err")) as fh:
    sm_message = fh.read()
want(sm_err != 0, "serviceMonitor.enabled=true with metrics off must fail the render")
want(
    "metrics.enabled=true" in sm_message,
    "serviceMonitor/metrics conflict must fail with a message naming metrics.enabled",
)

# --- CRD lifecycle gates ---------------------------------------------------
c = index(load("nocrd.yaml"))
want(
    ("CustomResourceDefinition", "secretsyncs.kvsynk8s.io") not in c,
    "crds.install=false: CRD still rendered",
)
want(len(c) == len(di) - 1, "crds.install=false: should drop exactly the CRD, got %d vs %d" % (len(c), len(di)))

k = index(load("nokeep.yaml"))
kcrd = k[("CustomResourceDefinition", "secretsyncs.kvsynk8s.io")]
want(
    "helm.sh/resource-policy" not in (kcrd["metadata"].get("annotations") or {}),
    "crds.keep=false: keep annotation still rendered",
)

# --- networkPolicy on its own ----------------------------------------------
np = [d for d in load("netpol.yaml") if d["kind"] == "NetworkPolicy"]
want(len(np) == 1, "networkPolicy.enabled=true: expected exactly one NetworkPolicy")
if np:
    ing = np[0]["spec"]["ingress"][0]
    want(
        ing["from"][0]["namespaceSelector"]["matchLabels"] == {"metrics": "enabled"},
        "networkPolicy: must only allow namespaces labeled metrics=enabled",
    )
    want(ing["ports"][0]["port"] == 8443, "networkPolicy: must only open the metrics port")

if failures:
    print("values contract check FAILED:", file=sys.stderr)
    for f in failures:
        print("  * %s" % f, file=sys.stderr)
    sys.exit(1)

print("values contract: every documented value lands where contracts/values.md says")
PY
