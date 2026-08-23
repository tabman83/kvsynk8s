# Implementation Plan: Helm Chart Install Method

**Branch**: `002-helm-chart` | **Date**: 2026-08-23 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/002-helm-chart/spec.md`

## Summary

Add a hand-maintained Helm chart at `charts/kvsynk8s/` as a second first-class
install method. The chart mirrors the current kustomize output (same names,
namespace convention, RBAC, pod security), renders the SecretSync CRD as an
upgradeable template gated by `crds.install` and protected by
`helm.sh/resource-policy: keep` behind `crds.keep`, and exposes only the
operator's real configuration surface (queue URL, reconcile interval, Azure
workload identity client ID, image, scheduling, metrics/ServiceMonitor/
NetworkPolicy toggles). A `make helm-sync` target copies generated CRD and
RBAC content into the chart; CI fails on drift and additionally lints and
renders the chart on every PR. The release pipeline packages the chart with
version/appVersion taken from the git tag, pushes it to
`oci://ghcr.io/tabman83/charts/kvsynk8s`, and attaches the `.tgz` to the
GitHub Release next to `install.yaml`.

## Technical Context

**Language/Version**: Helm chart (Go-template YAML), Helm >= 3.8 (OCI GA); Bash for sync/compare scripts; GNU Make; GitHub Actions. No Go code changes to the operator.

