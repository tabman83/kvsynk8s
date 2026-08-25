# kvsynk8s

A Kubernetes operator that syncs Azure Key Vault secrets into native Kubernetes
Secrets, event-driven instead of polling.

You declare a `SecretSync` custom resource pointing at a vault secret. The
operator creates a real Kubernetes `Secret` with that value and keeps it
current: when the secret is rotated in Key Vault, the operator finds out
through an Event Grid notification (delivered via an Azure Storage Queue) and
updates the Kubernetes Secret within seconds, not on the next poll.

## How it differs from akv2k8s

[akv2k8s](https://akv2k8s.io/) is the closest prior art and solves the same
basic problem, but the two differ on two points:

- **Notification model.** akv2k8s polls Key Vault on an interval. kvsynk8s
  reacts to Key Vault's own change notifications (Event Grid →
  Storage Queue), so a rotation reaches the cluster in under a minute instead
  of waiting for the next poll. A periodic reconciliation (default every hour)
  still runs underneath as a safety net for missed notifications, vault-side
  deletions, and in-cluster drift — so the cluster never depends on
  notifications alone to stay correct.
- **No webhook/injector.** akv2k8s also offers env-injection into pods via a
  mutating webhook (`akv2k8s-env-injector`). kvsynk8s does not: it only ever
  writes a plain Kubernetes `Secret` object. There is no admission webhook, no
  inbound network endpoint of any kind — the operator only ever makes outbound
  calls (to the queue, to Key Vault, to the Kubernetes API). Consume the
  Secret the standard Kubernetes way (env var, volume mount).

Scope in v1 is Key Vault **secrets** only (no certificates or keys), one vault
secret mapped to one Kubernetes Secret.

## Install

### Option A — Helm (recommended)

Needs Helm 3.8 or newer (that is where OCI support went GA).

```bash
helm install kvsynk8s oci://ghcr.io/tabman83/charts/kvsynk8s \
  --version 0.1.0 \
  --namespace kvsynk8s --create-namespace \
  --set azure.clientID=<managed-identity-client-id> \
  --set operator.queueURL=https://<storage>.queue.core.windows.net/<queue>
```

The chart version is always the release version, and the image tag defaults to
it, so you never have to line up two versions by hand. You still need the
Azure setup below.

No credentials and no `helm repo add` needed: the packages are public, linked
to this repository.

Values you can set (the full list is in `charts/kvsynk8s/values.yaml`, each one
with a comment):

| Value | Default | What it does |
|---|---|---|
| `operator.queueURL` | `""` | Storage queue with the Key Vault events. Unset means periodic reconcile only. |
| `operator.reconcileInterval` | `""` | Go duration. Unset means the built-in 4h. |
| `azure.clientID` | `""` | Managed identity client ID. Setting it wires up workload identity. |
| `image.repository` / `image.tag` / `image.pullPolicy` | `ghcr.io/tabman83/kvsynk8s` / `v` + chart appVersion / `IfNotPresent` | The operator image. Empty `image.tag` resolves to the release's own image, e.g. `:v1.2.3`. |
| `serviceAccount.name` | `""` | Defaults to `<release-name>-controller-manager`. |
| `resources`, `nodeSelector`, `tolerations`, `affinity` | see values.yaml | Pod scheduling and limits. |
| `metrics.enabled` | `true` | Metrics Service, `:8443` arg, and the RBAC that protects it. |
| `serviceMonitor.enabled` | `false` | Prometheus Operator ServiceMonitor. Needs `metrics.enabled=true`. |
| `networkPolicy.enabled` | `false` | Only namespaces labeled `metrics: enabled` may scrape. |
| `crds.install` | `true` | The SecretSync CRD is part of the release, so upgrades update it. |
| `crds.keep` | `true` | `helm uninstall` leaves the CRD and all SecretSync objects alone. |

There is no `replicas` value. The operator runs without leader election, so a
second replica would reconcile the same SecretSync objects and write the same
Secrets uncoordinated.

**Warning about `serviceAccount.name` and the namespace.** The Entra federated
credential is bound to the exact ServiceAccount name and namespace
(`system:serviceaccount:<namespace>:<name>`). Renaming the ServiceAccount or
installing into a different namespace breaks Azure authentication until you
update the federated credential on the managed identity to match.

#### Coming from the install.yaml manifest

Helm does not adopt resources it did not create. Installing the chart over a
manifest install fails on the existing objects. Remove the manifest install
first:

