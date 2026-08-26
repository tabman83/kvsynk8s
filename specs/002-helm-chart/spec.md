# Feature Specification: Helm Chart Install Method

**Feature Branch**: `002-helm-chart`

**Created**: 2026-08-23

**Status**: Draft

**Input**: User description: "Add a Helm chart as a second, first-class install method for kvsynk8s, alongside the existing rendered install.yaml. A user can install the operator with `helm install kvsynk8s oci://ghcr.io/tabman83/charts/kvsynk8s --version X.Y.Z` on Helm >= 3.8, with no chart repository to add. Each v* release publishes the chart to GHCR as an OCI package and also attaches the packaged .tgz to the GitHub Release next to install.yaml. Chart version and app version are both derived from the git tag. The chart lives in this repo at charts/kvsynk8s/, treated as reviewed source. Default rendered names and namespace must match the current kustomize output. The SecretSync CRD is rendered as a chart template gated by crds.install and protected by a keep policy behind crds.keep. values.yaml exposes the operator's actual configuration surface plus workload identity as a first-class value. No secret value ever appears in the chart or its rendered output. A make helm-sync target keeps generated CRD and RBAC in sync with the chart, enforced by CI. PR CI runs helm lint and helm template. README gains a Helm install section."

## Clarifications

### Session 2026-08-23

- Q: Should the chart expose a `replicas` value even though the operator currently always runs with leader election disabled? → A: No — the chart hardcodes 1 replica and does not expose a `replicas` value; it can be added later if leader election is ever enabled.
- Q: What is the supported behavior when the SecretSync CRD already exists (e.g., from an `install.yaml` install) and the chart installs with default `crds.install=true`? → A: The install fails on Helm's ownership check; the documentation provides lossless adoption steps (annotate/label the existing CRD so Helm takes it over) plus the `crds.install=false` escape hatch. No takeover logic is built into the chart.
- Q: Should the chart render the `kvsynk8s` Namespace object itself, or leave namespace creation to Helm? → A: No Namespace object in the chart; the documented install command standardizes on `--namespace kvsynk8s --create-namespace`, and equivalence with `install.yaml` explicitly excludes the Namespace resource.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Install the operator with Helm in one command (Priority: P1)

A cluster operator who manages their platform with Helm installs kvsynk8s directly from the project's container registry with a single `helm install` command, without adding a chart repository, downloading manifests, or editing YAML. The default installation is equivalent to applying the released `install.yaml`: same namespace, same resource names, same security posture.

**Why this priority**: This is the whole feature. Helm is the dominant install tool in Kubernetes platform teams; without this story nothing else in the spec has value.

**Independent Test**: On a fresh cluster, run the single install command against a released chart version and verify the operator Deployment becomes Ready in the `kvsynk8s` namespace and the SecretSync CRD is available — with no other steps.

**Acceptance Scenarios**:

