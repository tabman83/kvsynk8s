# Contract: Chart Values

**Feature**: `002-helm-chart` | Consumers: cluster operators running
`helm install/upgrade` | Producer: `charts/kvsynk8s/values.yaml`

Every key below is the *entire* supported surface. Anything not listed is not
configurable through the chart (by design — FR-009, Simplicity First).
`values.yaml` must carry each default and warning below as inline comments;
this file is the review-time contract for that.

## Operator settings

| Key | Type | Default | Effect when set | Effect when unset |
|---|---|---|---|---|
| `operator.queueURL` | string | `""` | Deployment gets `--queue-url=<v>`; operator starts the Event Grid queue listener | no arg; operator runs on periodic reconcile only (today's default) |
| `operator.reconcileInterval` | string (Go duration, e.g. `4h`, `30m`) | `""` | Deployment gets `--reconcile-interval=<v>` | no arg; operator's built-in `4h` default applies |

## Azure Workload Identity

| Key | Type | Default | Effect |
|---|---|---|---|
| `azure.clientID` | string (GUID) | `""` | When set, renders **all three together**: container arg `--azure-client-id=<v>`, pod template label `azure.workload.identity/use: "true"`, ServiceAccount annotation `azure.workload.identity/client-id: <v>`. When unset, none of the three render. |

## Image

| Key | Type | Default | Notes |
|---|---|---|---|
| `image.repository` | string | `ghcr.io/tabman83/kvsynk8s` | |
| `image.tag` | string | `""` | empty ⇒ `.Chart.AppVersion` (i.e. the release version) |
| `image.pullPolicy` | string | `IfNotPresent` | |

## ServiceAccount

| Key | Type | Default | Notes |
|---|---|---|---|
| `serviceAccount.name` | string | `""` | empty ⇒ `<release-name>-controller-manager` (with release name `kvsynk8s`: `kvsynk8s-controller-manager`, matching `install.yaml`) |

**Required warning (FR-012)** — must appear verbatim-in-spirit in
`values.yaml` next to `serviceAccount.name`, and in the README:

> The Microsoft Entra federated credential is bound to the exact
> ServiceAccount name and namespace
> (`system:serviceaccount:<namespace>:<name>`). Renaming the ServiceAccount
> or installing into a different namespace breaks Azure authentication until
> the federated credential on the managed identity is updated to match.

## Scheduling and resources

| Key | Type | Default |
|---|---|---|
| `resources` | object | `requests: {cpu: 10m, memory: 32Mi}`, `limits: {cpu: 200m, memory: 128Mi}` |
| `nodeSelector` | object | `{}` |
| `tolerations` | array | `[]` |
| `affinity` | object | `{}` |

## Feature toggles

| Key | Type | Default | Gates |
|---|---|---|---|
| `metrics.enabled` | bool | `true` | metrics Service, `--metrics-bind-address=:8443` arg, `metrics-auth` ClusterRole + Binding, `metrics-reader` ClusterRole |
| `serviceMonitor.enabled` | bool | `false` | ServiceMonitor (requires the Prometheus Operator CRDs; requires `metrics.enabled=true` — rendering it without metrics is a values error the chart fails with a clear message) |
| `networkPolicy.enabled` | bool | `false` | NetworkPolicy allowing metrics scraping only from namespaces labeled `metrics: enabled` |

## CRD lifecycle

| Key | Type | Default | Meaning |
|---|---|---|---|
| `crds.install` | bool | `true` | render (and therefore create/upgrade) the SecretSync CRD as part of the release; `false` = cluster must already have the CRD |
| `crds.keep` | bool | `true` | annotate the CRD `helm.sh/resource-policy: keep` so uninstall leaves the CRD and all SecretSync objects; `false` = uninstall deletes them (documented as destructive) |

## Explicitly not exposed

| Not a value | Why |
|---|---|
| `replicas` | fixed at 1: the operator runs without leader election; concurrent replicas would reconcile the same Secrets uncoordinated (FR-009) |
| leader election, probe ports, log flags, cert paths | not part of the shipped configuration; add only when a real need appears (Constitution III) |
| namespace | Helm's responsibility (`--namespace kvsynk8s --create-namespace`); the chart renders no Namespace object |
| any secret value | the chart carries configuration only (FR-013, Constitution I) |

## Contract tests (CI)

1. `helm template` with defaults renders successfully and matches
   [rendered-resources.md](rendered-resources.md).
2. `helm template` with every key above set to a non-default value renders
   successfully and each value lands where this contract says.
3. Grep over source and rendered output confirms no `<SET-ME>`-style
   placeholders and no secret-looking material.
