# Research: Helm Chart Install Method

**Feature**: `002-helm-chart` | **Date**: 2026-08-23

All findings below were checked against current Helm documentation (helm.sh,
via Context7) and the repository's actual kustomize output
(`kustomize build config/default`), which renders, in order: Namespace
`kvsynk8s`, CRD `secretsyncs.kvsynk8s.io`, ServiceAccount
`kvsynk8s-controller-manager`, ClusterRoles `kvsynk8s-manager-role`,
`kvsynk8s-metrics-auth-role`, `kvsynk8s-metrics-reader`,
`kvsynk8s-secretsync-{admin,editor,viewer}-role`, ClusterRoleBindings
`kvsynk8s-manager-rolebinding`, `kvsynk8s-metrics-auth-rolebinding`, Service
`kvsynk8s-controller-manager-metrics-service`, Deployment `kvsynk8s-operator`.

## R1. CRD delivery: chart template, not `crds/` directory

**Decision**: Render the SecretSync CRD as a normal template at
`templates/crds/secretsync-crd.yaml`, wrapped in
`{{- if .Values.crds.install }}`, with
`helm.sh/resource-policy: keep` added to `metadata.annotations` when
`.Values.crds.keep` is true (default true).

**Rationale**: Helm's special `crds/` directory cannot contain template
directives and is install-once: Helm 3 skips it entirely if the CRD already
exists and never upgrades it. That directly violates FR-005 (CRD must be an
upgradeable part of the release) and makes `crds.install`/`crds.keep`
impossible. Helm docs explicitly acknowledge templating CRDs in `templates/`
as the alternative for charts that need CRD upgrades. The `keep` policy makes
`helm uninstall` leave the CRD (and therefore all SecretSync objects) in
place, satisfying FR-006/SC-004.

**Alternatives considered**:
- `crds/` directory — rejected: no templating, no upgrades, no keep gating.
- Separate CRD sub-chart — rejected: spec puts umbrella/sub-charts out of
  scope; adds versioning complexity for one CRD.
- Helm 4 `--take-ownership`-based flows — not assumed; the chart targets
  Helm >= 3.8 and stays mechanism-free (adoption is documentation only).

## R2. Existing-CRD adoption (documentation only)

**Decision**: Document the three ownership fields Helm checks, as the lossless
adoption path for clusters that installed via `install.yaml`:

```bash
kubectl label crd secretsyncs.kvsynk8s.io app.kubernetes.io/managed-by=Helm
kubectl annotate crd secretsyncs.kvsynk8s.io \
  meta.helm.sh/release-name=kvsynk8s \
  meta.helm.sh/release-namespace=kvsynk8s
```

plus `--set crds.install=false` as the escape hatch. No takeover logic in the
chart (per clarification session 2026-08-23).

**Rationale**: Helm verifies exactly these three metadata fields
(`app.kubernetes.io/managed-by=Helm` label, `meta.helm.sh/release-name` and
`meta.helm.sh/release-namespace` annotations) before taking ownership;
setting them preserves the CRD object and every SecretSync in the cluster.

