# Tasks: Helm Chart Install Method

**Input**: Design documents from `/specs/002-helm-chart/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/ (values.md, rendered-resources.md, release-artifacts.md), quickstart.md

**Tests**: The spec defines chart "tests" as lint, renders, drift check, and the equivalence comparison (Constitution IV assessment in plan.md). These appear below as verification tasks inside each story — there are no Go test tasks because no Go code changes.

**Organization**: Tasks are grouped by user story so each story is independently implementable and testable.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (US1–US5)

## Path Conventions

Repository root; chart at `charts/kvsynk8s/`, scripts at `hack/`, workflows at `.github/workflows/` per plan.md Project Structure.

---

## Phase 1: Setup (Chart Skeleton)

**Purpose**: Minimal chart scaffold every later task builds on.

- [X] T001 Create chart skeleton: `charts/kvsynk8s/Chart.yaml` (apiVersion `v2`, name `kvsynk8s`, `version: 0.0.0` and `appVersion: 0.0.0` as dev placeholders per research.md R4, description, home/sources) and `charts/kvsynk8s/.helmignore`
- [X] T002 [P] Create `charts/kvsynk8s/templates/_helpers.tpl` with helpers per research.md R10: name prefix equal to the release name (DNS-truncated), common labels (`app.kubernetes.io/name: kvsynk8s`, `control-plane: controller-manager` for selector compatibility), and the ServiceAccount-name helper (`serviceAccount.name` override, else `<prefix>-controller-manager`)

---

## Phase 2: Foundational (helm-sync Machinery)

**Purpose**: The sync tooling that produces the CRD template and RBAC rules the chart cannot ship without. BLOCKS all user stories: US1 needs the synced CRD and ClusterRole content; US5 CI enforces this same tooling.

- [X] T003 Create `charts/kvsynk8s/templates/rbac/manager-clusterrole.yaml` wrapper: ClusterRole `<prefix>-manager-role` metadata plus empty `# BEGIN generated rules (make helm-sync)` / `# END generated rules` marker region (research.md R5)
- [X] T004 Create `hack/helm-sync.sh` (executable) implementing research.md R5: (1) wrap `config/crd/bases/kvsynk8s.io_secretsyncs.yaml` verbatim into `charts/kvsynk8s/templates/crds/secretsync-crd.yaml` with the `{{- if .Values.crds.install }}` gate and a `crds.keep`-gated `helm.sh/resource-policy: keep` annotation injected into `metadata.annotations`; (2) splice the `rules:` block of `config/rbac/role.yaml` between the markers in `charts/kvsynk8s/templates/rbac/manager-clusterrole.yaml`. Output must be byte-stable across runs
- [X] T005 Add `helm-sync` target to `Makefile` (depends on the existing `manifests` target, invokes `hack/helm-sync.sh`)
- [X] T006 Run `make helm-sync`; commit the generated `charts/kvsynk8s/templates/crds/secretsync-crd.yaml` and populated rules region; re-run and verify `git diff --exit-code charts/` is clean (byte-stability)

**Checkpoint**: CRD template and manager ClusterRole rules exist in the chart and regenerate deterministically.

---

## Phase 3: User Story 1 - Install the operator with Helm in one command (Priority: P1) 🎯 MVP

**Goal**: `helm install kvsynk8s oci://ghcr.io/tabman83/charts/kvsynk8s --namespace kvsynk8s --create-namespace` (or from the local chart dir) produces a Ready operator whose default render is equivalent to `install.yaml` minus the Namespace object.

**Independent Test**: `helm lint` passes; `helm template kvsynk8s charts/kvsynk8s --namespace kvsynk8s` with all defaults renders exactly the 12-resource inventory in contracts/rendered-resources.md; `hack/compare-helm-kustomize.sh` exits 0; installing into a kind cluster brings `deploy/kvsynk8s-operator` Ready with the CRD available.

### Implementation for User Story 1

