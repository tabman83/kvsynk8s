# Feature Specification: Event-Driven Azure Key Vault to Kubernetes Secret Sync

**Feature Branch**: `001-akv-eventgrid-sync`

**Created**: 2026-08-21

**Status**: Draft

**Input**: User description: "I want to create a component that makes Azure Key Vault secrets available in Kubernetes in a simple and secure way. I want to resemble the https://akv2k8s.io/ project but instead of using a polling approach, I want to leverage the near realtime notifications of the Azure Key Vault via Event Grid."

## Clarifications

### Session 2026-08-21

- Q: How should Azure Key Vault change notifications be delivered to the component running inside the cluster? → A: Pull — Event Grid routes events to an Azure queue and the component reads from that queue; no inbound network exposure needed.
- Q: Who is allowed to create sync declarations — can any namespace declare a sync for any secret the component can read? → A: Any namespace; the cluster is a single trust boundary for v1. Scope is limited by the component's vault permissions and by Kubernetes RBAC on who may create declarations.
- Q: What should the default interval be for the periodic fallback reconciliation against Key Vault? → A: 1 hour (configurable); notifications are the primary path, reconciliation is a low-load safety net.
- Q: Should the component do anything with "secret near expiry" / "secret expired" events in v1? → A: No — ignore them; only "new version created" events trigger action. Expiry awareness is future work.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Make a Key Vault secret available in the cluster (Priority: P1)

A platform operator declares that a specific secret stored in Azure Key Vault should be available in a specific Kubernetes namespace under a chosen name. Shortly after, a native Kubernetes Secret exists in that namespace containing the current value of the Key Vault secret, and applications can consume it the standard Kubernetes way (env vars, volume mounts) without any awareness of Azure.

**Why this priority**: This is the core value of the product. Without the initial sync there is nothing to keep up to date. It is also independently valuable on its own — even without realtime updates, operators get a declarative bridge from Key Vault to cluster secrets.

**Independent Test**: Create a sync declaration pointing at an existing Key Vault secret; verify a Kubernetes Secret with the expected name, namespace, and value appears without any manual copying of the value.

**Acceptance Scenarios**:

