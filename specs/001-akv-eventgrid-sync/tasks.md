# Tasks: Event-Driven Azure Key Vault to Kubernetes Secret Sync

**Input**: Design documents from `/specs/001-akv-eventgrid-sync/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md

**Tests**: Included — the constitution (Principle IV) requires tests that fail without the change for all sync, retry/backoff, and secret-writing logic.

**Organization**: Tasks are grouped by user story so each story is an independently testable increment.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies on incomplete tasks)
- **[Story]**: US1 (initial sync), US2 (near-realtime), US3 (recovery), US4 (observability)

## Path Conventions

Single Go module at repository root, standard kubebuilder layout per plan.md: `cmd/`, `api/v1alpha1/`, `internal/`, `config/`.

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Scaffold the kubebuilder project and pin dependencies

- [X] T001 Scaffold the operator with kubebuilder: `kubebuilder init --domain io --repo github.com/tabman83/kvsynk8s` producing cmd/main.go, Makefile, Dockerfile, config/ tree, go.mod (Go 1.25+, controller-runtime pinned). Domain is `io` so that group `kvsynk8s` yields API group `kvsynk8s.io` (kubebuilder composes `<group>.<domain>`)
- [X] T002 Scaffold the API with `kubebuilder create api --group kvsynk8s --version v1alpha1 --kind SecretSync` (namespaced, with controller), producing api/v1alpha1/secretsync_types.go and internal/controller/secretsync_controller.go stubs; verify the resulting API group is exactly `kvsynk8s.io` per contracts/secretsync-crd.yaml
- [X] T003 [P] Add pinned Azure SDK for Go dependencies to go.mod: azidentity (≥1.14), azsecrets, azqueue v2, azsystemevents
- [X] T004 [P] Configure golangci-lint (.golangci.yml) and extend .gitignore to cover kubeconfig/.env/local settings files (constitution: Security Requirements)

**Checkpoint**: `make build` and `make test` pass on the empty scaffold

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: The CRD contract, RBAC, Azure access layer, and operator configuration that every story builds on

**⚠️ CRITICAL**: No user story work can begin until this phase is complete

- [X] T005 Implement SecretSync spec/status types in api/v1alpha1/secretsync_types.go per data-model.md (spec.vault.name/secret with validation markers, spec.target.secretName/dataKey defaults, status.state/lastSyncTime/syncedVersion/reason/message/observedGeneration, printer columns, status subresource); run `make manifests generate` and verify the generated CRD in config/crd/ matches contracts/secretsync-crd.yaml
- [X] T006 Restrict config/rbac/role.yaml to least privilege (constitution V): secretsyncs + status + finalizers all verbs; secrets get/list/watch/create/update/delete; events create/patch — nothing else
- [X] T007 [P] Implement the SecretReader interface and azsecrets-backed implementation in internal/azure/keyvault.go: `GetLatest(ctx, vaultName, secretName) (value, version, err)` using DefaultAzureCredential, distinguishing not-found / access-denied / disabled / transient errors; value never appears in error strings or logs
- [X] T008 [P] Wire operator configuration in cmd/main.go: flags/env for queue URL, reconcile interval (default 1h), client ID; manager setup with health/ready probes; no leader election (single replica, plan.md)

**Checkpoint**: CRD installs (`make install`), operator starts against a cluster and does nothing — user story implementation can now begin

---

## Phase 3: User Story 1 — Make a Key Vault secret available in the cluster (Priority: P1) 🎯 MVP

**Goal**: Declaring a SecretSync produces a Kubernetes Secret with the vault value; deleting the declaration removes the Secret; failures are reported in status without leaking values.

**Independent Test**: quickstart.md V1 + V7 — create a SecretSync against a real vault secret, see the Secret appear with the right value; delete the SecretSync, see the Secret go away. No Event Grid/queue needed.

### Tests for User Story 1 (write first, must fail before implementation)

- [X] T009 [P] [US1] Sync engine unit tests in internal/sync/engine_test.go with a fake SecretReader: creates Secret with labels/annotations/ownerReference per data-model.md; skips write when annotated version matches (idempotency); refuses pre-existing unmanaged Secret → TargetConflict (FR-012); reader not-found → Failing/SecretNotFound with no value anywhere in status
- [X] T010 [P] [US1] Controller envtest in internal/controller/secretsync_controller_test.go: reconcile creates the Secret and sets state InSync/observedGeneration; CR deletion runs the finalizer and deletes the managed Secret; two SecretSyncs targeting the same Secret → later one Failing/TargetConflict

### Implementation for User Story 1

- [X] T011 [P] [US1] Implement the secret writer in internal/sync/writer.go — the ONLY code path handling values (constitution I): create/update the Opaque Secret with managed-by label, vault/secret/version annotations, ownerReference; refuse writes to unmanaged existing Secrets
- [X] T012 [US1] Implement the sync engine in internal/sync/engine.go: resolve target name/dataKey defaults, fetch latest via SecretReader, call writer, compute status (InSync/Pending/Failing + reason/message per data-model.md state transitions); idempotent, latest-wins (FR-005)
- [X] T013 [US1] Implement reconcile + finalizer in internal/controller/secretsync_controller.go: builder watches SecretSync and Owns(corev1.Secret); reconcile calls the engine, updates status subresource, requeues with backoff on failure (FR-008); finalizer deletes the managed Secret on CR deletion (FR-002)
- [X] T014 [US1] Add workload-identity deployment config in config/manager/manager.yaml: `azure.workload.identity/use` pod label, ServiceAccount annotation placeholder for client ID, resource requests/limits, non-root securityContext; set namespace to `kvsynk8s` and deployment name to `kvsynk8s-operator` in config/default kustomization (replacing kubebuilder's `-system`/`controller-manager` defaults) to match quickstart.md

**Checkpoint**: MVP — quickstart V1/V5/V7 pass on a real cluster; value propagation works without any queue

---

## Phase 4: User Story 2 — Secret changes propagate in near realtime (Priority: P2)

**Goal**: A new secret version in Key Vault reaches the cluster in under 60 seconds via Event Grid → Storage Queue, with duplicate/out-of-order/burst-safe processing.

**Independent Test**: quickstart.md V2 — with US1 synced, rotate the vault secret and watch the Kubernetes Secret flip in <60 s without waiting for reconciliation.

### Tests for User Story 2 (write first, must fail before implementation)

- [X] T015 [P] [US2] Parser unit tests in internal/events/parser_test.go using the example payload from contracts/queue-message.md: valid SecretNewVersionCreated → (vault, secretName); SecretNearExpiry/SecretExpired/unknown types → discard; ObjectType != secret → discard; malformed body → discard error without echoing body content
- [X] T016 [P] [US2] Listener unit tests in internal/events/listener_test.go with a fake QueueSource: matched events emit reconcile requests for all matching SecretSyncs (case-insensitive vault match); unmatched events deleted silently (FR-006); DequeueCount > 5 → poison delete; batch of 32 processed without loss; burst case — 100+ events spread across multiple fake receive batches all produce reconcile requests with none lost (SC-005)

### Implementation for User Story 2

- [X] T017 [P] [US2] Implement the QueueSource interface and azqueue-backed implementation in internal/azure/queue.go: batch receive (32), delete, DequeueCount exposure, DefaultAzureCredential
- [X] T018 [US2] Implement event parsing in internal/events/parser.go per contracts/queue-message.md rules 1–2 (Base64 decode, azsystemevents deserialization, type/objectType filtering)
- [X] T019 [US2] Implement the queue listener in internal/events/listener.go: adaptive poll loop (1–2 s busy → 30 s idle, research R6), map events to SecretSync objects via the manager's cached client, inject reconcile requests through a source.Channel, delete messages per contracts rules 5–6
- [X] T020 [US2] Wire the listener into the manager in cmd/main.go and internal/controller/secretsync_controller.go: WatchesRawSource(source.Channel) on the controller builder, listener started as a manager Runnable

**Checkpoint**: quickstart V2 passes — rotation lands in <60 s; US1 still fully functional with the queue unconfigured

---

## Phase 5: User Story 3 — Recover from missed notifications (Priority: P3)

**Goal**: The cluster never drifts permanently: periodic reconciliation (default 1 h), startup convergence, in-cluster drift repair, and vault-side deletion handling.

**Independent Test**: quickstart.md V3 + V4 — stop the operator, rotate + clear the queue, restart: value converges. Delete the managed Secret in-cluster: it comes back.

### Tests for User Story 3 (write first, must fail before implementation)

- [X] T021 [P] [US3] Reconciliation tests in internal/controller/secretsync_controller_test.go (extend): successful reconcile returns RequeueAfter == configured interval; deleting the managed Secret triggers re-creation via the Owns watch; fake reader flips value while no event arrives → next reconcile converges; retry/backoff — fake reader fails with a transient error N times then succeeds → reconcile is retried via the rate-limited queue and the CR reaches InSync without manual intervention (constitution IV retry/backoff test mandate; US2 acceptance scenario 4, FR-008)
- [X] T022 [P] [US3] Source-deleted tests in internal/sync/engine_test.go (extend): reader reports deleted/disabled → status Failing with SourceDeleted/SourceDisabled AND the existing Secret is left untouched (FR-013)

### Implementation for User Story 3

- [X] T023 [US3] Add periodic reconciliation in internal/controller/secretsync_controller.go: RequeueAfter with the configured interval (default 1 h) on every successful reconcile; startup convergence needs no extra code (informer sync enqueues all CRs) — assert it in the envtest from T021
- [X] T024 [US3] Implement keep-last-known-good in internal/sync/engine.go: on SourceDeleted/SourceDisabled set Failing status but skip writer deletion/overwrite (FR-013, spec edge case)

**Checkpoint**: quickstart V3/V4 pass; convergence guaranteed with the event path fully broken (SC-003)

---

## Phase 6: User Story 4 — Operator visibility and safe operations (Priority: P4)

**Goal**: Per-declaration state is visible at a glance, failures are isolated and explained, and no output channel ever carries a secret value.

**Independent Test**: quickstart.md V5 + V6 — break one sync, see Failing with reason while others stay InSync; grep all logs/status for planted values and find nothing.

### Tests for User Story 4 (write first, must fail before implementation)

- [X] T025 [P] [US4] Redaction test suite in internal/sync/redaction_test.go: plant `SENTINEL-...` values via the fake reader, capture controller/engine log output and all status/event fields across success, failure, conflict, and poison paths; assert zero occurrences (SC-004)
- [X] T026 [P] [US4] Failure isolation envtest in internal/controller/secretsync_controller_test.go (extend): one Failing SecretSync (reader error) while a second syncs normally — second reaches InSync on schedule (FR-008, SC-006)

### Implementation for User Story 4

- [X] T027 [US4] Emit Kubernetes Events on the SecretSync via an EventRecorder in internal/controller/secretsync_controller.go (Synced, SyncFailed with reason; names/versions only) and ensure status.message templates in internal/sync/engine.go reference vault/name/version, never values (FR-009, FR-010)
- [X] T028 [US4] Add structured logging conventions in internal/sync/writer.go and internal/events/listener.go: log keys limited to vault, secret, version, namespace, name, eventID, dequeueCount; add a lint-style unit check in internal/sync/redaction_test.go that the writer package never logs the value parameter

**Checkpoint**: All four stories independently functional; SC-004/SC-006 verified

---

## Phase 7: Integration & End-to-End Tests (cross-cutting)

**Purpose**: Automated verification of the real wire protocols (queue, Key Vault) and the full in-cluster loop without needing an Azure subscription. Tooling: kind (Kubernetes in Docker — kubebuilder's native e2e target), testcontainers-go with Azurite (official Azure Storage emulator, real azqueue protocol) and Lowkey Vault (community Key Vault emulator).

**Depends on**: US1 + US2 complete (US3 recommended for the drift scenario)

- [ ] T029 [P] Queue integration tests in internal/azure/queue_integration_test.go (build tag `integration`): testcontainers-go starts Azurite; exercise the azqueue-backed QueueSource against it — batch receive of 32, delete, DequeueCount visibility-timeout behavior, and a Base64-encoded Event Grid payload roundtrip per contracts/queue-message.md
- [ ] T030 [P] Key Vault reader integration tests in internal/azure/keyvault_integration_test.go (build tag `integration`): testcontainers-go starts Lowkey Vault; exercise the azsecrets-backed SecretReader — GetLatest happy path, new-version-wins, not-found, disabled-secret error mapping. If Lowkey Vault's challenge-auth emulation proves incompatible with the Go SDK, fall back to a local HTTPS stub implementing the two needed Key Vault REST responses, and record the decision in research.md R10
- [ ] T031 Add Makefile targets and tooling: `test-integration` (runs `go test -tags integration ./internal/azure/...`, requires Docker), `test-e2e` (creates/uses a kind cluster, builds and loads the operator image), pinned tool versions for kind/envtest/golangci-lint in the Makefile
- [ ] T032 E2E suite in test/e2e/e2e_test.go against kind: deploy CRD + operator via config/default with env pointed at Azurite (and Lowkey Vault or the stub) running on the kind network; assert the full loop — SecretSync created → Secret appears with correct value; SecretNewVersionCreated message injected into the queue → Secret updates within 60 s (SC-001); managed Secret deleted in-cluster → re-created (US3 drift); SecretSync deleted → Secret removed; operator logs captured and scanned for planted sentinel values (SC-004)
- [ ] T033 CI workflow in .github/workflows/ci.yml: on every PR run lint, `make test` (unit + envtest), `make test-integration` (Docker services), and `make test-e2e` (kind) — constitution IV: the full suite must pass before a PR is reviewable; pin action and tool versions

**Checkpoint**: the whole sync loop is provable on any laptop/CI runner with Docker — no Azure account needed

---

## Phase 8: Polish & Cross-Cutting Concerns

**Purpose**: Ship-readiness: docs, image, release pipeline, and the constitution's CLAUDE.md obligation

- [ ] T034 [P] Harden the Dockerfile: distroless static base, non-root, multi-arch (amd64/arm64) via `make docker-buildx`
- [ ] T035 [P] Write user documentation in README.md: what it is and how it differs from akv2k8s (event-driven, queue pull); install section (release manifest + image); full Azure setup walkthrough condensed from specs/001-akv-eventgrid-sync/quickstart.md §1 (queue, Event Grid subscription, workload identity, role assignments); SecretSync reference — every spec field with defaults, a complete example, and the status state/reason table from data-model.md; operator configuration reference (queue URL, reconcile interval, client ID); troubleshooting section keyed on status reasons (SecretNotFound, AccessDenied, TargetConflict, SourceDeleted/Disabled, TransientError) plus "events not arriving" (check queue/subscription; reconciliation still converges)
- [ ] T036 Release pipeline in .github/workflows/release.yml: on pushing a `v*` tag — run the full test suite, build and push the multi-arch image to ghcr.io/tabman83/kvsynk8s tagged `vX.Y.Z` and `latest`, render a single install manifest (`kustomize build config/default` with the image pinned to the tag) as install.yaml, and create a GitHub Release with install.yaml attached and generated notes; pin all action versions (constitution: pinned dependencies)
- [ ] T037 Update CLAUDE.md with real build/lint/test commands (make build/test/lint/test-integration/test-e2e, single-test invocation), the actual architecture, and the "PR CI babysitting" rule from Notes below (constitution: Development Workflow — required now that code exists)
- [ ] T038 Run the full quickstart.md validation V1–V7 against a real AKS cluster + vault + queue and record results (including the <60 s measurement for SC-001 and the 100-secret burst for SC-005) in specs/001-akv-eventgrid-sync/validation-results.md — the one check the emulators cannot give: real Event Grid delivery and workload identity

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: no dependencies. T001 → T002 → {T003, T004 in parallel}
- **Foundational (Phase 2)**: needs Phase 1. T005 → T006; T007 and T008 parallel to both
- **US1 (Phase 3)**: needs Phase 2 — BLOCKS everything below (all stories flow through the engine/controller it creates)
- **US2 (Phase 4)**: needs US1 (engine + controller exist). Independent of US3/US4
- **US3 (Phase 5)**: needs US1. Independent of US2 (reconciliation works with no queue at all)
- **US4 (Phase 6)**: needs US1; T025 exercises US2/US3 paths too, so run last among stories for full coverage
- **Integration/E2E (Phase 7)**: T029/T030 need only Phase 2 (they test internal/azure); T032 needs US1+US2 (US3 for the drift assertion); T033 last, once the targets it runs exist
- **Polish (Phase 8)**: needs all desired stories; T036 (release) needs T034 (image) + T033 (CI green); T038 needs US1–US3 minimum

### Within Each User Story

Tests first and failing → implementation → checkpoint. Engine/writer before controller wiring; parser/queue before listener wiring.

### Parallel Opportunities

- Phase 1: T003 ∥ T004 (after T002)
- Phase 2: T007 ∥ T008 ∥ (T005→T006)
- US1: T009 ∥ T010, then T011 ∥ (nothing) — T012 waits on T011, T013 on T012; T014 anytime
- US2: T015 ∥ T016, then T017 ∥ T018, then T019 → T020
- US3 and US2 can be built in parallel by different people once US1 is done
- US4: T025 ∥ T026; T027 ∥ T028
- Phase 7: T029 ∥ T030 (and both can start right after Phase 2, in parallel with the story phases); T031 → T032 → T033
- Phase 8: T034 ∥ T035; T036 after T034; T037 after code stabilizes; T038 last

## Parallel Example: User Story 2

```bash
# After US1 checkpoint, launch test authoring together:
Task: "Parser unit tests in internal/events/parser_test.go"      # T015
Task: "Listener unit tests in internal/events/listener_test.go"  # T016
# Then implementation in parallel:
Task: "QueueSource + azqueue impl in internal/azure/queue.go"    # T017
Task: "Event parsing in internal/events/parser.go"               # T018
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Phases 1–2 (scaffold, CRD, RBAC, Key Vault reader, config)
2. Phase 3 (US1) → **STOP and VALIDATE** with quickstart V1/V5/V7 on a real cluster
3. This alone is a useful tool (declarative vault→cluster bridge, akv2k8s-style), before any Event Grid wiring exists