**Alternatives considered**: pre-install hook that adopts automatically —
rejected as hidden magic that mutates resources the release does not own yet
(violates Simplicity First and the spec's explicit "no takeover logic").

## R3. OCI publishing to GHCR

**Decision**: In `release.yml`, after the image build job succeeds:

```bash
VERSION="${GITHUB_REF_NAME#v}"
helm package charts/kvsynk8s --version "$VERSION" --app-version "$VERSION"
echo "$GITHUB_TOKEN" | helm registry login ghcr.io -u "$GITHUB_ACTOR" --password-stdin
helm push "kvsynk8s-$VERSION.tgz" oci://ghcr.io/tabman83/charts
```

then upload the `.tgz` as a workflow artifact and add it to the existing
`softprops/action-gh-release` `files:` list next to `dist/install.yaml`.

**Rationale**: `helm push` infers basename and tag from chart name and
version, producing exactly `oci://ghcr.io/tabman83/charts/kvsynk8s:X.Y.Z` —
the install reference in FR-002. `GITHUB_TOKEN` with the already-used
`packages: write` permission is sufficient for GHCR. OCI tags are mutable, so
a failed-then-rerun release republishes the same version cleanly (edge case
in spec). Consumers need Helm >= 3.8, where OCI support is GA — matching the
spec's stated floor.

**Alternatives considered**:
- `chart-releaser` + GitHub Pages repository — rejected: spec puts a classic
  chart repository out of scope; OCI needs zero extra infrastructure.
- Publishing from a separate workflow — rejected: chart version must ship in
  lockstep with the image; one pipeline, one tag, one version (SC-003).

## R4. Chart versioning: placeholder in repo, tag at package time

**Decision**: `Chart.yaml` in the repository carries `version: 0.0.0` and
`appVersion: 0.0.0` as explicit dev placeholders. The release pipeline
overrides both with `helm package --version/--app-version` derived from the
tag (strip the `v`). Nothing ever rewrites `Chart.yaml` in git.

**Rationale**: keeps the chart pure reviewed source (FR-003) with no
version-bump commits and no generator in the release path; `helm lint` and
`helm template` work fine with the placeholder. `--app-version` also feeds
the image tag default (`.Chart.AppVersion`), satisfying FR-008.

**Alternatives considered**: committing the real version per release —
rejected: requires a bot commit on tag, races with the tag itself, and adds a
step that can drift.

## R5. `make helm-sync`: marker-based splice, checked in CI

**Decision**: `hack/helm-sync.sh` (invoked by `make helm-sync`, which depends
on `make manifests`) deterministically regenerates two regions of the chart:

1. **CRD**: takes `config/crd/bases/kvsynk8s.io_secretsyncs.yaml` verbatim
   and wraps it with a fixed header/footer: the `crds.install` gate, and a
   `crds.keep`-gated `helm.sh/resource-policy: keep` annotation injected into
   the (already present, controller-gen-generated) `metadata.annotations`
   block. The wrapper text lives in the script, so output is byte-stable.
2. **RBAC rules**: extracts the `rules:` block from `config/rbac/role.yaml`
   and splices it between `# BEGIN generated rules (make helm-sync)` /
   `# END generated rules` markers inside
   `templates/rbac/manager-clusterrole.yaml`.

CI runs `make manifests helm-sync` and fails on
`git diff --exit-code charts/` — the same generated-sources guardrail pattern
`make test` already relies on (SC-006, FR-015).

**Rationale**: only the two pieces that are *generated from Go markers* are
synced; everything else in the chart is hand-maintained source, matching the
spec's "reviewed source, not build output" stance. A diff-based CI check is
cheap, and the failure output *is* the drift diff the spec asks for.

**Alternatives considered**:
- Full chart generation (helmify or kustomize→helm converters) — rejected:
  spec allows a generator only for initial scaffolding; generated charts are
  unreviewable and mangle template gating.
- Symlinking/copying raw files without gating — rejected: the CRD template
  needs the install/keep gates, which raw copies cannot carry.

## R6. Values surface and defaults (mapping to the real operator)

**Decision**: expose exactly the surface below, mapped to `cmd/main.go`
flags and the kustomize defaults. Omitted values reproduce today's behavior.

| Value | Default | Renders as |
|---|---|---|
| `operator.queueURL` | `""` | `--queue-url=...` arg only when set (unset = no queue listener, periodic-only) |
| `operator.reconcileInterval` | `""` | `--reconcile-interval=...` arg only when set (unset = operator's built-in 4h) |
| `azure.clientID` | `""` | when set: `--azure-client-id` arg **and** pod label `azure.workload.identity/use: "true"` **and** SA annotation `azure.workload.identity/client-id` (FR-011) |
| `image.repository` | `ghcr.io/tabman83/kvsynk8s` | container image |
| `image.tag` | `""` → `v` + `.Chart.AppVersion` | image tag (the image is pushed with the `v` prefix, FR-008) |
| `image.pullPolicy` | `IfNotPresent` | container pullPolicy |
| `serviceAccount.name` | `""` → `<prefix>-controller-manager` | SA name (values.yaml carries the federated-credential warning, FR-012) |
| `resources` | requests 10m/32Mi, limits 200m/128Mi | container resources (today's manager.yaml values) |
| `nodeSelector` / `tolerations` / `affinity` | `{}` / `[]` / `{}` | pod scheduling |
| `metrics.enabled` | `true` | metrics Service, `--metrics-bind-address=:8443` arg, metrics-auth + metrics-reader RBAC |
| `serviceMonitor.enabled` | `false` | ServiceMonitor (kustomize default: commented out) |
| `networkPolicy.enabled` | `false` | NetworkPolicy (kustomize default: commented out) |
| `crds.install` / `crds.keep` | `true` / `true` | see R1 |

Not exposed, deliberately: `replicas` (fixed at 1 — clarification 2026-08-23;
the deployment template hardcodes it with a comment explaining why),
leader-election, probe ports, log flags, webhook/metrics cert paths (unused
in the shipped configuration).

**Rationale**: FR-007..FR-011 plus Simplicity First: every value corresponds
to something a user can actually change today by editing manifests; nothing
speculative. `metrics.enabled=true` matches the kustomize default
(`metrics_service.yaml` + the `:8443` patch are active in
`config/default/kustomization.yaml`; prometheus and network-policy are not).

**Workload-identity note**: `install.yaml` today always carries the
`azure.workload.identity/use` pod label and a `<SET-ME>` placeholder
annotation. The chart improves on this: WI wiring renders **only when
`azure.clientID` is set**, so a default render contains no placeholder
values. SC-002 equivalence is defined over kinds/names/namespace/RBAC/pod
security, not over these labels, so this stays within spec.

## R7. No `values.schema.json` (for now)

**Decision**: skip JSON-schema validation of values in this feature.

**Rationale**: Simplicity First — the values surface is small and flat, every
value is exercised by the CI render matrix, and a schema is a second copy of
the contract that can drift. Can be added later if user error reports show a
need.

## R8. PR CI: dedicated `helm.yml` workflow

**Decision**: a new `.github/workflows/helm.yml` on `pull_request` (and
`push` to master, matching the other workflows) with steps:

1. `helm lint charts/kvsynk8s` (helm from the ubuntu-latest runner image,
   pinned via `azure/setup-helm` with an explicit version for
   reproducibility).
2. `helm template` with default values → must succeed and be non-empty.
3. `helm template` with a representative non-default values file (queue URL,
   clientID, toggles flipped, image overridden) → must succeed.
4. Drift check: `make manifests helm-sync && git diff --exit-code charts/`.
5. Equivalence check: `hack/compare-helm-kustomize.sh` — renders
   `helm template kvsynk8s charts/kvsynk8s --namespace kvsynk8s` and
   `bin/kustomize build config/default`, then compares the sorted
   `kind/namespace/name` inventory (excluding the Namespace object) and the
   Deployment's security contexts, probes, and args (SC-002, FR-016).

**Rationale**: mirrors the repo's one-concern-per-workflow layout; the
equivalence script makes SC-002 a permanent regression guard instead of a
one-time manual diff. Schema validation of rendered output (kubeconform) was
considered "if cheap" by the spec — deferred: `helm template` already catches
YAML/structural errors, and the CRD/RBAC content is generated by
controller-gen, which emits valid objects by construction.

**Alternatives considered**: extending `test.yml` — rejected: keeps Go and
chart concerns separately re-runnable, consistent with existing layout.

## R9. Release-pipeline failure modes

**Decision**: the chart publish steps run in a job that `needs:
build-and-push` (image first, chart second); the GitHub Release job `needs:`
the chart job and attaches both files, so a half-published release leaves the
pipeline visibly red. Re-running the workflow on the same tag re-pushes the
same OCI version (tags are mutable, R3) and `action-gh-release` updates the
existing release's assets idempotently.

**Rationale**: matches the spec's edge case "pipeline fails after the image
is pushed but before the chart is published": nothing needs manual cleanup, a
re-run converges.

## R10. Naming: fixed base names behind a release-name prefix

**Decision**: `_helpers.tpl` defines a name prefix that equals the release
name (truncated to DNS limits). Each template appends the same base name
kustomize uses (`operator`, `controller-manager`, `manager-role`,
`manager-rolebinding`, `metrics-auth-role`, `metrics-reader`,
`controller-manager-metrics-service`, ...). With the documented install
command (`helm install kvsynk8s ... --namespace kvsynk8s`), rendered names
are byte-identical to `install.yaml` (FR-004): e.g. `kvsynk8s-operator`,
`kvsynk8s-controller-manager`.

**Rationale**: keeps cluster-scoped resources (ClusterRoles/Bindings)
collision-free if someone installs under a different release name, while the
default command reproduces today's names exactly. Labels use the chart's own
consistent set (`app.kubernetes.io/name: kvsynk8s`,
`control-plane: controller-manager` for selector compatibility with the
existing Service/NetworkPolicy selectors).