- [X] T007 [US1] Write `charts/kvsynk8s/values.yaml` with the complete documented surface from contracts/values.md: `operator.queueURL`/`operator.reconcileInterval` (both `""`), `azure.clientID` (`""`), `image.{repository: ghcr.io/tabman83/kvsynk8s, tag: "", pullPolicy: IfNotPresent}`, `serviceAccount.name` (`""`), `resources` (requests 10m/32Mi, limits 200m/128Mi), `nodeSelector`/`tolerations`/`affinity`, `metrics.enabled: true`, `serviceMonitor.enabled: false`, `networkPolicy.enabled: false`, `crds.{install: true, keep: true}` — each with an inline comment stating default and effect, including the FR-012 federated-credential warning next to `serviceAccount.name`
- [X] T008 [P] [US1] Create `charts/kvsynk8s/templates/rbac/serviceaccount.yaml`: name from the helper, `azure.workload.identity/client-id` annotation rendered only when `azure.clientID` is set
- [X] T009 [P] [US1] Create `charts/kvsynk8s/templates/rbac/manager-clusterrolebinding.yaml` binding `<prefix>-manager-role` to the ServiceAccount (same role/SA pair as install.yaml)
- [X] T010 [P] [US1] Create the metrics RBAC templates, each wrapped in `{{- if .Values.metrics.enabled }}`: `charts/kvsynk8s/templates/rbac/metrics-auth-clusterrole.yaml`, `charts/kvsynk8s/templates/rbac/metrics-auth-clusterrolebinding.yaml`, `charts/kvsynk8s/templates/rbac/metrics-reader-clusterrole.yaml` (content mirrored from `config/rbac/`)
- [X] T011 [P] [US1] Create the aggregation ClusterRoles mirrored from `config/rbac/`: `charts/kvsynk8s/templates/rbac/secretsync-admin-clusterrole.yaml`, `charts/kvsynk8s/templates/rbac/secretsync-editor-clusterrole.yaml`, `charts/kvsynk8s/templates/rbac/secretsync-viewer-clusterrole.yaml`
- [X] T012 [P] [US1] Create `charts/kvsynk8s/templates/deployment.yaml` per contracts/rendered-resources.md: name `<prefix>-operator`, `replicas: 1` hardcoded with a comment explaining no leader election (FR-009), pod securityContext (`runAsNonRoot`, `seccompProfile: RuntimeDefault`), container securityContext (`readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`, drop ALL), probes (liveness `/healthz:8081` 15s/20s, readiness `/readyz:8081` 5s/10s), `terminationGracePeriodSeconds: 10`, container port 8081 (name `health`), pod annotation `kubectl.kubernetes.io/default-container: manager`, args: `--leader-elect` (kept only for install.yaml equivalence — the operator hardcodes `LeaderElection: false` in `cmd/main.go` and ignores the flag; add a template comment saying so, do not drop it), `--health-probe-bind-address=:8081`, `--metrics-bind-address=:8443` gated by `metrics.enabled`, plus `--queue-url`/`--reconcile-interval`/`--azure-client-id` each rendered only when its value is set; image `repository:tag|.Chart.AppVersion` with `pullPolicy`; `resources`/`nodeSelector`/`tolerations`/`affinity` from values; pod label `azure.workload.identity/use: "true"` only when `azure.clientID` is set
- [X] T013 [P] [US1] Create `charts/kvsynk8s/templates/metrics-service.yaml`: Service `<prefix>-controller-manager-metrics-service` (port 8443, selector `control-plane: controller-manager`), gated by `metrics.enabled`
- [X] T014 [P] [US1] Create `charts/kvsynk8s/templates/NOTES.txt` with post-install pointers: CRD presence check, workload-identity/federated-credential setup reminder, link to README
- [X] T015 [US1] Verify quickstart.md §1: `helm lint charts/kvsynk8s` passes with 0 failures; `helm template kvsynk8s charts/kvsynk8s --namespace kvsynk8s` renders exactly the 12-resource inventory of contracts/rendered-resources.md (no Namespace, no ServiceMonitor, no NetworkPolicy, no WI label/annotation); fix templates until true
- [X] T016 [US1] Create `hack/compare-helm-kustomize.sh` (executable) per research.md R8: render `helm template kvsynk8s charts/kvsynk8s --namespace kvsynk8s` and `bin/kustomize build config/default`, compare the sorted kind/namespace/name inventory (excluding the Namespace object) and assert the Deployment's security contexts, probes, args, resources, serviceAccountName, replicas, container ports, and the `kubectl.kubernetes.io/default-container` pod annotation match; print a diff on failure
- [X] T017 [US1] Run `hack/compare-helm-kustomize.sh` and fix chart output until it exits 0 (SC-002, FR-004)

**Checkpoint**: Default chart render is equivalent to install.yaml; chart installs standalone (kind smoke via quickstart §6 first half is possible now).

---

## Phase 4: User Story 2 - Configure the installation through values (Priority: P2)

