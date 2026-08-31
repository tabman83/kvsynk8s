# Tasks: Single-Replica Invariant Made Real

**Input**: Design documents from `/specs/003-single-replica-invariant/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/ (deployment-rollout.md, runnable-leadership.md), quickstart.md

**Tests**: Required, not optional. Constitution IV and FR-010 both demand a test that fails when the change is reverted, so every behaviour task below is preceded by the check that must fail first. Two of the three "tests" are shell assertions over rendered manifests rather than Go tests — that is what testing a manifest field looks like here, and it matches how 002 treated chart verification.

**Organization**: Tasks are grouped by user story so each story is independently implementable and testable.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (US1–US3)

## Path Conventions

Repository root. Chart at `charts/kvsynk8s/`, manifest install at `config/manager/`, scripts at `hack/`, Go at `internal/`, e2e at `test/e2e/`, per plan.md Project Structure.

---

## Phase 1: Setup (Baseline Capture)

**Purpose**: Record what the repository does today, so every "this test fails first" claim later is evidence rather than assertion.

- [X] T001 Confirm the toolchain the checks need: run `make kustomize` so `bin/kustomize` exists, and confirm `helm` >= 3.8 and `python3 -c 'import yaml'` both succeed
- [X] T002 Capture the baseline in `/tmp`: render `helm template kvsynk8s charts/kvsynk8s --namespace kvsynk8s` and `bin/kustomize build config/default`, and confirm **neither** operator Deployment has a `spec.strategy` key. This is the defect, and it is what the Phase 3 checks must start by catching

**Checkpoint**: The absence of `spec.strategy` in both install paths is confirmed by hand, not assumed.

---

## Phase 2: Foundational (Make the Local Target Cover the Checks)

**Purpose**: US1 puts one of its two enforcement points in `hack/check-render.sh`, which no `make` target runs today — only the `helm.yml` workflow does. Without this, a developer's `make helm-verify` would pass on a change CI then rejects. BLOCKS US1's verification tasks only; US2 and US3 do not depend on it.

- [X] T003 Add `hack/check-render.sh` to the `helm-verify` recipe in `Makefile`: render the chart with default values and with `charts/kvsynk8s/ci/nondefault-values.yaml` to a temp file each, and run the script over both. Keep the existing `helm lint`, `hack/check-values.sh` and `hack/compare-helm-kustomize.sh` steps unchanged (research.md R2)
- [X] T004 Run `make helm-verify` and confirm it passes unchanged against the current tree — this task adds coverage, it must not add a failure

**Checkpoint**: `make helm-verify` now runs the same chart checks CI does, and is still green.

---

## Phase 3: User Story 1 - An upgrade never runs two operators at once (Priority: P1) 🎯 MVP

**Goal**: Both install paths render `spec.strategy.type: Recreate`, pinned by two checks that fail for different reasons, so the documented single-instance invariant becomes a property of the shipped manifests.

**Independent Test**: Render both install paths and confirm each declares `Recreate`; delete the field from one path and confirm the equivalence check fails; delete it from both and confirm the render check still fails. Ship this story alone and the defect is fixed.

### Tests for User Story 1 (write first, confirm they fail) ⚠️

- [X] T005 [US1] Add the positive assertion to `hack/check-render.sh`: in any render given to it, the operator Deployment (`kind: Deployment`, name ending `-operator`) MUST have `spec.strategy.type == "Recreate"` and MUST NOT have `spec.strategy.rollingUpdate`. Follow the file's existing style — a numbered check in the header comment plus a block in the Python body appending to `failures`. **Confirm it FAILS** against the baseline render from T002
- [X] T006 [US1] Add `check("Deployment .spec.strategy", h.get("strategy"), k.get("strategy"))` to the Deployment field comparison in `hack/compare-helm-kustomize.sh`, next to the existing `.spec.replicas` check. **Confirm it PASSES** against the current tree — both paths omit the field, so they agree. Record that in the task: this is exactly why T005 exists and why the equivalence check alone cannot enforce the contract (research.md R2)

### Implementation for User Story 1

- [X] T007 [US1] Add `strategy: {type: Recreate}` to `spec` in `charts/kvsynk8s/templates/deployment.yaml`, directly under the existing `replicas: 1`. Replace the "Not a value, on purpose" comment above `replicas` so it states the mechanism — one instance is now enforced by the strategy, not requested of the reader
- [X] T008 [US1] Run `hack/compare-helm-kustomize.sh` and confirm it now **FAILS**, naming `.spec.strategy`. This is the drift case T006 was added to catch, and confirming it here is what makes T006 a real test rather than a passing no-op
- [X] T009 [US1] Add the matching `strategy: {type: Recreate}` to `spec` in `config/manager/manager.yaml`, and rewrite the "No --leader-elect" comment block so it explains that a single running instance is now guaranteed by the strategy, and that the flag remains accepted and ignored
- [X] T010 [US1] Run `make helm-verify` and confirm every check is green again, including the render check from T005 and the equivalence check from T006
- [X] T011 [US1] Add an assertion to the scaffold specs in `test/e2e/e2e_test.go` that the deployed operator Deployment reports `strategy.type: Recreate` (`kubectl get deployment ... -o jsonpath={.spec.strategy.type}`), placed alongside the existing "controller-manager pod is running" spec. Do **not** add a spec that watches a rollout and counts pods — research.md R3 explains why that test would be flaky with an inconclusive pass

**Checkpoint**: Both install paths guarantee one running instance, two independent checks fail if that regresses, and a live cluster proves the shipped manifest carries it. The defect is fixed; US2 and US3 add safety and honesty on top.

---

## Phase 4: User Story 2 - A queue listener never runs where nothing consumes its events (Priority: P2)

**Goal**: The queue listener requires leadership, so a future leader-election mode cannot silently discard rotation events from a standby.

**Independent Test**: Start a manager with leader election enabled against a Lease already held by someone else, register the listener with a fake queue, and confirm it never polls. Independent of US1 — it touches no manifest.

### Tests for User Story 2 (write first, confirm they fail) ⚠️

- [X] T012 [US2] Create `internal/controller/listener_leadership_test.go`: build a manager on the suite's `cfg` with `LeaderElection: true`, a unique `LeaderElectionID`, an explicit `LeaderElectionNamespace`, `Metrics: {BindAddress: "0"}` and `HealthProbeBindAddress: "0"`; pre-create the `coordination.k8s.io/v1` Lease that ID maps to, held by a foreign `holderIdentity` with a long `leaseDurationSeconds` and a fresh `renewTime`; register a real `events.Listener` (test-only import) backed by a fake `azure.QueueSource` modelled on `fakeQueueSource` in `internal/events/listener_test.go`; start the manager, wait for cache sync, and assert the fake's receive count stays zero. Add a file-header comment explaining why the test lives in this package and not `internal/events` (research.md R4: keeping `internal/events` free of any envtest dependency, as CLAUDE.md documents). **Confirm it FAILS** on the current `NeedLeaderElection() == false`

### Implementation for User Story 2

- [X] T013 [US2] Change `NeedLeaderElection()` to return `true` in `internal/events/listener.go`
- [X] T014 [US2] Rewrite the comment above `NeedLeaderElection` in `internal/events/listener.go` to state the safety consequence rather than current irrelevance (FR-011): a listener on an instance without the controller blocks on a full events channel, stalls its sequential poll loop mid-batch, and the held messages are redelivered on Azure's 30s default visibility timeout until `DequeueCount` passes the poison threshold and they are deleted unprocessed — silent event loss with `kvsynk8s_queue_consecutive_receive_failures` reading zero throughout. Cite `contracts/runnable-leadership.md`
- [X] T015 [US2] Run `make test` (T012 now passes) and `go test ./internal/events/...` with no `KUBEBUILDER_ASSETS` set, confirming `internal/events` still needs no envtest assets — the property CLAUDE.md documents must survive this feature

**Checkpoint**: The listener requires leadership, proven by a test that fails when reverted, and no package gained an envtest dependency.

---

## Phase 5: User Story 3 - An operator can find out what happens when kvsynk8s is down (Priority: P3)

**Goal**: The documentation states the operator's real failure behaviour and the upgrade trade, and every existing statement this feature makes inaccurate is corrected.

**Independent Test**: Hand the README to someone who has not read the code; they can say what happens to their applications while the operator is down, and how long an upgrade leaves them without one.

### Implementation for User Story 3

- [X] T016 [US3] Add a new top-level section to `README.md` between `## Operator configuration` and `## Troubleshooting` covering FR-007 and FR-008: the operator is not on any request path; while it is down, already-synced Secrets keep their values and the pods mounting them are unaffected; a Key Vault change during that window is delayed, not lost (an unconsumed queue message waits for the new instance, and the periodic reconcile converges even if the event is lost); upgrades now stop the old instance before starting the new one, so there is a short gap with no operator, deliberately chosen over a short overlap with two. Plain English per the project's writing rules, no install command (so `hack/check-doc-versions.sh` is unaffected)
- [X] T017 [US3] Update the "There is no `replicas` value" paragraph in `README.md` (around line 77) so it says the single instance is enforced by the Deployment's `Recreate` strategy, and link to the new section from T016
- [X] T018 [US3] Update the `--leader-elect` row in the operator-configuration table in `README.md` (around line 345): the flag is still accepted and still ignored, and single-instance operation is now a property of the manifest rather than a convention
- [X] T019 [P] [US3] Update the "Deliberately not configurable" block at the end of `charts/kvsynk8s/values.yaml`: `replicas` stays fixed at 1, and the rollout strategy is fixed at `Recreate` and is likewise not a value (FR-003)
- [X] T020 [P] [US3] Add a pointer line to `specs/002-helm-chart/contracts/rendered-resources.md` naming `specs/003-single-replica-invariant/contracts/deployment-rollout.md` as the source for the rollout field, since `hack/compare-helm-kustomize.sh` cites that 002 contract as its definition of equivalence (research.md R7). Leave the rest of the 002 artifacts alone — they are the historical record of what was decided then