```bash
# use whichever version you installed
kubectl delete -f https://github.com/tabman83/kvsynk8s/releases/download/v0.1.0/install.yaml
```

That deletes the CRD too, and with it every SecretSync object. If you want to
keep them, hand the CRD over to Helm instead of deleting it. Helm checks
exactly three fields before it takes ownership:

```bash
kubectl label crd secretsyncs.kvsynk8s.io app.kubernetes.io/managed-by=Helm
kubectl annotate crd secretsyncs.kvsynk8s.io \
  meta.helm.sh/release-name=kvsynk8s \
  meta.helm.sh/release-namespace=kvsynk8s
```

Then delete the rest of the manifest install (not the CRD) and run
`helm install`. The CRD object and every SecretSync in the cluster survive.

If you would rather keep the CRD out of the release entirely — because
something else manages it — install with `--set crds.install=false`. The
cluster must already have the CRD in that case.

Careful when flipping `crds.install` to false on a release that already exists.
Removing the CRD from the manifest is a resource removal, so Helm would delete
it. It does not, because `crds.keep` (true by default) annotated the CRD
`helm.sh/resource-policy: keep`. But if you set both `crds.install=false` and
`crds.keep=false`, that upgrade deletes the CRD and every SecretSync object in
the cluster.

#### Uninstall

```bash
helm uninstall kvsynk8s --namespace kvsynk8s
```

By default the CRD stays (`crds.keep=true`), so your SecretSync objects and
the Secrets the operator wrote are untouched and a reinstall picks up where it
left off.

With `crds.keep=false`, uninstall deletes the CRD, which deletes every
SecretSync object. Note the ordering: Helm removes the operator Deployment in
the same operation, so the finalizer that cleans up managed Secrets may not get
to run, leaving orphaned Secrets behind. If you really want everything gone,
delete the SecretSync objects first, wait for them to disappear, then
uninstall.

### Option B — release manifest

```bash
kubectl apply -f https://github.com/tabman83/kvsynk8s/releases/download/v0.1.0/install.yaml
```

This is the file `.github/workflows/release.yml` builds and attaches to each
GitHub Release: the CRD, RBAC, and the operator Deployment, with the image
already pinned to that release's tag.

### Option C — from source

```bash
git clone https://github.com/tabman83/kvsynk8s.git
cd kvsynk8s
make docker-build IMG=<your-registry>/kvsynk8s:dev
make docker-push IMG=<your-registry>/kvsynk8s:dev
make deploy IMG=<your-registry>/kvsynk8s:dev   # kustomize edit set image + kubectl apply -k
```

Whichever option you pick, before the operator can do anything useful you need
the Azure-side setup below. With Helm you pass `azure.clientID` and
`operator.queueURL` as values. With the manifest install you have to edit two
things it ships as placeholders:

- `config/rbac/service_account.yaml`'s
  `azure.workload.identity/client-id: "<SET-ME>"` annotation — the client ID
  of the managed identity you federate with this ServiceAccount.
- Something has to set `--queue-url`/`QUEUE_URL` on the `manager` container
  for the near-realtime path to be active at all. Without it the operator
  still works correctly through periodic reconciliation alone, just without
  the <60s propagation.