**Goal**: Every documented value lands where contracts/values.md says; optional resources appear/disappear with their toggles; workload identity is a first-class value.

**Independent Test**: quickstart.md §3 — render with non-default values and check each landing spot; render with `metrics.enabled=false` and confirm the metrics Service, arg, and RBAC disappear.

### Implementation for User Story 2

- [X] T018 [P] [US2] Create `charts/kvsynk8s/templates/servicemonitor.yaml`: gated by `serviceMonitor.enabled`; calls `fail` with a clear message when `serviceMonitor.enabled=true` but `metrics.enabled=false` (contracts/values.md); targets the metrics Service like the kustomize `config/prometheus` overlay
- [X] T019 [P] [US2] Create `charts/kvsynk8s/templates/networkpolicy.yaml`: gated by `networkPolicy.enabled`; allows metrics scraping only from namespaces labeled `metrics: enabled`, selector matching the kustomize `config/network-policy` overlay
- [X] T020 [US2] Verify quickstart.md §3: render with `operator.queueURL`, `operator.reconcileInterval=30m`, `azure.clientID`, `serviceMonitor.enabled=true`, `networkPolicy.enabled=true`, `image.tag=test` set — confirm args, the three WI markers rendering together (FR-011), ServiceMonitor and NetworkPolicy presence, image tag; then render with `metrics.enabled=false` and confirm no metrics Service, no `--metrics-bind-address` arg, no metrics-auth/reader RBAC
- [X] T021 [US2] Verify the no-secret contract (SC-005, FR-013) per quickstart.md §4: default and all-values renders contain no `kind: Secret`; `grep -ri "SET-ME" charts/` is empty; no secret-looking material in chart source

**Checkpoint**: Full values surface works; both stories independently verifiable.

---

## Phase 5: User Story 3 - Every release ships the chart automatically (Priority: P2)

**Goal**: A `v*` tag publishes the chart to `oci://ghcr.io/tabman83/charts/kvsynk8s` and attaches `kvsynk8s-X.Y.Z.tgz` to the GitHub Release next to `install.yaml`, versions equal to the tag minus `v`.

**Independent Test**: Push a test tag after merge; `helm install ... --version X.Y.Z --dry-run` resolves the OCI chart with matching appVersion; `gh release view` lists both assets (quickstart.md §7).

### Implementation for User Story 3

- [X] T022 [US3] Modify `.github/workflows/release.yml`: add a chart-publish job with `needs: build-and-push` per research.md R3/R9 — `VERSION="${GITHUB_REF_NAME#v}"`, `helm package charts/kvsynk8s --version "$VERSION" --app-version "$VERSION"`, `helm registry login ghcr.io` with `GITHUB_TOKEN` (`packages: write`), `helm push` to `oci://ghcr.io/tabman83/charts`, upload the `.tgz` as a workflow artifact
- [X] T023 [US3] Modify the GitHub Release job in `.github/workflows/release.yml` to `needs:` the chart-publish job, download the `.tgz` artifact, and add `kvsynk8s-*.tgz` to the `softprops/action-gh-release` `files:` list next to `dist/install.yaml`; confirm a re-run on the same tag republishes idempotently (contracts/release-artifacts.md)
- [ ] T024 [US3] Manual post-merge validation (maintainer, after first tag): run quickstart.md §7 — OCI pull resolves with app version `X.Y.Z`, release assets list both `install.yaml` and `kvsynk8s-X.Y.Z.tgz` (SC-003, FR-014). **Also check the GHCR package visibility, which the spec did not anticipate**: a new package scoped to a personal account can land private, and `helm push` sends no `org.opencontainers.image.source` label linking it to this repo, so it may not inherit the repo's visibility. If the anonymous install fails with `unauthorized`, set `ghcr.io/tabman83/charts/kvsynk8s` to Public once in its package settings. Nothing in the pipeline can do this. **Still open** — it needs a real `v*` tag pushed after merge, so it cannot be done on the branch. What was verified locally instead: `helm package --version 1.4.2 --app-version 1.4.2` produces `kvsynk8s-1.4.2.tgz` with `version == appVersion == 1.4.2`, `ci/` excluded by `.helmignore`, and a default render from the package resolving the image to `ghcr.io/tabman83/kvsynk8s:1.4.2` with no values set.

**Checkpoint**: Release pipeline ships both install methods with matching versions, zero manual steps.

---

## Phase 6: User Story 4 - Safe lifecycle: upgrades update the CRD, uninstall preserves data (Priority: P3)

