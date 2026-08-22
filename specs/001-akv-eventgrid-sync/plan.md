# Implementation Plan: Event-Driven Azure Key Vault to Kubernetes Secret Sync

**Branch**: `001-akv-eventgrid-sync` | **Date**: 2026-08-21 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/001-akv-eventgrid-sync/spec.md`

## Summary

A Kubernetes operator, written in Go on controller-runtime, that syncs Azure Key Vault secrets to native Kubernetes Secrets. Operators declare syncs with a namespaced custom resource (`SecretSync`). The primary update path is event-driven: Key Vault publishes `SecretNewVersionCreated` events through Event Grid to an Azure Storage Queue; the operator pulls that queue (outbound-only) and re-fetches the secret value from Key Vault, updating the managed Kubernetes Secret within the 60-second target. A periodic full reconciliation (default 1 hour) is the safety net for missed events, vault-side deletions, and in-cluster drift. All Azure access uses Microsoft Entra Workload ID; no inbound endpoint, no static credentials.

## Technical Context

**Language/Version**: Go (current stable, 1.25+)

**Primary Dependencies**:
- controller-runtime + kubebuilder scaffolding: CRD generation, reconcile loop with workqueue/backoff, finalizers, status updates, owned-resource watches
- client-go (transitive) for Secret read/write
- Azure SDK for Go: `azsecrets` (secret fetch), `azqueue` v2 (notification queue pull), `azsystemevents` (typed `KeyVaultSecretNewVersionCreatedEventData` deserialization), `azidentity` ≥ 1.3 (`DefaultAzureCredential` → workload identity)

**Storage**: none of its own — all state lives in the Kubernetes API (CR status subresource + managed Secrets); the Azure Storage Queue is transport, not storage

**Testing**: three layers — (1) standard `go test` with controller-runtime's envtest (real API server for reconciler tests) and fake Azure clients behind small interfaces (sync logic, event parsing, retry/backoff, redaction); (2) integration tests (build tag `integration`) via testcontainers-go against Azurite (real azqueue protocol) and Lowkey Vault (Key Vault emulator); (3) e2e on a kind cluster exercising the full loop (deploy → sync → queue event → update <60 s → drift repair) with no Azure subscription. All three run in CI on every PR

**Target Platform**: Linux container on Kubernetes (AKS is the primary target because workload identity and Event Grid are Azure-native; any cluster with outbound Azure access and federated credentials works)

**Project Type**: single service (cluster operator/controller), one Go module scaffolded with kubebuilder

**Performance Goals**: change propagation < 60 s (SC-001); 100-secret rotation burst within one minute fully propagated (SC-005); queue poll cadence 1–30 s adaptive

**Constraints**: outbound-only networking; no secret values in any log/status/metric output (constitution I); short-lived platform credentials only; one failing secret never blocks others

**Scale/Scope**: hundreds of `SecretSync` resources per cluster, single operator replica (leader election deferred until a real HA need appears)

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | Gate | Status |
|-----------|------|--------|
| I. Secrets are never exposed | Design has a single code path that serializes a value (the Secret writer); logs/status/events carry vault+name+version only; tests plant sentinel values and scan all output (SC-004) | PASS |
| II. Reliability of sync | Event path + 1 h reconciliation guarantees convergence; per-secret retry with exponential backoff; per-declaration status (InSync/Pending/Failing) on the CR status subresource | PASS |
| III. Simplicity first | One CRD, one controller, one project; Storage Queue (not Service Bus); no webhook server, no leader election, no Helm chart in v1 (plain manifests); no admission webhooks | PASS |
| IV. Tested changes | Sync engine, event parsing, backoff, ownership rules, and redaction covered by unit tests; reconciler behavior covered by envtest against a real API server — all failing without the change | PASS |
| V. Least-privilege access | Azure: "Key Vault Secrets User" on the vault + "Storage Queue Data Message Processor" on the queue only. K8s: RBAC for `secretsyncs` (all verbs + status) and `secrets` (get/list/watch/create/update/delete) only, plus events | PASS |

Post-design re-check (after Phase 1): no new violations introduced; the design added no extra projects, CRDs, or dependencies beyond those listed above. PASS.

## Project Structure

### Documentation (this feature)

```text
specs/001-akv-eventgrid-sync/
├── plan.md              # This file
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 output
├── contracts/
│   ├── secretsync-crd.yaml       # CRD contract (operator-facing API)
│   └── queue-message.md          # Event Grid → queue message contract
└── tasks.md             # Phase 2 output (/speckit-tasks — not created here)
```

### Source Code (repository root)

```text
cmd/
└── main.go                          # manager setup, controller + queue listener wiring

api/
└── v1alpha1/
    ├── secretsync_types.go          # CRD types (spec + status); CRD YAML generated from these
    └── zz_generated.deepcopy.go     # generated

internal/
├── controller/
│   ├── secretsync_controller.go     # reconcile loop, finalizer, periodic requeue (1 h default)
│   └── secretsync_controller_test.go  # envtest: reconcile, finalizer, ownership rules
├── events/
│   ├── listener.go                  # goroutine: pull queue (adaptive delay), feed reconciler via source.Channel
│   ├── parser.go                    # queue message → (vault, secretName) or discard
│   └── parser_test.go
├── sync/
│   ├── engine.go                    # fetch from KV, write Secret, set status; idempotent
│   ├── writer.go                    # the ONLY code path that touches secret values
│   └── engine_test.go               # idempotency, latest-wins, redaction sentinels
└── azure/
    ├── keyvault.go                  # azsecrets wrapper behind SecretReader interface
    └── queue.go                     # azqueue wrapper behind QueueSource interface

config/                              # kubebuilder-generated manifests (kustomize)
├── crd/                             # generated CRD
├── rbac/                            # ServiceAccount + Role/ClusterRole + bindings
├── manager/                         # Deployment (workload identity label/annotations)
└── default/                         # kustomize entry point (namespace/name overrides); install via `kubectl apply -k config/default`

test/
└── e2e/
    └── e2e_test.go                  # kind-based e2e: full loop against Azurite + KV emulator

.github/workflows/ci.yml             # PR gate: lint, unit+envtest, integration (Docker), e2e (kind)
.github/workflows/release.yml        # on v* tag: test, push multi-arch image to GHCR, attach install.yaml to a GitHub Release

Makefile                             # kubebuilder targets: manifests, generate, test, test-integration, test-e2e, docker-build
```

**Structure Decision**: standard kubebuilder layout (single Go module), matching "Project Type: single service" and what every comparable operator uses — contributors and reviewers already know where things live. Packages separate the event path (`internal/events`), the sync core (`internal/sync`), and the only value-carrying file (`internal/sync/writer.go`) so constitution principle I is auditable in one place. Deployment manifests are the kubebuilder-generated kustomize tree under `config/` (no Helm in v1; simplicity first).

## Complexity Tracking

No constitution violations to justify — table intentionally empty.