The operator installs into the `kvsynk8s` namespace as Deployment
`kvsynk8s-operator`, running under ServiceAccount `kvsynk8s-controller-manager`
(the name kustomize's `namePrefix: kvsynk8s-` produces from the kubebuilder
scaffold's `controller-manager` service account — this is the exact name your
workload identity federated credential's subject needs to reference).

## Azure setup

The operator does not create any Azure-side resources for you: the storage
queue, the Event Grid subscription, and the identity are one-time setup the
cluster operator does themselves (Azure CLI, Bicep/Terraform, whatever fits).
This is that setup, condensed to the commands.

Prerequisites: an AKS cluster with the OIDC issuer and workload identity
enabled (`az aks update --enable-oidc-issuer --enable-workload-identity`), an
Azure Key Vault (RBAC permission model), a Storage Account, and an Azure CLI
session with rights to create role assignments, an Event Grid subscription,
and a user-assigned managed identity.

```bash
RG=<resource-group> VAULT=<vault-name> SA=<storage-account> QUEUE=kvsynk8s-events
AKS=<cluster-name> NS=kvsynk8s SA_K8S=kvsynk8s-controller-manager

# Queue that receives Key Vault events
az storage queue create --name $QUEUE --account-name $SA --auth-mode login

# Route Key Vault events to the queue (Event Grid system topic)
az eventgrid event-subscription create \
  --name kvsynk8s \
  --source-resource-id $(az keyvault show -n $VAULT -g $RG --query id -o tsv) \
  --endpoint-type storagequeue \
  --endpoint $(az storage account show -n $SA -g $RG --query id -o tsv)/queueservices/default/queues/$QUEUE \
  --included-event-types Microsoft.KeyVault.SecretNewVersionCreated

# Identity for the operator + federation with its ServiceAccount
az identity create -n kvsynk8s-operator -g $RG
ISSUER=$(az aks show -n $AKS -g $RG --query oidcIssuerProfile.issuerUrl -o tsv)
az identity federated-credential create -n kvsynk8s -g $RG \
  --identity-name kvsynk8s-operator \
  --issuer $ISSUER --subject system:serviceaccount:$NS:$SA_K8S

# Least-privilege roles
PRINCIPAL=$(az identity show -n kvsynk8s-operator -g $RG --query principalId -o tsv)
az role assignment create --assignee $PRINCIPAL --role "Key Vault Secrets User" \
  --scope $(az keyvault show -n $VAULT -g $RG --query id -o tsv)
az role assignment create --assignee $PRINCIPAL --role "Storage Queue Data Message Processor" \
  --scope "$(az storage account show -n $SA -g $RG --query id -o tsv)/queueServices/default/queues/$QUEUE"
```

Two roles, nothing more: `Key Vault Secrets User` on the vault (read
secrets — never write, never list/manage keys or certs) and
`Storage Queue Data Message Processor` on the queue (read + delete messages —
never manage the queue itself).

Note the `SA_K8S` value above: it must match whatever ServiceAccount the
operator Deployment actually runs as (`kvsynk8s-controller-manager` out of the
box — see Install). If you rename it, update the federated credential's
subject to match.

Then install the operator (see Install above) and point it at the queue and
identity:

```bash
kubectl -n kvsynk8s rollout status deploy/kvsynk8s-operator
```

## The `SecretSync` custom resource

API group `kvsynk8s.io`, version `v1alpha1`, kind `SecretSync` (short name
`ss`), namespaced. One `SecretSync` maps one Key Vault secret to one
Kubernetes Secret in the same namespace.

### Spec

| Field | Type | Required | Default | Validation |
|---|---|---|---|---|
| `spec.vault.name` | string | yes | — | 3–24 chars, `^[a-zA-Z][a-zA-Z0-9-]+$` (Key Vault naming rules). Name of the vault, not its URI. |
| `spec.vault.secret` | string | yes | — | 1–127 chars, `^[a-zA-Z0-9-]+$`. Name of the secret inside the vault. |
| `spec.target.secretName` | string | no | the `SecretSync`'s own name | max 253 chars, `^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$` (DNS-1123 subdomain). Name of the Kubernetes Secret to create, in the same namespace as the `SecretSync`. |
| `spec.target.dataKey` | string | no | the vault secret name | max 253 chars, `^[-._a-zA-Z0-9]+$`. Key under the Secret's `.data` the value is stored at. |

There is no cross-namespace targeting: the target namespace is always the
`SecretSync`'s own namespace.

### Status (status subresource — set by the controller only)

| Field | Type | Meaning |
|---|---|---|
| `status.state` | `Pending` \| `InSync` \| `Failing` | High-level sync state. |
| `status.lastSyncTime` | timestamp | Time of the last successful write/verify. |
| `status.syncedVersion` | string | Key Vault secret version currently reflected in the Secret. An identifier, never a value. |
| `status.reason` | string | Machine-readable failure reason (see Troubleshooting below). Empty while `InSync`/`Pending`. |
| `status.message` | string | Human-readable detail — always built from vault name, secret name, namespace/name, and version only; never a secret value. |
| `status.observedGeneration` | int64 | Which `spec` generation this status describes. |

### Example

```yaml
apiVersion: kvsynk8s.io/v1alpha1
kind: SecretSync
metadata:
  name: demo-password
  namespace: demo
spec:
  vault:
    name: my-vault
    secret: demo-password
  target:
    secretName: demo-password   # optional, defaults to metadata.name
    dataKey: password           # optional, defaults to spec.vault.secret
```

```bash
kubectl -n demo get secretsync demo-password
# NAME            VAULT      SECRET          STATE    VERSION   LAST SYNC
# demo-password   my-vault   demo-password   InSync   a1b2c3…   2026-08-22T10:00:00Z

kubectl -n demo get secret demo-password -o jsonpath='{.data.password}' | base64 -d
```

### Rules worth knowing

- **First writer wins.** If the target Secret already exists and was not
  created by kvsynk8s (missing the `app.kubernetes.io/managed-by: kvsynk8s`
  label), the operator refuses to touch it: `Failing` / `TargetConflict`. Same
  outcome if two `SecretSync` objects in the same namespace target the same
  Secret name — the later one loses and goes `Failing` / `TargetConflict`.
- **Deleting a `SecretSync` deletes its managed Secret** (via a finalizer),
  but only the Secret it actually created — a `SecretSync` that lost a
  conflict never deletes anything.
- **Vault-side deletion/disable never deletes the Secret.** If Key Vault
  reports the secret gone or disabled, the `SecretSync` goes `Failing` with
  reason `SourceDeleted`/`SourceDisabled`, but the last synced value stays in
  the Kubernetes Secret untouched — workloads keep the last known good value.
- **Latest always wins.** A queue notification is only ever a trigger; the
  operator always re-reads the current value from Key Vault rather than
  trusting a version number carried in the notification. Duplicate or
  out-of-order notifications can never roll a secret back.

## Operator configuration

Flags (and the environment variable each one falls back to, where one
exists), all read in `cmd/main.go`:

| Flag | Env var | Default | What it does |
|---|---|---|---|
| `--queue-url` | `QUEUE_URL` | empty | Storage Queue URL that receives Key Vault Event Grid notifications. Optional — without it, the operator relies entirely on periodic reconciliation and still converges correctly, just without near-realtime propagation. |
| `--reconcile-interval` | `RECONCILE_INTERVAL` | `4h` | How often every `SecretSync` is fully reconciled against Key Vault regardless of notifications — the safety net for missed events, vault-side deletions, and in-cluster drift. Any value `time.ParseDuration` accepts (e.g. `30m`, `1h`). |
| `--azure-client-id` | `AZURE_CLIENT_ID` | empty | Client ID of the workload identity used to authenticate to Azure. `DefaultAzureCredential` already reads `AZURE_CLIENT_ID` from the environment on its own; this flag is an alternative way to set the same thing (it just sets the env var internally) for a Deployment that only supports passing command-line args. |
| `--metrics-bind-address` | — | `0` (disabled) | Address the metrics endpoint binds to. `:8080` for HTTP, `:8443` for HTTPS, or `0` to disable it. |
| `--metrics-secure` | — | `true` | Serve metrics over HTTPS. `--metrics-secure=false` for HTTP instead. |
| `--health-probe-bind-address` | — | `:8081` | Address the `/healthz`/`/readyz` endpoints bind to. |
| `--leader-elect` | — | `false` | Present for scaffold/manifest compatibility only. This operator always runs a single replica and never actually enables leader election regardless of this flag's value. |
| `--metrics-cert-path` / `--metrics-cert-name` / `--metrics-cert-key` | — | empty / `tls.crt` / `tls.key` | Directory and file names for the metrics server's TLS certificate. Left empty, controller-runtime generates a self-signed certificate (fine for development, not for production). |
| `--webhook-cert-path` / `--webhook-cert-name` / `--webhook-cert-key` | — | empty / `tls.crt` / `tls.key` | Same, for the webhook server. kvsynk8s registers no webhooks in v1, so this is unused scaffold surface. |
| `--enable-http2` | — | `false` | HTTP/2 is disabled by default on the metrics/webhook servers (mitigates the HTTP/2 Rapid Reset class of CVEs); set this to re-enable it. |

`--zap-*` flags (log level, encoding, stacktrace level, …) are also available,
from controller-runtime's standard zap flag set.

## Troubleshooting

Check `kubectl get secretsync -A` first — `status.state` and `status.reason`
are the starting point for everything below.

| `status.reason` | Meaning | What to check |
|---|---|---|
| `SecretNotFound` | The vault secret does not exist (and this `SecretSync` never had a prior successful sync). | Vault name and secret name in `spec.vault`; that the secret actually exists in that vault. |
| `AccessDenied` | The operator's identity cannot read the secret. | The `Key Vault Secrets User` role assignment on the vault; the workload identity federation (issuer, subject, namespace/ServiceAccount name all matching); the ServiceAccount's `azure.workload.identity/client-id` annotation. |
| `TargetConflict` | The target Kubernetes Secret already exists and was not created by kvsynk8s, or another `SecretSync` already claims the same namespace + `target.secretName`. | Whether the Secret name collides with something you did not intend it to; if two `SecretSync` objects are meant to target the same Secret, that is not supported — use one `SecretSync`. |
| `SourceDeleted` | The vault secret used to exist (this `SecretSync` had a prior synced version) and Key Vault now reports it missing. The Kubernetes Secret is left at its last synced value. | Whether the secret was deleted/purged in Key Vault on purpose; restore it there if not. |
| `SourceDisabled` | The vault secret exists but is administratively disabled. Same keep-last-known-good behavior as `SourceDeleted`. | Whether it was disabled on purpose; re-enable it in Key Vault if not. |
| `TransientError` | A retryable failure: network error, Key Vault throttling, an unclassified upstream error. | Usually nothing — the controller retries with exponential backoff on its own. Persisting for a long time can mean a networking problem (outbound access to `*.vault.azure.net` from the cluster) or sustained throttling. |

**Queue events not arriving / rotations not propagating within seconds:**
check that the Event Grid subscription still exists and points at the right
queue, that its `--included-event-types` still includes
`Microsoft.KeyVault.SecretNewVersionCreated`, and that the operator's identity
still has `Storage Queue Data Message Processor` on it. None of this leaves
the cluster stuck: periodic reconciliation (`--reconcile-interval`, default
4h) is the safety net and will converge every `SecretSync` to the current
vault value on its own — you lose the near-realtime property, not
correctness. Check `status.lastSyncTime` per `SecretSync` to see whether
reconciliation is still happening on schedule.

**No secret value ever appears in logs, status, or events, by design.**
Every message references a `SecretSync` only by vault name, secret name,
namespace/name, and Key Vault version — never by value. If you're
troubleshooting by reading logs, that is expected and is not something to
work around.

## Releasing

There are two kinds of release.

### Stable releases (manual, on purpose)

You decide the version and start the release yourself:

```bash
gh workflow run Release -f version=0.2.0
```

No `v` in front of the number, the workflow adds it. It creates the tag itself
at the end, pointing at the commit it built. Pushing a tag by hand still works
too:

```bash
git tag v0.2.0 && git push origin v0.2.0
```

A stable release is the only thing that moves the `:latest` image tag.

Pick the number by hand. There is no version file to edit anywhere:

| Bump | When |
|---|---|
| patch | bug fixes only, no new configuration |
| minor | new values, new behaviour, nothing existing breaks |
| major | anything that breaks existing users — most importantly a `SecretSync` CRD schema change that makes objects already in clusters invalid |

The CRD is a published API. People have `SecretSync` objects live in their
clusters, and `helm upgrade` applies the new schema to them, so a breaking
schema change is a major bump even if the Go code barely changed.

You can also cut a release candidate. Use a version with a suffix, like
`0.2.0-rc1`. It publishes everything, but leaves `:latest` alone and shows as a
prerelease.

### Dev builds (automatic, every merge)

Every merge to `master` publishes a dev build on its own. Nothing to run.

A dev build goes to the container registry and stops there. There is no git
tag and no GitHub Release for it, so the releases page only ever shows real
releases. The image and the chart are published normally, so you install one
exactly like any other version:

```bash
helm install kvsynk8s oci://ghcr.io/tabman83/charts/kvsynk8s \
  --version 0.1.1-dev.42 --namespace kvsynk8s --create-namespace
```

The version is the next patch plus the run number, so `0.1.1-dev.42` comes
after `v0.1.0`. It sorts below the real `0.1.1`, so it can never look newer
than the release it is heading towards. Dev builds never move `:latest`.

To find the version to install, look at the Actions run for the merge. The
version is printed in the run summary. Or list what has been published:

```bash
gh api /users/tabman83/packages/container/charts%2Fkvsynk8s/versions \
  --jq '.[].metadata.container.tags[]' | head
```

If you want the `install.yaml` of a dev build instead of the chart, download it
from the Artifacts section of that same Actions run.

They are dev builds. Do not run them in production.

## Development

See [CLAUDE.md](CLAUDE.md) for the build/lint/test commands and the
architecture this operator is actually built from.
