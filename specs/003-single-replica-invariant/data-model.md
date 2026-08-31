# Phase 1 Data Model: Single-Replica Invariant Made Real

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Date**: 2026-08-30

## No entities, no schema change

This feature introduces no entity, changes no schema, and stores nothing. Stating that
plainly is the useful content here, because it settles two questions that would otherwise
come up during planning and again at release time:

- **The `SecretSync` API is untouched.** `api/v1alpha1/secretsync_types.go` does not appear
  in this feature's diff. No field is added, removed, renamed, or given a tighter validation
  rule. Under the project's release rules a published-API change is what forces a major bump;
  since there is none, this feature imposes no version floor of its own.
- **No object's stored state changes.** Existing `SecretSync` objects, their `status`
  subresources, and the Kubernetes Secrets the operator manages are all bit-for-bit
  unaffected by installing this change (SC-007). An upgrade carrying it is a workload
  replacement and nothing else.

## The invariants this feature makes real

In place of entities, the feature's substance is three invariants that were previously
asserted in prose and are now enforced. They are the things a reviewer should check are
still true after any future change.

| # | Invariant | Where it is enforced | Where it was only claimed before |
|---|---|---|---|
| INV-1 | At most one operator instance runs at any moment, including during an upgrade | `spec.strategy.type: Recreate` in both install paths | `README.md`, `charts/kvsynk8s/values.yaml`, and the comments in both Deployment manifests |
| INV-2 | The two install paths declare the same rollout behaviour | `hack/compare-helm-kustomize.sh` field comparison | Nothing — the field was outside the compared set |
| INV-3 | The queue listener runs only where the reconcile loop that consumes its events also runs | `Listener.NeedLeaderElection()` returning `true`, pinned by an envtest spec | Nothing — the declaration asserted the opposite |

INV-1 and INV-2 are complementary and neither is sufficient alone: INV-2 only detects the
two paths disagreeing, so two manifests that both omit the strategy would satisfy it. See
research [R2](./research.md).

## Objects the feature touches

Not entities of this project's own, but the Kubernetes objects whose shape changes:

- **Operator `Deployment`** (`kvsynk8s-operator` in namespace `kvsynk8s`) — gains
  `spec.strategy`. Every other field is unchanged, including `replicas: 1`, which stays
  hardcoded and stays absent from the chart's values.
- **`Lease`** (`coordination.k8s.io/v1`) — appears only inside the new test, pre-created and
  held by a foreign identity so the test manager stays a leadership candidate. Nothing in the
  shipped operator reads or writes a Lease, and no permission to do so is granted. See
  [contracts/runnable-leadership.md](./contracts/runnable-leadership.md).
