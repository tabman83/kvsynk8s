# Contract: operator Deployment rollout behaviour

**Feature**: [../spec.md](../spec.md) | **Satisfies**: FR-001, FR-002, FR-003 | **Date**: 2026-08-30

This contract extends the rendered-resource equivalence contract in
`specs/002-helm-chart/contracts/rendered-resources.md`, which
`hack/compare-helm-kustomize.sh` names as its definition of "equivalent". Read the two
together: that file defines the fields the two install paths must agree on, this one adds
`spec.strategy` to them and states what its value must be.

## Required render

Both install paths MUST render the operator Deployment with exactly:

```yaml
spec:
  replicas: 1
  strategy:
    type: Recreate
```

Specifically:

- `spec.strategy.type` MUST be the string `Recreate`.
- `spec.strategy.rollingUpdate` MUST be absent. Kubernetes rejects it alongside `Recreate`,
  and its presence would mean somebody changed the type without finishing the edit.
- `spec.replicas` MUST remain `1` and MUST remain absent from the chart's values, unchanged
  from today.

"Both install paths" means the default `helm template` render of `charts/kvsynk8s` **and**
`kustomize build config/default`, i.e. the manifest that becomes the release's
`install.yaml`.

## Value independence

The rollout behaviour is not configurable (FR-003). The rendered `spec.strategy` MUST be
identical for every combination of chart values, including the non-default set in
`charts/kvsynk8s/ci/nondefault-values.yaml` and each toggle
(`metrics.enabled`, `crds.install`, `crds.keep`, `networkPolicy.enabled`,
`serviceMonitor.enabled`). No value may make it vary, and none may remove it.

## How the contract is enforced

Two checks, because they fail on different things and neither covers the other:

| Check | Asserts | Fails when |
|---|---|---|
| `hack/check-render.sh` | The operator Deployment in a given render declares `strategy.type: Recreate` and no `rollingUpdate` | The chart stops rendering the strategy, under any value combination it is run with |
| `hack/compare-helm-kustomize.sh` | `.spec.strategy` is equal between the chart render and the kustomize output | The two install paths drift — one is updated and the other is not |

The equivalence check alone would pass if **both** paths dropped the field, since they would
still agree. The render check is what makes the value positive rather than merely consistent.

Additionally, the e2e suite asserts the property on the object as deployed in a live cluster:
the operator Deployment in the test namespace reports `strategy.type: Recreate`. This proves
the shipped manifest carries it end to end. It deliberately does **not** try to observe a
rollout and count running pods — see research [R3](../research.md) for why that would be a
flaky test with an inconclusive pass.

## Behaviour this guarantees, and what it costs

**Guaranteed**: during any upgrade of the operator workload, the running instance is fully
terminated before its replacement is created. Two operator instances never run at the same
time, so there is never a moment when two controllers reconcile the same `SecretSync`
objects or two listeners poll the same queue.

**Cost**: a window with no operator running, bounded below by
`terminationGracePeriodSeconds: 10` plus the readiness probe's `initialDelaySeconds: 5`, and
required by SC-003 to stay under 60 seconds where the image is already present on the node.

**Why the cost is acceptable**: the operator is not on any request path. While it is down,
already-synced Kubernetes Secrets still exist with their current values and the workloads
mounting them are unaffected. A Key Vault change during the gap is delayed, not lost: an
unconsumed queue message waits for the new instance, and if the event is lost entirely the
periodic reconcile still converges. This must be stated in the README (FR-007, FR-008), not
left for a user to discover during their first upgrade.