**Goal**: The CRD is an upgradeable template; default uninstall leaves the CRD and every SecretSync object; `crds.install=false` and `crds.keep=false` behave per data-model.md's state table.

**Independent Test**: quickstart.md §6 — install on kind, create SecretSyncs, uninstall, verify CRD and objects survive, adopt and reinstall with zero loss (SC-004).

### Implementation for User Story 4

- [X] T025 [US4] Verify CRD gating renders per data-model.md (CRD lifecycle settings): default render includes the CRD with `helm.sh/resource-policy: keep`; `--set crds.keep=false` renders it without the annotation; `--set crds.install=false` omits the CRD entirely while everything else still renders — adjust `hack/helm-sync.sh` wrapper output in `charts/kvsynk8s/templates/crds/secretsync-crd.yaml` if any case fails (FR-005, FR-006)
- [X] T026 [US4] Manual kind lifecycle validation per quickstart.md §6: install → apply `config/samples/` → `helm uninstall` → CRD and SecretSync objects still present → adopt CRD via the research.md R2 label/annotate commands → reinstall → operator resumes managing objects; record zero objects lost (SC-004)

**Checkpoint**: Lifecycle semantics proven end-to-end on a real cluster.

---

## Phase 7: User Story 5 - The chart cannot silently drift from the operator (Priority: P3)

**Goal**: One command re-syncs generated CRD/RBAC into the chart; PR CI fails on drift, lint errors, or invalid renders.

**Independent Test**: quickstart.md §5 — add an RBAC marker verb without syncing, confirm the drift check fails; run `make helm-sync`, confirm it passes.

### Implementation for User Story 5

- [X] T027 [P] [US5] Create `charts/kvsynk8s/ci/nondefault-values.yaml`: queue URL, reconcile interval, `azure.clientID`, `serviceMonitor.enabled=true`, `networkPolicy.enabled=true`, image override — the representative non-default values file for the CI render matrix (research.md R8; `helm lint` picks up `ci/*-values.yaml` automatically)
- [X] T028 [US5] Create `.github/workflows/helm.yml` on `pull_request` and `push` to master per research.md R8: pin helm via `azure/setup-helm`; `helm lint charts/kvsynk8s`; `helm template` with defaults (must succeed, non-empty); `helm template` with `charts/kvsynk8s/ci/nondefault-values.yaml`; drift check `make manifests helm-sync && git diff --exit-code charts/`; equivalence check `hack/compare-helm-kustomize.sh`; no-secret grep over source and both renders (contracts/values.md CI tests)
- [X] T029 [US5] Verify the drift guardrail locally per quickstart.md §5: simulate drift by adding a verb to the kubebuilder RBAC marker in `internal/controller/secretsync_controller.go`, confirm `make manifests helm-sync` produces a `charts/` diff, restore, confirm clean (SC-006, FR-015)

**Checkpoint**: All five stories complete and independently verified.

---

## Phase 8: Polish & Cross-Cutting Concerns

**Purpose**: Documentation and final validation spanning all stories.

- [X] T030 [P] Add the Helm install section to `README.md` (FR-017): the one-command install (`--namespace kvsynk8s --create-namespace`), Helm >= 3.8 floor, values overview with the FR-012 federated-credential warning, migration caveat (Helm does not adopt install.yaml resources — remove the manifest install first), the lossless CRD adoption steps from research.md R2, the `crds.install=false` escape hatch, and the `crds.keep=false` uninstall-ordering caveat
- [X] T031 [P] Update `CLAUDE.md` (project) build/test section: add `make helm-sync` and the `helm.yml` workflow to the CI list, and mention `charts/kvsynk8s/` in the architecture notes
- [X] T032 Run the full `specs/002-helm-chart/quickstart.md` validation (§1–§6; §7 stays post-merge) and fix anything that fails

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: no dependencies
- **Foundational (Phase 2)**: needs T001 (chart dir) — T003 → T004 → T005 → T006 in order. BLOCKS all user stories (US1 renders the synced CRD/ClusterRole; US5 CI runs the same tooling)
- **US1 (Phase 3)**: needs Phase 2. T007 first (templates read values), then T008–T014 in parallel, then T015 → T016 → T017
- **US2 (Phase 4)**: needs T007 + T012 (deployment/values wiring exists); T018–T019 parallel, then T020 → T021
- **US3 (Phase 5)**: needs Phase 2 only (packages whatever the chart is); T022 → T023 → T024 (manual, post-merge)
- **US4 (Phase 6)**: needs Phase 2 (CRD template) and, for T026, US1 complete (installable chart)
- **US5 (Phase 7)**: needs Phase 2 (drift tooling) and T016 (equivalence script); T027 → T028 → T029
- **Polish (Phase 8)**: T030–T031 anytime after their subject matter settles; T032 last