1. **Given** a fresh cluster and Helm >= 3.8, **When** the user runs `helm install kvsynk8s oci://ghcr.io/tabman83/charts/kvsynk8s --version X.Y.Z --namespace kvsynk8s --create-namespace`, **Then** the operator Deployment reaches Ready and the SecretSync CRD is installed, with no chart repository configured beforehand.
2. **Given** a default chart install (into namespace `kvsynk8s`) and a cluster with the released `install.yaml` applied, **When** the rendered resources of both are compared, **Then** they produce equivalent clusters: same resource kinds, names, namespace (`kvsynk8s`), name prefix (`kvsynk8s-`), RBAC scope, and pod security settings — except the Namespace object itself, which the chart does not render (Helm's `--create-namespace` covers it).
3. **Given** a default chart install, **When** the user creates a valid SecretSync object, **Then** the operator processes it exactly as it would under an `install.yaml` install.

---

### User Story 2 - Configure the installation through values (Priority: P2)

A cluster operator tailors the installation at install/upgrade time through chart values instead of patching manifests: the queue URL, reconcile interval, and Azure client ID; the image reference; Azure Workload Identity wiring; scheduling controls; and toggles for the optional pieces (metrics, ServiceMonitor, NetworkPolicy).

**Why this priority**: An unconfigurable chart forces users back to manifest editing, which defeats the point of offering Helm. But it depends on Story 1 existing.

**Independent Test**: Render the chart with non-default values and verify each value lands in the right place in the output; install with a queue URL set and verify the operator starts its queue listener.

**Acceptance Scenarios**:

1. **Given** the chart, **When** the user sets values for queue URL, reconcile interval, or Azure client ID, **Then** the rendered operator Deployment passes those settings to the operator process, and omitting them yields the same defaults the operator has today (no queue, 4h interval).
2. **Given** the chart, **When** the user sets an Azure client ID for workload identity, **Then** the rendered pod carries the `azure.workload.identity/use: "true"` label and the ServiceAccount carries the `azure.workload.identity/client-id` annotation.
3. **Given** the chart, **When** the user overrides image repository/tag/pullPolicy, resources, nodeSelector, tolerations, or affinity, **Then** the rendered Deployment reflects each override, and the image tag defaults to `v` + the chart's app version when not set (FR-008).
4. **Given** the chart, **When** the user enables or disables metrics, ServiceMonitor, or NetworkPolicy toggles, **Then** the corresponding resources appear or disappear from the rendered output, matching what the existing kustomize overlays offer.
5. **Given** the values documentation, **When** a user reads the ServiceAccount and namespace settings, **Then** they find an explicit warning that renaming either breaks the Entra federated credential binding.

---

### User Story 3 - Every release ships the chart automatically (Priority: P2)

A maintainer tags a release (`vX.Y.Z`) and, with no manual steps, the release pipeline publishes the chart as an OCI package to the project's registry and attaches the packaged chart archive to the GitHub Release next to `install.yaml`. Chart version and app version both equal the tag.

**Why this priority**: Without automated publishing the chart goes stale immediately; but it only matters once the chart (Story 1) exists.

**Independent Test**: Push a `v*` tag on a test release and verify the OCI package exists at the expected reference with the right version, and the `.tgz` is attached to the GitHub Release.

**Acceptance Scenarios**:

1. **Given** a `vX.Y.Z` tag is pushed and the release pipeline succeeds, **When** a user runs the install command with `--version X.Y.Z`, **Then** the chart is found and its app version equals X.Y.Z.
2. **Given** a completed release, **When** a user opens the GitHub Release page, **Then** both `install.yaml` and the packaged chart `.tgz` are attached.
3. **Given** a chart-only fix, **When** the maintainer ships it, **Then** it goes out as a new patch tag (no independent chart versioning).

---

### User Story 4 - Safe lifecycle: upgrades update the CRD, uninstall preserves data (Priority: P3)

A cluster operator upgrades the release and gets the new CRD schema along with the new operator version. When they uninstall, the SecretSync CRD — and therefore every SecretSync object — survives by default, so an uninstall/reinstall cycle does not destroy the cluster's sync configuration.

**Why this priority**: Lifecycle safety is what separates a first-class chart from a demo chart, but it only becomes observable after install and release exist.

**Independent Test**: Install, create SecretSync objects, uninstall, verify CRD and objects remain; reinstall and verify the operator resumes managing them.

**Acceptance Scenarios**:

1. **Given** release N is installed and release N+1 changes the CRD schema, **When** the user upgrades, **Then** the CRD in the cluster is updated to the new schema (the CRD is managed as an upgradeable part of the release, not install-once).
2. **Given** a default install with existing SecretSync objects, **When** the user uninstalls the release, **Then** the CRD and all SecretSync objects remain in the cluster.
3. **Given** a user who explicitly opts out of CRD management (`crds.install=false`) on a cluster where the CRD already exists, **When** they install, **Then** the chart installs cleanly without touching the existing CRD.

---

### User Story 5 - The chart cannot silently drift from the operator (Priority: P3)

A contributor changes the operator's API or RBAC. The repository provides a single command that re-syncs the generated CRD and RBAC rules into the chart, and CI fails any pull request where the chart no longer matches the generated sources — the same guardrail pattern the project already uses for generated manifests.

**Why this priority**: This protects the long-term truthfulness of the chart; it has no user-visible value on day one but prevents the classic failure mode of in-repo charts.

**Independent Test**: Modify the CRD source, do not run the sync command, open a PR, and verify CI fails; run the sync command and verify CI passes.

**Acceptance Scenarios**:

1. **Given** a change to the operator's CRD or RBAC markers, **When** the contributor runs the sync command, **Then** the chart's CRD and RBAC content match the newly generated output.
2. **Given** a pull request where generated CRD/RBAC and the chart disagree, **When** CI runs, **Then** the build fails with a diff pointing at the drift.
3. **Given** any pull request, **When** CI runs, **Then** the chart is linted and rendered with default values, and a chart that fails linting or produces invalid Kubernetes objects fails the build.

---

### Edge Cases

- A user who installed via `install.yaml` later runs `helm install` on the same cluster: Helm will not adopt the existing resources and the install fails on conflicts. The documentation must state this and describe the supported path (remove the manifest-based install first).
- The CRD already exists (from `install.yaml` or a previous release) when the chart installs with default `crds.install=true`: the install fails on Helm's ownership check. The documentation provides lossless adoption steps (annotate/label the existing CRD so Helm takes ownership) and the `crds.install=false` escape hatch; the chart itself contains no takeover logic.
- Rendering with an empty values file (all defaults) must produce a complete, valid, installable manifest set — defaults may not leave required fields empty.
- A user overrides the namespace or ServiceAccount name: the install still works, but Azure authentication breaks until the federated credential is updated — the values documentation must warn about this rather than the chart trying to prevent it.
- Uninstall with `crds.keep=false` explicitly set: the CRD and all SecretSync objects are deleted — this is the user's informed choice; managed Secrets are then cleaned up per the operator's existing deletion behavior while the operator is still running, and documentation must note that uninstall ordering affects this.
- The release pipeline fails after the image is pushed but before the chart is published: the release is incomplete; the pipeline must fail loudly so the maintainer re-runs it, and a re-run must be able to overwrite/republish the same version.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The project MUST provide a Helm chart that installs the complete operator (namespace-scoped workload, ServiceAccount, RBAC, CRD, health probes, metrics wiring) on Helm >= 3.8.
- **FR-002**: The chart MUST be installable directly from the project's OCI registry (`oci://ghcr.io/tabman83/charts/kvsynk8s`) with no chart repository configuration.
- **FR-003**: The chart MUST live in this repository under `charts/kvsynk8s/` and be treated as reviewed source, not generated build output.
- **FR-004**: A default-values install into namespace `kvsynk8s` MUST render resources equivalent to the released `install.yaml`: resource name prefix `kvsynk8s-`, and the same RBAC scope and pod security settings. The chart MUST NOT render a Namespace object; namespace creation is Helm's responsibility (`--create-namespace`), and the documented install command pins `--namespace kvsynk8s`.
- **FR-005**: The SecretSync CRD MUST be rendered as an upgradeable part of the release (a chart template, not an install-once artifact), gated by a `crds.install` value defaulting to true.
- **FR-006**: With the default `crds.keep=true`, uninstalling the release MUST leave the CRD (and therefore all SecretSync objects) in the cluster; setting it to false makes uninstall remove them.
- **FR-007**: The chart's values MUST expose the operator's configuration surface: queue URL, reconcile interval, and Azure client ID — with the same defaults the operator applies when they are unset.
- **FR-008**: The chart's values MUST expose image repository, tag, and pull policy; the repository MUST default to `ghcr.io/tabman83/kvsynk8s` and the tag MUST default to `v` + the chart's app version (the image is pushed under the tag with the `v` prefix; see contracts/release-artifacts.md, Version invariants).
- **FR-009**: The chart's values MUST expose resources, nodeSelector, tolerations, and affinity for the operator workload. The replica count is fixed at 1 and MUST NOT be exposed as a value, because the operator runs without leader election and concurrent replicas would reconcile the same Secrets uncoordinated.
- **FR-010**: The chart MUST provide toggles for metrics exposure, ServiceMonitor, and NetworkPolicy that match what the existing kustomize configuration offers, each defaulting to the current default behavior.
- **FR-011**: Workload identity MUST be configurable as a first-class value: setting the Azure client ID MUST produce the `azure.workload.identity/use: "true"` pod label and the `azure.workload.identity/client-id` ServiceAccount annotation.
- **FR-012**: The values documentation MUST warn that changing the ServiceAccount name or namespace breaks the Entra federated credential binding.
- **FR-013**: Neither the chart source nor its rendered output for any values combination may contain a secret value; the chart carries configuration only.
- **FR-014**: Every `v*` release MUST publish the chart to the OCI registry and attach the packaged `.tgz` to the GitHub Release next to `install.yaml`, with chart version and app version both equal to the tag (without the `v` prefix).
- **FR-015**: The repository MUST provide a single command (`make helm-sync`) that copies the generated CRD and RBAC rules into the chart, and CI MUST fail when running it would produce a diff.
- **FR-016**: PR CI MUST lint the chart and render it with default values, failing on lint errors or invalid output.
- **FR-017**: The README MUST document the Helm install method alongside the existing `kubectl apply` method, including the migration caveat that Helm does not adopt resources installed from `install.yaml`, the lossless CRD adoption steps for migrating clusters, and the `crds.install=false` escape hatch.

### Key Entities

- **Helm chart**: The packaged, versioned install unit; mirrors the operator's existing deployment shape; its version always equals the operator release version.
- **Chart values**: The user-facing configuration contract — operator settings, image reference, workload identity wiring, scheduling, and feature toggles; documented inline with defaults and warnings.
- **CRD lifecycle settings** (`crds.install`, `crds.keep`): The user's control over whether the chart manages the SecretSync CRD and whether uninstall preserves it.
- **Release artifacts**: Per release tag: the container image, `install.yaml`, the OCI chart package, and the `.tgz` on the GitHub Release — all carrying the same version.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A user on a fresh cluster gets a Ready operator with one install command in under 5 minutes, editing zero manifest files.
- **SC-002**: A default chart install and the released `install.yaml` produce equivalent cluster state (same resources, names, namespace, security settings), verified by comparing rendered output.
- **SC-003**: Every release from the feature's merge onward ships both install methods (manifest and chart) with matching versions, with zero manual publishing steps.
- **SC-004**: An install → create SecretSyncs → uninstall → reinstall cycle with default settings loses zero SecretSync objects.
- **SC-005**: No secret value appears in chart source or rendered output for any supported values combination.
- **SC-006**: 100% of pull requests that change the operator's CRD or RBAC without updating the chart are blocked by CI.

## Assumptions

- Users installing via Helm run Helm >= 3.8 (OCI support); older Helm versions are not supported and are not documented as a target.
- Chart version and app version are intentionally coupled to the git tag; a chart-only change ships as a new patch release of the whole project. Independent chart versioning is out of scope.
- The initial chart content may be scaffolded with a generator, but from the first commit onward the chart is hand-maintained source; no generator runs in CI or the release pipeline.
- Migration from an existing `install.yaml` install to Helm is documentation only: remove the manifest-based workload install, adopt the existing CRD via the documented annotate/label steps (preserving SecretSync objects), then install via Helm. No adoption tooling is built.
- The pull-request CI validation is lint + render (plus schema validation of the rendered output if cheap to add); installing the chart into the existing kind-based e2e cluster is a stretch goal, not a requirement of this feature.
- Out of scope: Artifact Hub listing, a chart repository served via GitHub Pages/chart-releaser, umbrella or sub-charts.