**Primary Dependencies**: `helm` CLI (pinned in CI via `azure/setup-helm` or the runner's preinstalled helm — decision in research.md R8), existing toolchain: `controller-gen` v0.21.0, `kustomize` v5.8.1 (both already vendored under `bin/` by the Makefile).

**Storage**: N/A (chart is static packaging; state lives in the cluster as today).

**Testing**: `helm lint`, `helm template` with default and representative non-default values, drift check (`make helm-sync` + `git diff --exit-code`), rendered-output equivalence script against `kustomize build config/default` (SC-002). Installing the chart into the kind e2e cluster is a stretch goal, not planned here.

**Target Platform**: Kubernetes clusters (same as the operator: AKS primarily, kind for tests); chart consumers run Helm >= 3.8 on any OS.

**Project Type**: Kubernetes operator packaging (in-repo Helm chart + Makefile target + CI/release wiring + docs).

**Performance Goals**: N/A — packaging only. Release pipeline overhead for chart publish should stay under ~1 minute.

**Constraints**: Default render must be name-for-name equivalent to `install.yaml` minus the Namespace object (FR-004); no secret values anywhere in chart source or rendered output (FR-013, Constitution I); chart version always equals the release tag (no independent versioning); chart is reviewed source — no generator runs in CI or release (only the sync check).

**Scale/Scope**: One chart, ~12 rendered resources, ~15 documented values, 1 new Makefile target, 1 new PR workflow (or job), 1 modified release workflow, 1 README section.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | Assessment | Status |
|---|---|---|
| I. Secrets Are Never Exposed (NON-NEGOTIABLE) | The chart carries configuration only: queue URL, interval, client ID, image ref, toggles — none of these are secret values. No Secret objects are templated; the only secret-writing path stays `internal/sync/writer.go`, untouched. FR-013/SC-005 make this an explicit acceptance criterion. | PASS |
| II. Reliability of Sync | No sync-logic change. The chart must not weaken reliability defaults: replicas fixed at 1 (no value), same probes, same 4h reconcile default when values are omitted. | PASS |
| III. Simplicity First | Values expose only the operator's actual configuration surface (FR-007..FR-011); no `replicas`, no speculative knobs, no umbrella charts, no independent chart versioning, no adoption tooling (docs only), no chart repository infrastructure (OCI only). `values.schema.json` deliberately skipped for now (research.md R7). | PASS |
| IV. Tested Changes | Chart is declarative YAML, so "tests" are: lint, default+non-default renders, drift check, and the equivalence comparison against kustomize output — each fails CI without the corresponding change being correct. No Go behavior changes, so no new Go tests; the PR description must state this exemption per the constitution. | PASS (with stated exemption) |
| V. Least-Privilege Access | RBAC in the chart is synced verbatim from the generated `config/rbac/role.yaml` — it cannot drift wider than what the operator declares (FR-015/SC-006). The release workflow reuses the existing `packages: write` permission for GHCR. | PASS |

**Post-design re-check (after Phase 1)**: unchanged — the design adds no new
resource kinds beyond what kustomize already produces, no new credentials, and
no configuration options beyond the spec's list. No Complexity Tracking
entries needed.

## Project Structure

### Documentation (this feature)

```text
specs/002-helm-chart/
├── plan.md              # This file (/speckit-plan command output)
├── research.md          # Phase 0 output (/speckit-plan command)
├── data-model.md        # Phase 1 output (/speckit-plan command)
├── quickstart.md        # Phase 1 output (/speckit-plan command)
├── contracts/           # Phase 1 output (/speckit-plan command)
│   ├── values.md              # user-facing values contract
│   ├── rendered-resources.md  # default-render equivalence contract (SC-002)
│   └── release-artifacts.md   # per-tag publish contract (FR-014)
└── tasks.md             # Phase 2 output (/speckit-tasks command - NOT created by /speckit-plan)
```

### Source Code (repository root)

```text
charts/kvsynk8s/                     # NEW — the chart, reviewed source (FR-003)
├── Chart.yaml                       # apiVersion v2; version/appVersion are dev
│                                    # placeholders (0.0.0), overridden at package time
├── values.yaml                      # documented values incl. federated-credential warning
├── .helmignore
└── templates/
    ├── _helpers.tpl                 # name prefix, labels, serviceaccount-name helpers
    ├── NOTES.txt                    # post-install pointers (CRD, WI setup)
    ├── crds/
    │   └── secretsync-crd.yaml      # SYNCED from config/crd/bases/, gated by
    │                                # .Values.crds.install, keep-annotation by .Values.crds.keep
    ├── rbac/
    │   ├── serviceaccount.yaml      # WI annotation when azure.clientID set
    │   ├── manager-clusterrole.yaml # rules SYNCED from config/rbac/role.yaml
    │   ├── manager-clusterrolebinding.yaml
    │   ├── metrics-auth-clusterrole.yaml         # gated by metrics.enabled
    │   ├── metrics-auth-clusterrolebinding.yaml  # gated by metrics.enabled
    │   ├── metrics-reader-clusterrole.yaml       # gated by metrics.enabled
    │   ├── secretsync-admin-clusterrole.yaml
    │   ├── secretsync-editor-clusterrole.yaml
    │   └── secretsync-viewer-clusterrole.yaml
    ├── deployment.yaml              # 1 replica hardcoded; args from values
    ├── metrics-service.yaml         # gated by metrics.enabled
    ├── servicemonitor.yaml          # gated by serviceMonitor.enabled
    └── networkpolicy.yaml           # gated by networkPolicy.enabled

hack/
├── helm-sync.sh                     # NEW — regenerates the two SYNCED regions above
└── compare-helm-kustomize.sh        # NEW — SC-002 equivalence check

Makefile                             # MODIFIED — add helm-sync (and helm-verify) targets
.github/workflows/
├── helm.yml                         # NEW — PR CI: lint, template, drift, equivalence
└── release.yml                      # MODIFIED — package + push OCI + attach .tgz
README.md                            # MODIFIED — Helm install section + migration caveats
```

**Structure Decision**: single in-repo chart under `charts/kvsynk8s/`
(FR-003), scripts under the existing `hack/` directory, CI as a new dedicated
workflow file mirroring the repo's one-workflow-per-concern layout
(`lint.yml`, `test.yml`, `test-integration.yml`, `test-e2e.yml`).

## Complexity Tracking

No constitution violations to justify — table intentionally empty.