### Incremental Delivery

Each phase ends at a demoable checkpoint; ship order US1 → US2 (the headline feature) → US3 (production trust) → US4 (operability) → integration/e2e (CI-provable loop) → polish. T029/T030 can land earlier, alongside the stories, since they only depend on Phase 2. Per the constitution, every phase lands as one or more focused PRs off `master`, reviewed by Nino — the checkpoints above are natural PR boundaries.

---

## Notes

- [P] = different files, no dependency on an incomplete task
- Constitution I applies to every task: values only ever flow through internal/sync/writer.go
- Verify each story's tests fail before implementing; run `make test` before declaring any task done
- Total: 38 tasks (Setup 4, Foundational 4, US1 6, US2 6, US3 4, US4 4, Integration/E2E 5, Polish 5)

### PR CI babysitting (standing instruction for the implementing agent)

Once T033 exists, every PR follows this loop — it is part of finishing a task, not optional:

1. After pushing a branch and opening the PR, watch the checks: `gh pr checks <PR#> --watch` (or `gh run watch <run-id>`).
2. On any failure, fetch the failing job logs with `gh run view <run-id> --log-failed`, diagnose, fix on the same branch, push, and watch again. Repeat until all checks are green.
3. Never work around a failure by weakening a test, skipping it, or loosening lint rules — fix the cause. If a failure is genuinely unrelated flake, say so in a PR comment instead of retrying silently.
4. A task is done only when its PR is green. Never merge — Nino reviews and merges every PR (constitution: Development Workflow).
