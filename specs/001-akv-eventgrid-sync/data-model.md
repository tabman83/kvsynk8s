# Data Model: Event-Driven Azure Key Vault to Kubernetes Secret Sync

Three entities. The only persistent state is in the Kubernetes API (the `SecretSync` custom resource and the managed Secret); the queue message is transient transport.

## SecretSync (custom resource, namespaced)

API group `kvsynk8s.io`, version `v1alpha1`, kind `SecretSync`. See [contracts/secretsync-crd.yaml](contracts/secretsync-crd.yaml) for the full schema.

### Spec (operator-declared, immutable intent)

| Field | Type | Required | Validation | Notes |
|-------|------|----------|------------|-------|
| `spec.vault.name` | string | yes | 3–24 chars, alphanumeric + hyphen (Key Vault naming rules) | Source vault |
| `spec.vault.secret` | string | yes | 1–127 chars, alphanumeric + hyphen | Source secret name |
| `spec.target.secretName` | string | no | DNS-1123 subdomain | Output Secret name; defaults to the CR's own name |
| `spec.target.dataKey` | string | no | valid Secret data key | Key inside the Secret's `data`; defaults to the source secret name |

Target namespace is always the CR's namespace (namespaced resource; single trust boundary per clarification #2 — no cross-namespace targeting in v1).

### Status (controller-owned, status subresource)

| Field | Type | Notes |
|-------|------|-------|
| `status.state` | enum `Pending` \| `InSync` \| `Failing` | FR-009 |
| `status.lastSyncTime` | timestamp | Last successful write/verify |
| `status.syncedVersion` | string | Key Vault secret version currently reflected — safe to expose (identifier, not value) |
| `status.reason` | string | Machine-readable failure reason (e.g. `SecretNotFound`, `AccessDenied`, `TargetConflict`, `SourceDeleted`) |
| `status.message` | string | Human-readable detail; MUST never contain a secret value (FR-010) |
| `status.observedGeneration` | int | Standard convention: which spec generation the status describes |

### State transitions

```text
(created) ──► Pending ──sync ok──► InSync ──vault change + sync ok──► InSync (new version)
                 │                    │
                 └──sync fails──► Failing ◄──sync fails (retry w/ backoff keeps trying)──┘
Failing ──cause resolved, sync ok──► InSync
(CR deleted) ──finalizer──► managed Secret deleted ──► CR removed
```

Special rule (FR-013): source secret deleted/disabled in the vault ⇒ `Failing` with reason `SourceDeleted`/`SourceDisabled`, but the managed Secret keeps its last synced value.

### Identity & uniqueness

- CR identity: standard namespace/name.
- Uniqueness rule (FR-012): at most one `SecretSync` may own a given target Secret (namespace + `target.secretName`). The controller enforces this at reconcile time — first writer wins, later declarations go `Failing` with reason `TargetConflict`. (Admission webhook rejected for v1: simplicity first.)

## Managed Kubernetes Secret

The output object. Written exclusively by the secret writer (`internal/sync/writer.go` — the single value-carrying code path, constitution I).

| Property | Value |
|----------|-------|
| `type` | `Opaque` |
| `metadata.name` / `namespace` | from `SecretSync` target rules above |
| `metadata.labels` | `app.kubernetes.io/managed-by: kvsynk8s` (ownership marker checked before every write) |
| `metadata.annotations` | `kvsynk8s.io/vault`, `kvsynk8s.io/secret`, `kvsynk8s.io/version` (synced KV version — enables no-op detection without reading values) |
| `metadata.ownerReferences` | the owning `SecretSync` (GC backstop to the finalizer) |
| `data[<dataKey>]` | the secret value (base64, as all Secret data) |

Write rules:

- Never create/update a Secret lacking the `managed-by` label unless creating it fresh (FR-012).
- Skip the write when the annotated version already matches the latest Key Vault version (idempotency, FR-005).
- Delete only via finalizer/GC when the owning CR is deleted (FR-002).

## Change notification (transient)

An Event Grid schema event pulled from the Azure Storage Queue. See [contracts/queue-message.md](contracts/queue-message.md).

Fields used: `eventType` (filter: only `Microsoft.KeyVault.SecretNewVersionCreated` acts), `data.VaultName`, `data.ObjectName`, `data.ObjectType` (filter: `secret`), `data.Version`, `id` (logging/correlation). Carries no secret value by design.

Mapping: `(VaultName, ObjectName)` → all `SecretSync` resources across namespaces whose `spec.vault` matches (case-insensitive on vault name). No match ⇒ delete message, done (FR-006). Match ⇒ trigger `SyncEngine` for each; the engine fetches the **latest** version from Key Vault, not `data.Version` (latest-wins, research R8).

Scale assumptions: hundreds of `SecretSync` CRs, bursts up to 100 events/min (SC-005), queue batch size 32.
