# Quickstart: Validating the Helm Chart Feature

**Feature**: `002-helm-chart` | **Date**: 2026-08-23

Runnable scenarios that prove the feature works end-to-end. Contracts
referenced: [values.md](contracts/values.md),
[rendered-resources.md](contracts/rendered-resources.md),
[release-artifacts.md](contracts/release-artifacts.md).

## Prerequisites

- `helm` >= 3.8 (`helm version`)
- `make`, `docker`, `kind` (for the cluster scenario)
- Repo checked out on the feature branch

## 1. Chart is valid and renders with defaults (FR-001, FR-016)

```bash
helm lint charts/kvsynk8s
helm template kvsynk8s charts/kvsynk8s --namespace kvsynk8s
```

Expected: lint passes with 0 failures; template output is non-empty and
contains exactly the inventory in
[rendered-resources.md](contracts/rendered-resources.md) — 12 resources, no
Namespace, no ServiceMonitor, no NetworkPolicy.

## 2. Equivalence with install.yaml (SC-002, FR-004)

```bash
hack/compare-helm-kustomize.sh
```

Expected: exit 0. To see it fail meaningfully, change any resource name in a
chart template and re-run — the script must print the inventory diff.

## 3. Values land where the contract says (US2)

```bash
helm template kvsynk8s charts/kvsynk8s --namespace kvsynk8s \
  --set operator.queueURL=https://example.queue.core.windows.net/kv-events \
  --set operator.reconcileInterval=30m \
  --set azure.clientID=11111111-2222-3333-4444-555555555555 \
  --set serviceMonitor.enabled=true \
  --set networkPolicy.enabled=true \
  --set image.tag=test
```

Expected in the output:
- Deployment args contain `--queue-url=...`, `--reconcile-interval=30m`,
  `--azure-client-id=1111...`.
- Pod template label `azure.workload.identity/use: "true"`; ServiceAccount
  annotation `azure.workload.identity/client-id: 1111...`.
- A ServiceMonitor and a NetworkPolicy appear; image ends in `:test`.

Then verify the off-switches:

```bash
helm template kvsynk8s charts/kvsynk8s --namespace kvsynk8s \
  --set metrics.enabled=false | grep -E "metrics-bind-address|kind: Service"
```

Expected: no metrics Service, no `--metrics-bind-address` arg, and the
metrics-auth/reader RBAC is gone from the full output.

## 4. No secret values anywhere (SC-005, Constitution I)

```bash
helm template kvsynk8s charts/kvsynk8s --namespace kvsynk8s | grep -i "kind: Secret"
grep -ri "SET-ME" charts/
```

Expected: both greps return nothing.

## 5. Drift guardrail (US5, SC-006)

```bash
make manifests helm-sync
git diff --exit-code charts/        # expected: clean

# simulate drift: add a marker verb to the kubebuilder RBAC comment in
# internal/controller/secretsync_controller.go, then:
make manifests
git diff --exit-code charts/        # still clean — chart not yet synced
make helm-sync
git diff charts/                    # expected: shows the new rule in the chart
git checkout -- . && make manifests # restore
```

CI equivalent: the `helm.yml` workflow fails any PR where
`make manifests helm-sync` produces a diff.

## 6. Full lifecycle on a kind cluster (US1, US4, SC-004)

```bash
kind create cluster --name helm-quickstart
helm install kvsynk8s charts/kvsynk8s --namespace kvsynk8s --create-namespace \
  --set image.tag=v0.0.0-dev   # or a released tag
kubectl -n kvsynk8s rollout status deploy/kvsynk8s-operator
kubectl get crd secretsyncs.kvsynk8s.io

# create a SecretSync, then exercise uninstall/reinstall:
kubectl apply -f config/samples/
helm uninstall kvsynk8s -n kvsynk8s
kubectl get crd secretsyncs.kvsynk8s.io      # expected: still present (crds.keep)
kubectl get secretsyncs -A                    # expected: objects still present

# reinstall requires adopting the kept CRD (research.md R2):
kubectl label crd secretsyncs.kvsynk8s.io app.kubernetes.io/managed-by=Helm
kubectl annotate crd secretsyncs.kvsynk8s.io \
  meta.helm.sh/release-name=kvsynk8s meta.helm.sh/release-namespace=kvsynk8s
helm install kvsynk8s charts/kvsynk8s --namespace kvsynk8s
kind delete cluster --name helm-quickstart
```

Expected: zero SecretSync objects lost across the cycle. (Without Azure
credentials the operator reports `Failing` status on the sample — that is
fine; this scenario validates lifecycle, not sync.)

## 7. Release publishing (US3, FR-014) — after merge, on a real tag

```bash
git tag vX.Y.Z && git push origin vX.Y.Z
# after the workflow completes:
helm install kvsynk8s oci://ghcr.io/tabman83/charts/kvsynk8s --version X.Y.Z \
  --namespace kvsynk8s --create-namespace --dry-run
gh release view vX.Y.Z --json assets -q '.assets[].name'
```

Expected: the OCI pull succeeds with app version `X.Y.Z`; release assets list
both `install.yaml` and `kvsynk8s-X.Y.Z.tgz`.
