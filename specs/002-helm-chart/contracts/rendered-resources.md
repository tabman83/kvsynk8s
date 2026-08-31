# Contract: Default Rendered Resources (equivalence with install.yaml)

**Feature**: `002-helm-chart` | Verified by: `hack/compare-helm-kustomize.sh`
in CI (SC-002, FR-004)

Command under contract:

```bash
helm template kvsynk8s charts/kvsynk8s --namespace kvsynk8s
```

with an empty values override (all defaults) MUST render exactly this
resource inventory — the output of `kustomize build config/default` minus the
Namespace object:

| Kind | Name | Scope/Namespace |
|---|---|---|
| CustomResourceDefinition | `secretsyncs.kvsynk8s.io` | cluster |
| ServiceAccount | `kvsynk8s-controller-manager` | `kvsynk8s` |
| ClusterRole | `kvsynk8s-manager-role` | cluster |
| ClusterRole | `kvsynk8s-metrics-auth-role` | cluster |
| ClusterRole | `kvsynk8s-metrics-reader` | cluster |
| ClusterRole | `kvsynk8s-secretsync-admin-role` | cluster |
| ClusterRole | `kvsynk8s-secretsync-editor-role` | cluster |
| ClusterRole | `kvsynk8s-secretsync-viewer-role` | cluster |
| ClusterRoleBinding | `kvsynk8s-manager-rolebinding` | cluster |
| ClusterRoleBinding | `kvsynk8s-metrics-auth-rolebinding` | cluster |
| Service | `kvsynk8s-controller-manager-metrics-service` | `kvsynk8s` |
| Deployment | `kvsynk8s-operator` | `kvsynk8s` |

Explicitly absent from the default render: Namespace (Helm's
`--create-namespace`), ServiceMonitor, NetworkPolicy (toggles default off,
matching the kustomize defaults), any Secret, any workload-identity label or
annotation (rendered only when `azure.clientID` is set).

## Field-level equivalence (Deployment)

The comparison script asserts these fields match `install.yaml`'s Deployment:

- `spec.replicas: 1` (hardcoded, no value).
- Pod `securityContext`: `runAsNonRoot: true`, `seccompProfile.type: RuntimeDefault`.
- Container `securityContext`: `readOnlyRootFilesystem: true`,
  `allowPrivilegeEscalation: false`, `capabilities.drop: ["ALL"]`.
- Args include `--metrics-bind-address=:8443` (metrics on by default) and
  `--health-probe-bind-address=:8081`.
  Note: neither install method passes `--leader-elect` any more. The operator
  hardcodes `LeaderElection: false` in `cmd/main.go` and ignores the flag, and
  no lease RBAC is granted, so shipping it in the manifests only suggested that
  scaling the Deployment was safe (it is not — replicas stays 1, FR-009). The
  binary still accepts the flag, so a hand-written Deployment that passes it
  keeps working; both install methods must simply agree not to.
- Container port `8081` (name `health`); pod template annotation
  `kubectl.kubernetes.io/default-container: manager`.
- Probes: liveness `/healthz:8081` (delay 15s / period 20s), readiness
  `/readyz:8081` (delay 5s / period 10s).
- Resources: requests `10m/32Mi`, limits `200m/128Mi`.
- `serviceAccountName: kvsynk8s-controller-manager`,
  `terminationGracePeriodSeconds: 10`.
- Selector labels include `control-plane: controller-manager` and
  `app.kubernetes.io/name: kvsynk8s` (so the metrics Service and
  NetworkPolicy selectors keep working).
- `spec.strategy.type: Recreate` (hardcoded, no value). Added by
  [specs/003-single-replica-invariant/contracts/deployment-rollout.md](../../003-single-replica-invariant/contracts/deployment-rollout.md),
  which is now the source of truth for this field: without it, Kubernetes'
  default rolling update at `replicas: 1` starts the replacement pod before
  the old one terminates.

## Field-level equivalence (RBAC)

- `kvsynk8s-manager-role` rules are byte-identical to the `rules:` block of
  the generated `config/rbac/role.yaml` (enforced by `make helm-sync` +
  drift check, FR-015).
- Bindings reference the same role/SA pairs as `install.yaml`.

## Tolerated differences

- Helm release metadata labels/annotations
  (`app.kubernetes.io/managed-by: Helm`, `meta.helm.sh/*`) versus kustomize's
  `app.kubernetes.io/managed-by: kustomize`.
- Absence of the `<SET-ME>` workload-identity placeholder annotation and the
  always-on `azure.workload.identity/use` label that `install.yaml` carries
  (the chart renders workload-identity wiring only when configured).
- YAML formatting, key ordering, document order.