1. **Given** a Key Vault secret exists and the component is running with read access to it, **When** an operator creates a sync declaration for that secret, **Then** a Kubernetes Secret with the declared name and namespace is created containing the secret's current value.
2. **Given** a sync declaration references a Key Vault secret that does not exist or is not readable, **When** the component processes it, **Then** no Kubernetes Secret is created, the declaration reports a failed status with a clear reason, and no secret value or partial value appears in any log or status message.
3. **Given** a synced Kubernetes Secret exists, **When** the operator deletes the sync declaration, **Then** the corresponding Kubernetes Secret is removed (cleanup follows the declaration's lifecycle).

---

### User Story 2 - Secret changes propagate in near realtime (Priority: P2)

A secret value is rotated in Azure Key Vault (a new version is created). Instead of waiting for a polling interval, the component reacts to the change notification published by Azure and updates the corresponding Kubernetes Secret within seconds.

**Why this priority**: This is the differentiator versus the polling approach of akv2k8s. It depends on User Story 1 (there must be a synced secret to update) but delivers the headline promise: near realtime propagation of rotations.

**Independent Test**: With a secret already synced, create a new version of it in Key Vault; measure the time until the Kubernetes Secret reflects the new value without restarting or manually triggering anything.

**Acceptance Scenarios**:

1. **Given** a secret is synced and change notifications are configured, **When** a new version of the secret is created in Key Vault, **Then** the Kubernetes Secret is updated to the new value in near realtime (target: under 60 seconds).
2. **Given** a change notification arrives for a secret that no sync declaration references, **When** the component receives it, **Then** the notification is acknowledged and ignored without error.
3. **Given** the same change notification is delivered more than once (at-least-once delivery), **When** the component processes the duplicates, **Then** the resulting Kubernetes Secret is identical to processing it once (updates are idempotent).
4. **Given** a notification arrives while the component is temporarily unable to reach Key Vault, **When** the fetch of the new value fails, **Then** the component retries with backoff until it succeeds, and the failure of this secret does not block updates of other secrets.

---

### User Story 3 - Recover from missed notifications (Priority: P3)

Notifications can be lost: the component may be down when one is delivered and its retry window lapses, the notification infrastructure may be misconfigured, or a secret may be deleted in Key Vault (deletions produce no change notification). The component periodically reconciles all sync declarations against Key Vault so the cluster never drifts permanently from the vault.

**Why this priority**: Notifications alone cannot guarantee convergence. This safety net is what makes the event-driven approach trustworthy in production, but it only matters once Stories 1 and 2 exist.

**Independent Test**: Stop the component, rotate a secret in Key Vault, discard the notification, restart the component; verify the Kubernetes Secret reaches the new value within one reconciliation interval.

**Acceptance Scenarios**:

1. **Given** a secret changed in Key Vault while the component was down and no notification was replayed, **When** the component starts (or the next periodic reconciliation runs), **Then** the Kubernetes Secret converges to the current Key Vault value.
2. **Given** a synced Kubernetes Secret is modified or deleted directly in the cluster by someone else, **When** the component detects the drift, **Then** it restores the Secret to match the declared Key Vault value.
3. **Given** notifications have not been received for longer than expected, **When** an operator inspects the component, **Then** the health/status information makes the degraded notification path visible while reconciliation keeps secrets converging.

---

### User Story 4 - Operator visibility and safe operations (Priority: P4)

An operator responsible for the cluster can tell, at any moment and per declaration, whether each secret is in sync, pending, or failing, and can troubleshoot from status information and logs alone — without ever seeing a secret value.

**Why this priority**: Observability is what makes the tool operable in production, but it supports the other stories rather than delivering standalone user value.

**Independent Test**: Break one sync on purpose (e.g., revoke read access to one secret); verify its status reports failing with a useful reason, other secrets keep syncing, and no log line or status field contains a secret value.

**Acceptance Scenarios**:

1. **Given** several sync declarations in mixed states, **When** the operator lists them, **Then** each shows its current state (in sync / pending / failing), the last successful sync time, and for failures a reason that names the secret by vault, name, and version — never by value.
2. **Given** any operation performed by the component (sync, retry, reconcile, error), **When** logs, status fields, or metrics are inspected, **Then** no secret value appears in any of them.

---

### Edge Cases

- A notification refers to a secret version that is already superseded by the time the component fetches it: the component always writes the latest value it reads from Key Vault, so out-of-order or stale notifications never roll a secret back.
- A secret is deleted or disabled in Key Vault (no change notification exists for deletion): reconciliation detects it; the sync declaration reports failing and the existing Kubernetes Secret is kept as-is (last known good) rather than deleted, so workloads do not lose credentials unexpectedly.
- Two sync declarations target the same Kubernetes Secret name in the same namespace: the component detects the conflict and marks the later declaration as failing with a clear reason instead of letting the two overwrite each other.
- A Kubernetes Secret with the declared output name already exists but was not created by the component: the component refuses to overwrite it and reports the conflict, to avoid destroying data it does not own.
- A message on the notification queue is malformed or refers to an unknown vault: it is discarded (dead-lettered or dropped after logging its metadata, never its content as a secret) and does not stall processing of later messages.
- A burst of notifications arrives (e.g., bulk rotation of many secrets): the component processes them without dropping any and without exceeding Key Vault read limits (throttling responses are retried with backoff).
- The component's own credentials expire or are rotated: it recovers by obtaining fresh platform-issued credentials without a restart or manual intervention.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Operators MUST be able to declare, per secret, which Azure Key Vault secret to sync and the name and namespace of the resulting Kubernetes Secret, using a declarative resource managed the standard Kubernetes way.
- **FR-002**: The system MUST create the declared Kubernetes Secret with the current Key Vault value when a declaration is added, and remove that Secret when its declaration is deleted.
- **FR-003**: The system MUST consume Azure Key Vault change notifications delivered by Event Grid to an Azure queue, and update the corresponding Kubernetes Secret when a new secret version is published. The component only makes outbound connections (to the queue, Key Vault, and the Kubernetes API); it MUST NOT require any inbound network endpoint.
- **FR-004**: On receiving a change notification, the system MUST fetch the secret value from Key Vault; the notification itself MUST be treated only as a trigger, never as a source of the value.
- **FR-005**: Notification processing MUST be idempotent: duplicate or out-of-order deliveries MUST converge to the latest Key Vault value and never roll a secret back to an older version.
- **FR-006**: The system MUST act only on messages read from the configured queue that parse as valid Key Vault change notifications for a declared vault; malformed messages and notifications for secrets no declaration references MUST be discarded without error. Access to the queue MUST use the same short-lived platform credentials as the rest of the system.
- **FR-007**: The system MUST periodically reconcile every declaration against Key Vault as a fallback, so that missed notifications, deletions in Key Vault, and drift introduced by direct edits to the Kubernetes Secret are all corrected without manual intervention. The reconciliation interval MUST be configurable, with a default of 1 hour.
- **FR-008**: Transient failures (network errors, Key Vault throttling, expired tokens) MUST be retried with backoff; a persistent failure of one secret MUST NOT block or delay the sync of other secrets.
- **FR-009**: Each declaration MUST expose an observable status — in sync, pending, or failing — including the time of the last successful sync and, on failure, a human-readable reason.
- **FR-010**: Secret values MUST never appear in logs, status fields, events, metrics, error messages, or any output other than the managed Kubernetes Secret itself. References to secrets in such outputs MUST use vault name, secret name, and version only.
- **FR-011**: The system MUST authenticate to Azure and to Kubernetes using short-lived, platform-issued credentials (e.g., workload identity); it MUST NOT require long-lived static credentials, and it MUST need only read access to the declared secrets and write access to the Secrets it manages.
- **FR-012**: The system MUST NOT overwrite Kubernetes Secrets it did not create; ownership conflicts (pre-existing Secret, or two declarations targeting the same output) MUST be reported as failures on the declaration.
- **FR-013**: When Key Vault reports a declared secret as deleted or disabled, the system MUST mark the declaration as failing while preserving the last synced Kubernetes Secret value, so running workloads are not broken by the vault-side change.

### Key Entities

- **Sync declaration**: The operator-facing record stating "this Key Vault secret should exist as that Kubernetes Secret". Attributes: source vault, source secret name, target Secret name, namespace, target data key. Carries the observable sync status.
- **Key Vault secret**: The external source of truth — a named, versioned secret value held in an Azure Key Vault. Never stored by the component beyond what is needed to write the Kubernetes Secret.
- **Kubernetes Secret**: The managed output object consumed by workloads. Owned and lifecycle-managed by the component only when it created it.
- **Change notification**: A message from Azure stating that a secret has a new version. Used purely as a trigger; carries identity of the secret, never its value. Azure also emits near-expiry/expired notifications; v1 discards these without action.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After rotating a secret in Key Vault, the corresponding Kubernetes Secret reflects the new value within 60 seconds in the normal (notification-driven) path, measured from the creation of the new secret version.
- **SC-002**: An operator can go from "nothing installed" to "first Key Vault secret available in the cluster" in under 15 minutes following only the project's own documentation.
- **SC-003**: With notifications completely unavailable, every synced secret still converges to the current Key Vault value within one reconciliation interval — no scenario leaves the cluster permanently out of sync.
- **SC-004**: Zero secret values appear in logs, statuses, events, or metrics across the entire test suite, verified by automated checks that scan all captured output for planted sentinel values.
- **SC-005**: A rotation burst of 100 secrets within one minute is fully propagated, with no notification lost and no failed secret blocking the others.
- **SC-006**: A deliberate failure of a single secret (revoked access) leaves all other secrets syncing normally and is visible in that declaration's status within one reconciliation interval.

## Assumptions

- Scope is Key Vault **secrets** only (as stated by the user). Certificates and keys, and akv2k8s-style transparent env injection into pods, are out of scope for this feature.
- One declaration maps one Key Vault secret to one Kubernetes Secret; richer shapes (multi-key secrets, many-to-one composition) are future work, not v1.
- The operator (or their infrastructure automation) is responsible for creating the Azure-side resources: the queue and the Event Grid subscription that routes Key Vault notifications to it. The project documents how, but provisioning Azure resources automatically is out of scope.
- Azure Key Vault only publishes change notifications for new versions, near-expiry, and expiry — not for deletions — which is why periodic reconciliation (FR-007) is a hard requirement rather than an optimization. In v1 only "new version created" notifications trigger action; near-expiry and expired notifications are discarded (expiry awareness is future work).
- Notification delivery is at-least-once with retry/backoff on the Azure side; the component is designed for duplicates and gaps rather than assuming exactly-once, in-order delivery.
- Notifications are pulled from an Azure queue, so the cluster needs only outbound connectivity to Azure. If the queue is unreachable or not configured, the component still works correctly via reconciliation, only losing the "near realtime" property.
- Kubernetes-standard access control protects the synced Secrets; protecting cluster access itself is outside this project's scope.
- The cluster is a single trust boundary in v1: any namespace may hold sync declarations, and any declared secret readable by the component's Azure identity will be synced. Limiting who can create declarations is delegated to Kubernetes RBAC; per-namespace or per-vault allowlisting is future work for multi-tenant clusters.
