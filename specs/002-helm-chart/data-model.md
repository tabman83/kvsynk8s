# Data Model: Helm Chart Install Method

**Feature**: `002-helm-chart` | **Date**: 2026-08-23

This feature ships no runtime data structures; its "data model" is the chart's
configuration contract and the artifact set each release produces.

## Entity: Helm chart

The packaged install unit at `charts/kvsynk8s/`.

| Field | Value / rule |
|---|---|
| `name` | `kvsynk8s` (fixed) |
| `apiVersion` | `v2` |
| `version` | `0.0.0` placeholder in git; set to the release tag (minus `v`) at package time |
| `appVersion` | same rule as `version`; always equal to it |
| `kubeVersion` | not constrained (operator supports what controller-runtime v0.24 supports) |
| Helm floor | `>= 3.8` (OCI), documented, not enforceable in Chart.yaml |

**Relationships**: one-to-one with a git tag `vX.Y.Z`; carries the CRD and
RBAC content synced from `config/crd/bases/` and `config/rbac/role.yaml`
(source of truth: the Go kubebuilder markers).

**Validation rules**: `helm lint` clean; default render non-empty and
equivalent to `install.yaml` per [contracts/rendered-resources.md](contracts/rendered-resources.md);
no secret values in source or any render (Constitution I / FR-013).

## Entity: Chart values

The user-facing configuration contract. Full field-by-field contract with
types, defaults, and warnings: [contracts/values.md](contracts/values.md).

Top-level structure:

```text
operator.queueURL          string   ""            optional queue listener
operator.reconcileInterval string   ""            Go duration, "" = built-in 4h
azure.clientID             string   ""            workload identity wiring
image.{repository,tag,pullPolicy}
serviceAccount.name        string   ""            "" = <prefix>-controller-manager
resources                  object   today's requests/limits
nodeSelector / tolerations / affinity
metrics.enabled            bool     true
serviceMonitor.enabled     bool     false
networkPolicy.enabled      bool     false
crds.{install,keep}        bool     true / true
```

**Validation rules**:
- Empty values file must render a complete, installable manifest set (spec
  edge case) — no required value may be defaulted to a placeholder.
- `azure.clientID` set ⇒ all three WI markers render together (arg, pod
  label, SA annotation); unset ⇒ none of them render (FR-011).
- `image.tag` empty ⇒ `v` + `.Chart.AppVersion` is used (FR-008) — the `v`
  prefix is required because the image is pushed as `:vX.Y.Z`; see
  [contracts/values.md](contracts/values.md) and
  [contracts/release-artifacts.md](contracts/release-artifacts.md).
- No `replicas` field exists; the Deployment hardcodes `replicas: 1` (FR-009).

## Entity: CRD lifecycle settings

State machine over `crds.install` × `crds.keep`:

| `crds.install` | `crds.keep` | Install/upgrade | Uninstall |
|---|---|---|---|
| `true` (default) | `true` (default) | CRD rendered, created or upgraded as part of the release; fails Helm's ownership check if an unowned CRD exists (adoption documented, research.md R2) | CRD and all SecretSync objects remain (`helm.sh/resource-policy: keep`) |
| `true` | `false` | same as above, without the keep annotation | CRD deleted ⇒ all SecretSync objects deleted; managed Secrets cleaned up by the operator's finalizer if it is still running (uninstall-ordering caveat documented) |
| `false` | any | CRD not rendered; cluster must already have it | CRD untouched |

**Transitions of note**: upgrade from release N to N+1 with a schema change
updates the CRD in place (template, not `crds/` dir — research.md R1).
`crds.keep` flipping only changes the annotation; it takes effect at the next
uninstall.

**Flipping `crds.install` true → false on an existing release is a resource
removal, not just "the chart stops managing it".** Helm deletes resources that
disappear from a release's manifest. It leaves the CRD alone only because the
previous revision annotated it `helm.sh/resource-policy: keep`. With the
default `crds.keep=true` this is safe; with `crds.keep=false` the same upgrade
deletes the CRD and every SecretSync object in the cluster. Verified on kind.
`values.yaml` and the README carry this warning.

## Entity: Release artifacts

Per tag `vX.Y.Z`, the release pipeline produces, in dependency order:

| Artifact | Location | Version marker |
|---|---|---|
| Container image (multi-arch) | `ghcr.io/tabman83/kvsynk8s:vX.Y.Z` (+ `latest`) | image tag |
| Rendered manifest | `install.yaml` on the GitHub Release | image ref inside |
| OCI chart package | `oci://ghcr.io/tabman83/charts/kvsynk8s:X.Y.Z` | chart version + appVersion |
| Chart archive | `kvsynk8s-X.Y.Z.tgz` on the GitHub Release | filename + Chart.yaml inside |

**Invariants**: all four exist for every release from this feature onward
(SC-003); chart version == app version == tag minus `v`; a pipeline re-run on
the same tag overwrites all four idempotently (research.md R9). Full contract:
[contracts/release-artifacts.md](contracts/release-artifacts.md).

## Entity: Sync markers (helm-sync regions)

The two machine-managed regions inside otherwise hand-maintained templates:

| Region | Source of truth | Target | Wrapper |
|---|---|---|---|
| CRD body | `config/crd/bases/kvsynk8s.io_secretsyncs.yaml` (controller-gen) | `templates/crds/secretsync-crd.yaml` | `crds.install` gate + `crds.keep`-gated keep annotation |
| ClusterRole rules | `rules:` block of `config/rbac/role.yaml` (controller-gen) | `templates/rbac/manager-clusterrole.yaml` | `# BEGIN/END generated rules (make helm-sync)` markers |

**Validation rule**: `make manifests helm-sync` followed by
`git diff --exit-code charts/` must be clean on every PR (FR-015, SC-006).
