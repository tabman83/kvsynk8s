# Implementation Plan: Single-Replica Invariant Made Real

**Branch**: `003-single-replica-invariant` | **Date**: 2026-08-30 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/003-single-replica-invariant/spec.md`

## Summary

The project documents an "exactly one operator instance" invariant in three places and enforces it in none. Neither install path states a Deployment update strategy, so Kubernetes' default rolling update starts the replacement pod before terminating the old one — at one replica, `maxSurge` 25% rounds up to 1 and `maxUnavailable` 25% rounds down to 0. Every upgrade briefly runs two uncoordinated managers and two queue listeners.

The fix is to state `strategy: Recreate` in both install paths, pin it with two checks that fail if it is removed or if the paths drift, correct the queue listener's leadership declaration (currently `false`, which would put a listener on replicas where nothing drains its events channel), and document the operator's real failure behaviour so the resulting upgrade gap reads as the deliberate trade it is.

No Go behaviour changes in the shipped configuration. No new values, no new flags, no API change, no RBAC change.

## Technical Context

**Language/Version**: Go 1.26.0

**Primary Dependencies**: sigs.k8s.io/controller-runtime v0.24.1, k8s.io/api v0.36.0, Ginkgo v2.27.4; Helm >= 3.8 (CI matrix v3.8.2 / v3.21.4 / v4.2.4); kustomize; python3 + PyYAML for the chart checks

**Storage**: N/A — no persistence introduced

**Testing**: `go test` (unit), envtest-backed suite in `internal/controller`, `hack/check-render.sh` and `hack/compare-helm-kustomize.sh` for the manifests, kind-based e2e in `test/e2e`

**Target Platform**: Kubernetes, operator Deployment in namespace `kvsynk8s`

**Project Type**: Single Go module, kubebuilder operator, plus a Helm chart and a kustomize manifest install

**Performance Goals**: Unchanged. The only new timing property is the upgrade gap: under 60s on a cluster where the image is already present (SC-003), bounded below by the existing `terminationGracePeriodSeconds: 10` plus the readiness probe's `initialDelaySeconds: 5`

**Constraints**: No new chart values, no new flags, no new environment variables (Constitution III). The existing `--leader-elect` flag stays accepted and ignored. Every behaviour change needs a test that fails without it (Constitution IV). The Helm/kustomize equivalence check must still pass

**Scale/Scope**: Two manifest files, one Go declaration, one comment, README and values documentation, three test additions. No change to reconcile behaviour, metrics, or the SecretSync API

## Constitution Check

*GATE: evaluated before Phase 0, re-evaluated after Phase 1. Both passes below.*

| Principle | Assessment | Verdict |
|---|---|---|
| I. Secrets Are Never Exposed | No code path in this feature reads, writes, logs, or serialises a secret value. `internal/sync/writer.go` is untouched. The listener change removes a scenario in which secret *staleness* goes unnoticed; it adds no place a value could leak. The new README prose describes availability, never values. | PASS |
| II. Reliability of Sync | The upgrade gap means a window with no operator. Convergence is still guaranteed: a queue message is not consumed while nothing runs, so it waits and is picked up by the new instance; if it is lost anyway, the periodic reconcile (default 4h) still converges. Sync is delayed, never abandoned — the same guarantee the project already documents for an unconfigured queue. The listener fix strictly improves this principle: it closes a path where events would be silently discarded as poison with no failure signal. | PASS |
| III. Simplicity First | Nothing is added. No value, no flag, no environment variable, no new resource kind. One field is set on two existing manifests, one boolean is corrected, and three documents are brought into agreement with the shipped behaviour. The multi-replica alternative is recorded as rejected in the spec with the evidence, so it is not re-proposed speculatively. | PASS |
| IV. Tested Changes | Three behaviour changes, three tests that fail when reverted: the rendered strategy (chart render check), the two install paths agreeing (equivalence check), and the listener not starting without leadership (envtest). The comment rewrite is documentation with no logic and is exempt; the PR must say so explicitly, as the principle requires. | PASS |
| V. Least-Privilege Access | No RBAC change. This feature deliberately does **not** add the `coordination.k8s.io` lease permissions that a real leader-election mode would need, so the operator's granted permissions are byte-for-byte unchanged. `config/rbac/role.yaml` must not appear in the diff. | PASS |

**Post-Phase-1 re-evaluation**: no gate moved. The Phase 1 design added two contract documents and a validation guide; it introduced no new dependency, no new permission, and no new configuration surface. Constitution IV was the only gate at risk, because FR-004's test needs a configuration production never runs (leader election enabled) — resolved in research R4 without adding production code.

## Project Structure

### Documentation (this feature)

```text
specs/003-single-replica-invariant/
├── plan.md              # This file
├── spec.md              # Feature specification
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 output
├── contracts/
│   ├── deployment-rollout.md    # what both install paths must render, and what pins it
│   └── runnable-leadership.md   # which manager runnables may run without leadership
├── checklists/
│   └── requirements.md  # spec quality checklist (already passing)
└── tasks.md             # Created by /speckit-tasks, not by this command
```

### Source Code (repository root)

Only these paths change. Everything else in the repository is out of scope for this feature.

```text
charts/kvsynk8s/
├── templates/deployment.yaml     # add spec.strategy; rewrite the replicas comment
└── values.yaml                   # rewrite the "Deliberately not configurable" note

config/manager/
└── manager.yaml                  # add spec.strategy; rewrite the leader-election comment

internal/events/
└── listener.go                   # NeedLeaderElection() false -> true, comment rewritten

internal/controller/
└── listener_leadership_test.go   # NEW: envtest proof the listener waits for leadership

hack/
├── check-render.sh               # assert the operator Deployment renders strategy Recreate
└── compare-helm-kustomize.sh     # add .spec.strategy to the compared Deployment fields

Makefile                          # add check-render.sh to helm-verify (see research R2) so
                                  # the local target covers the same checks CI does

test/e2e/
└── e2e_test.go                   # assert the deployed Deployment carries strategy Recreate

README.md                         # new failure-behaviour section; fix the replicas and
                                  # --leader-elect statements

specs/002-helm-chart/contracts/rendered-resources.md
                                  # pointer to this feature's rollout contract, since
                                  # compare-helm-kustomize.sh cites that file as its definition
```

**Structure Decision**: No structural change. This is a defect fix inside the existing kubebuilder layout; the only new file is one test. The new test lives in `internal/controller` rather than `internal/events` on purpose — see research R4 — so that `internal/events` keeps the property CLAUDE.md documents, that plain `go test` is enough for it with no envtest assets.

## Complexity Tracking

No Constitution Check violations. Table omitted.
