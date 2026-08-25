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
helm template kvsynk8s charts/kvsynk8s --namespace kvsynk8s > /tmp/render.yaml
hack/check-render.sh /tmp/render.yaml
grep -ri "SET-ME" charts/
```

Expected: `check-render.sh` reports no Secret, no duplicate keys and no
placeholders; the grep returns nothing.

Note: do not check this with `grep -i "kind: Secret"`. That also matches
`kind: SecretSync` and `listKind: SecretSyncList` inside the CRD schema, so it
always "finds" something. `check-render.sh` parses the render and looks at each
document's actual `kind`, which is the check that means anything.

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

# The chart's default image tag is the appVersion, which is the 0.0.0 dev
# placeholder in git, so build and load a local image for this run.
make docker-build IMG=kvsynk8s:helm-test
kind load docker-image kvsynk8s:helm-test --name helm-quickstart

helm install kvsynk8s charts/kvsynk8s --namespace kvsynk8s --create-namespace \
  --set image.repository=kvsynk8s --set image.tag=helm-test --set image.pullPolicy=Never
kubectl -n kvsynk8s rollout status deploy/kvsynk8s-operator
kubectl get crd secretsyncs.kvsynk8s.io

# Create a couple of SecretSync objects. Do NOT use config/samples/ — the
# scaffolded sample there still has an empty spec and the CRD rejects it.
cat <<'EOF' | kubectl apply -f -
apiVersion: kvsynk8s.io/v1alpha1
kind: SecretSync
metadata: {name: demo-one, namespace: default}
spec: {vault: {name: my-vault, secret: demo-password}}
---
apiVersion: kvsynk8s.io/v1alpha1
kind: SecretSync
metadata: {name: demo-two, namespace: kube-system}
spec: {vault: {name: my-vault, secret: other-password}, target: {secretName: renamed, dataKey: pw}}
EOF

helm uninstall kvsynk8s -n kvsynk8s
kubectl get crd secretsyncs.kvsynk8s.io      # expected: still present (crds.keep)
kubectl get secretsyncs -A                    # expected: both objects still present

# Reinstalling under the same release name and namespace needs no adoption
# step: the kept CRD still carries Helm's ownership label and annotations.
helm install kvsynk8s charts/kvsynk8s --namespace kvsynk8s \
  --set image.repository=kvsynk8s --set image.tag=helm-test --set image.pullPolicy=Never
kubectl get secretsyncs -A                    # expected: still both, statuses being set again

kind delete cluster --name helm-quickstart
```

Expected: zero SecretSync objects lost across the cycle. (Without Azure
credentials the operator reports `Failing` / `TransientError` on both objects —
that is fine; this scenario validates lifecycle, not sync.)

The research.md R2 adoption commands are for the *other* case: a CRD that Helm
never owned, i.e. one created by `kubectl apply -f install.yaml`. To exercise
that path, strip the ownership metadata first and watch the install refuse:

```bash
kubectl label crd secretsyncs.kvsynk8s.io app.kubernetes.io/managed-by-
kubectl annotate crd secretsyncs.kvsynk8s.io \
  meta.helm.sh/release-name- meta.helm.sh/release-namespace-
helm install kvsynk8s charts/kvsynk8s -n kvsynk8s   # expected: fails, naming all three fields

kubectl label crd secretsyncs.kvsynk8s.io app.kubernetes.io/managed-by=Helm
kubectl annotate crd secretsyncs.kvsynk8s.io \
  meta.helm.sh/release-name=kvsynk8s meta.helm.sh/release-namespace=kvsynk8s
helm install kvsynk8s charts/kvsynk8s -n kvsynk8s   # expected: succeeds, objects intact
```

Finally, the destructive branch of the data-model state table:

```bash
helm upgrade kvsynk8s charts/kvsynk8s -n kvsynk8s --set crds.keep=false
helm uninstall kvsynk8s -n kvsynk8s
kubectl get crd secretsyncs.kvsynk8s.io   # expected: NotFound
kubectl get secretsyncs -A                 # expected: NotFound (the CRD took them with it)
```

## 7. Release publishing (US3, FR-014) — after merge, on a real tag

Either push a tag, or run the workflow from the Actions tab and let it create
the tag (the version goes in without a leading `v`):

```bash
git tag vX.Y.Z && git push origin vX.Y.Z
# or:
gh workflow run Release -f version=X.Y.Z

# after the workflow completes:
helm install kvsynk8s oci://ghcr.io/tabman83/charts/kvsynk8s --version X.Y.Z \
  --namespace kvsynk8s --create-namespace --dry-run
gh release view vX.Y.Z --json assets -q '.assets[].name'
```

Expected: the OCI pull succeeds with app version `X.Y.Z`; release assets list
both `install.yaml` and `kvsynk8s-X.Y.Z.tgz`.

**Check the package visibility on the first release.** A new GHCR package can
land private (GitHub's default for a package scoped to a personal account),
and `helm push` sends nothing that links it to this repository, so it may not
inherit the repo's visibility the way the image does. If the `helm install`
above fails with `unauthorized`, set `ghcr.io/tabman83/charts/kvsynk8s` to
Public in its GitHub package settings — a one-time step. Check the same for
`ghcr.io/tabman83/kvsynk8s`, or the pod will not pull either. See
[contracts/release-artifacts.md](contracts/release-artifacts.md).