**Checkpoint**: Nothing in the repository still claims the invariant is a convention, and a reader can answer the availability question from the README alone.

---

## Phase 6: Polish & Cross-Cutting Concerns

- [X] T021 Add one line to the architecture section of `CLAUDE.md` recording that both install paths pin `strategy: Recreate` and that the queue listener requires leadership, so the next agent does not re-derive it. Keep it to the facts; the constitution forbids documenting anything that does not exist
- [X] T022 Run the full local suite: `make lint`, `make test`, `make helm-verify`, `make test-e2e`. All green, e2e still 8 of 8 plus the new assertion, nothing skipped
- [X] T023 Walk `quickstart.md` steps 1–4 and 6 end to end, including step 2's deliberate revert (delete the strategy from one path, then from both) to confirm each check fails for the reason it is meant to. Restore the tree afterwards with `git checkout -- charts config`
- [ ] T024 Open the PR from `003-single-replica-invariant`. Description in plain English per the project's writing rules; it MUST state explicitly that the comment rewrites in T014, T007 and T009 are documentation with no logic and are therefore exempt from Constitution IV, and that no RBAC changed (`config/rbac/role.yaml` must not be in the diff — Constitution V). Then follow the standing PR CI babysitting loop in `specs/001-akv-eventgrid-sync/tasks.md`: watch `gh pr checks <PR#> --watch`, fix forward on any failure, never weaken a check to get green, and never merge — Nino reviews and merges
- [ ] T025 Manual validation of SC-001 and SC-003, the one thing no automated test concludes: follow `quickstart.md` step 5 on a kind or real cluster, watching pods across a rollout, and record in the PR that the old pod terminated before the new one appeared and how long the gap was. Human-run, like T038 in 001 — do not fake it with a scripted poll

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: no dependencies. T001 → T002
- **Foundational (Phase 2)**: needs T001. T003 → T004. Blocks only US1's verification tasks (T010); US2 and US3 do not touch it
- **US1 (Phase 3)**: needs Phase 1 for the baseline and Phase 2 for T010. Independent of US2 and US3
- **US2 (Phase 4)**: needs nothing from US1 or Phase 2 — it touches only Go files and can start immediately after Phase 1
- **US3 (Phase 5)**: T016–T019 describe behaviour US1 introduces, so they land after T007/T009 are written; T020 is independent of everything
- **Polish (Phase 6)**: T021–T023 need all desired stories; T024 needs a pushed branch; T025 needs US1 merged into the branch and a cluster

