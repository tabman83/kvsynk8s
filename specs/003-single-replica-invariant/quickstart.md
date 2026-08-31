# Quickstart: validating the single-replica invariant

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Date**: 2026-08-30

How to prove this feature works. Everything except the last section runs locally or in CI;
the last section is the manual check that a human does once, because it is the one thing an
automated test cannot conclude (research [R3](./research.md)).

## Prerequisites

- `helm` >= 3.8, `python3` with PyYAML, Docker, and the repo's usual toolchain
- `make kustomize` once, so `bin/kustomize` exists

## 1. Both install paths render the strategy

```bash
make helm-verify
helm template kvsynk8s charts/kvsynk8s --namespace kvsynk8s > /tmp/render.yaml
hack/check-render.sh /tmp/render.yaml
```

Two commands, not one. `make helm-verify` runs `helm-sync`, `helm lint` over the default and
non-default values, `hack/check-values.sh` and the kustomize equivalence check — but **not**
`hack/check-render.sh`, which today only runs in the `helm.yml` workflow's "Render the chart"
step. Since this feature puts one of its two enforcement points in that script, the plan
proposes folding it into `helm-verify` so local and CI cover the same ground; until that
lands, run it by hand as above.

Spot-check either path by hand:

```bash
helm template kvsynk8s charts/kvsynk8s --namespace kvsynk8s \
  | python3 -c 'import sys,yaml; print([d["spec"]["strategy"] for d in yaml.safe_load_all(sys.stdin) if d and d["kind"]=="Deployment"])'
# expected: [{'type': 'Recreate'}]

bin/kustomize build config/default \
  | python3 -c 'import sys,yaml; print([d["spec"]["strategy"] for d in yaml.safe_load_all(sys.stdin) if d and d["kind"]=="Deployment"])'
# expected: [{'type': 'Recreate'}]
```

**Expected**: both print `Recreate`, with no `rollingUpdate` key.

## 2. The checks actually fail when the change is reverted

This is the Constitution IV evidence, and it is worth running deliberately rather than
assuming — research [R2](./research.md) explains why one of these two checks passes on its
own even with the field missing from both paths.

```bash
# temporarily delete the strategy from the chart only
hack/compare-helm-kustomize.sh   # expect: FAIL, naming .spec.strategy
helm template kvsynk8s charts/kvsynk8s --namespace kvsynk8s > /tmp/r.yaml
hack/check-render.sh /tmp/r.yaml # expect: FAIL

# now delete it from both paths
hack/compare-helm-kustomize.sh   # expect: PASS  <- this is why the render check exists
hack/check-render.sh /tmp/r.yaml # expect: FAIL
```

Restore the files afterwards (`git checkout -- charts config`).

## 3. The listener waits for leadership

```bash
make test
```

The new envtest spec in `internal/controller` starts a manager with leader election enabled
against a Lease held by another identity and asserts the listener never polls its queue.

**Expected**: passes. To confirm it is a real test, set `NeedLeaderElection()` back to
`false` in `internal/events/listener.go` and re-run — it must fail.

Nothing else in the Go suite should change:

```bash
go test ./internal/events/...   # no KUBEBUILDER_ASSETS needed, as before
```

## 4. Nothing about the running operator changed

```bash
make test-e2e
```

**Expected**: 8 of 8 specs pass, nothing skipped, plus the new assertion that the deployed
Deployment reports `strategy.type: Recreate`. The sync loop, drift repair, deletion,
TargetConflict and redaction specs must be untouched — this feature changes no operator
behaviour in the shipped configuration (FR-006).

## 5. Manual: watch a real upgrade (SC-001, SC-003)

The one check no automated test concludes. On a kind cluster or a real one:

```bash
# install, then watch pods in a second terminal for the whole rollout
kubectl -n kvsynk8s get pods -w

# in the first terminal, trigger a rollout
kubectl -n kvsynk8s set image deployment/kvsynk8s-operator manager=<some other tag>
# or: kubectl -n kvsynk8s rollout restart deployment/kvsynk8s-operator
```

**Expected**: the watch shows the existing pod reach `Terminating` and disappear **before**
any new pod appears. At no point are two pods simultaneously `Running`. Time the gap between
the old pod disappearing and the new one reporting `Ready` — under 60 seconds when the image
is already present on the node.

**Before this feature**, the same watch shows the new pod appear and become `Running` while
the old one is still `Running`. That overlap is the defect.

## 6. The documentation answers the question (SC-004)

Read the new README section without reading any source. It must answer, plainly:

- what happens to applications while the operator is down (nothing — existing Secrets keep
  their values and stay mounted; the operator is not on any request path);
- what an upgrade costs now (a short window with no operator, deliberately chosen over a
  short window with two);
- what happens to a Key Vault change during that window (delayed, not lost).