### User Story Dependencies

- **US1 (P1)**: only Foundational — the MVP
- **US2 (P2)**: builds on US1's values.yaml and deployment template
- **US3 (P2)**: independent of US1/US2 content; ships whatever is in `charts/`
- **US4 (P3)**: render checks independent; lifecycle test needs US1
- **US5 (P3)**: needs Foundational tooling + US1's equivalence script

### Parallel Opportunities

- T002 alongside T001's Chart.yaml work
- T008–T014 (seven template files, all different files) after T007
- T018 and T019 in parallel
- T027 alongside T028 drafting; T030 and T031 in parallel
- After Foundational: US3 (release wiring) can proceed in parallel with US1 template work

---

## Parallel Example: User Story 1

```bash
# After T007 (values.yaml), launch all template files together:
Task: "Create charts/kvsynk8s/templates/rbac/serviceaccount.yaml"                 # T008
Task: "Create charts/kvsynk8s/templates/rbac/manager-clusterrolebinding.yaml"     # T009
Task: "Create metrics RBAC templates (3 files)"                                   # T010
Task: "Create secretsync admin/editor/viewer ClusterRoles (3 files)"              # T011
Task: "Create charts/kvsynk8s/templates/deployment.yaml"                          # T012
Task: "Create charts/kvsynk8s/templates/metrics-service.yaml"                     # T013
Task: "Create charts/kvsynk8s/templates/NOTES.txt"                                # T014
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Phase 1 (Setup) → Phase 2 (Foundational: helm-sync machinery)
2. Phase 3 (US1): full default-render chart, lint clean, equivalence script green
3. **STOP and VALIDATE**: quickstart §1–§2 pass; optional kind smoke install
4. This alone is a working, installable chart from the local directory

### Incremental Delivery

1. US1 → equivalent default install (MVP)
2. US2 → configurability proven with non-default renders
3. US3 → releases publish OCI + .tgz automatically
4. US4 → lifecycle semantics validated on kind
5. US5 → CI locks it all in against drift
6. Polish → README, CLAUDE.md, full quickstart run

Per the repo workflow, all of this lands as one focused PR on a `002-helm-chart` branch (never on master), with checks watched until green; T024 and the first real §7 validation happen after merge on the first tagged release.

---

## Deviations from the task text

Three things were done differently from what the task said, because the task
text turned out to be wrong:

- **T027** claims `helm lint` picks up `ci/*-values.yaml` automatically. It does
  not — that is `ct lint` (chart-testing), not `helm lint`. The file is still
  there and is still the CI render matrix input, but `helm.yml` and
  `make helm-verify` pass it explicitly with `-f`.
- **T021 / quickstart §4** used `grep -i "kind: Secret"`, which always matches
  `kind: SecretSync` inside the CRD schema and so can never pass. Replaced by
  `hack/check-render.sh`, which parses the render and checks each document's
  real `kind`. That script also grew two checks the tasks did not ask for but
  that caught a real bug: duplicate YAML mapping keys (the first version of
  `_helpers.tpl` emitted `app.kubernetes.io/name` twice on the Deployment,
  Service and ServiceMonitor) and `<SET-ME>`-style placeholders.
- **T026 / quickstart §6** applied `config/samples/`, but the scaffolded sample
  still has an empty `spec:` and the CRD rejects it. The scenario now creates
  two valid SecretSync objects inline. The same section claimed a reinstall
  after a `crds.keep` uninstall needs the research.md R2 adoption commands — it
  does not: the kept CRD still carries Helm's ownership metadata, so a
  same-name, same-namespace reinstall just works. The R2 commands are for a CRD
  Helm never owned (a `kubectl apply -f install.yaml` install). Both paths were
  run on a kind cluster; quickstart.md now documents both correctly.

## Notes

- [P] tasks = different files, no dependencies on incomplete tasks
- The chart is reviewed source: no generator output committed except the two helm-sync regions (research.md R5)
- No secret value may appear in any chart source or render at any task — T021 and the T028 CI grep enforce this (Constitution I)
- The PR description must state the "no Go tests because no Go behavior changed" exemption (plan.md Constitution Check IV)