### Within Each User Story

Test first and failing → implementation → confirm green. In US1 specifically the order matters more than usual: T005 must fail before T007, and T008 must fail after T007 but before T009. Skipping either confirmation turns a real test into one that has never been observed to fail.

### Parallel Opportunities

- US2 (T012–T015) runs fully parallel to US1 (T005–T011) — different files, no shared state. This is the largest parallel win in the feature
- T019 ∥ T020 — different files, and independent of the README edits. T016 → T017 → T018 are
  sequential because all three edit `README.md`, and only the first of them adds a section the
  other two link to
- T005 ∥ T006 — different scripts, and their confirmations are independent
- Nothing in Phase 6 is parallel; it is a sequence of gates

---

## Parallel Example: US1 and US2 together

```bash
# Two independent tracks after Phase 1:
Track A (US1): hack/check-render.sh, hack/compare-helm-kustomize.sh,
               charts/kvsynk8s/templates/deployment.yaml,
               config/manager/manager.yaml, test/e2e/e2e_test.go
Track B (US2): internal/controller/listener_leadership_test.go,
               internal/events/listener.go
```

---

## Implementation Strategy

### MVP (User Story 1 only)

1. Phase 1 (T001–T002), Phase 2 (T003–T004)
2. Phase 3 (T005–T011)
3. **STOP and VALIDATE**: quickstart steps 1, 2 and 5. The defect is fixed and the fix is pinned

US1 alone is a complete, shippable change. US2 fixes a latent trap that harms nobody today, and US3 explains the trade — both are worth having, neither blocks the fix.

### Incremental Delivery

1. Setup + Foundational → local checks match CI
2. US1 → the invariant is real → could ship here
3. US2 → the leadership declaration is correct → could ship here
4. US3 → the documentation matches the shipped behaviour
5. Polish → one PR, green, reviewed by Nino

### Scope discipline

Three things are explicitly **not** in any task above, and adding them means reopening the spec:

- Enabling leader election, or adding `coordination.k8s.io` lease RBAC. The spec records why in its rejected-alternative section
- A `replicas` value, a strategy value, or any new flag (FR-003)
- A PodDisruptionBudget, anti-affinity, or topology spread constraints

---

## Notes

- [P] tasks = different files, no dependencies
- Every behaviour change here has a paired confirmation that it fails first. If a check cannot be made to fail, it is not a test and the task is not done
- `config/rbac/role.yaml` must not appear in this feature's diff at any point
- Commit per task or logical group; never commit to `master`; never merge the PR yourself
