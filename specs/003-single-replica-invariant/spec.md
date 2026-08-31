# Feature Specification: Single-Replica Invariant Made Real

**Feature Branch**: `003-single-replica-invariant`

**Created**: 2026-08-30

**Status**: Draft

**Input**: User description: "Make the operator's documented 'exactly one replica' invariant actually true at runtime, and correct the leader-election code that contradicts it. Today the operator hardcodes LeaderElection: false (cmd/main.go), the chart pins replicas: 1, and README plus charts/kvsynk8s/values.yaml both state that a second replica would reconcile the same Secrets uncoordinated. But the Deployment sets no update strategy, so the default RollingUpdate (maxSurge rounds up to 1, maxUnavailable rounds down to 0 at one replica) starts the new pod before terminating the old one. Every upgrade runs two uncoordinated managers and two queue listeners for a short window. No test covers it and no doc admits it. Separately, internal/events/listener.go declares NeedLeaderElection() false, which is the opposite of what it should be. If leader election were ever enabled, the listener would run on non-leader replicas where no controller drains the events channel; the 256-slot buffer fills, the send blocks, the poll loop stalls, and the held messages become visible again after Azure's default 30s visibility timeout. After DequeueCount passes the poison threshold of 5 they are deleted without ever being processed. A standby would silently discard rotation events. Scope: set the Deployment update strategy to Recreate in both install paths (the Helm chart and config/manager), so only one manager ever runs, and the kustomize/Helm equivalence check must still pass; change Listener.NeedLeaderElection() to true and update the comment to explain why a listener on a non-leader is unsafe, not merely unnecessary; document the operator's real failure behaviour in the README. Out of scope, and stated in the spec as a rejected alternative with reasons: running multiple replicas with leader election. Constraints: Constitution III (Simplicity First) — no new values, no new flags. The existing --leader-elect flag stays accepted and ignored for compatibility. Every behaviour change needs a test that fails without it (Constitution IV)."

## User Scenarios & Testing *(mandatory)*

### User Story 1 - An upgrade never runs two operators at once (Priority: P1)

A cluster operator upgrades kvsynk8s — with `helm upgrade`, or by re-applying `install.yaml`. Throughout the upgrade, at most one operator instance is running. The new instance starts only after the old one has fully stopped.

Today this is not what happens. Neither install path states an update strategy, so Kubernetes applies its default rolling update. At one replica that default starts the replacement pod first and only then terminates the old one, which means two uncoordinated operator instances run side by side for the length of the new pod's startup and readiness. Both hold a controller loop over the same SecretSync objects, and both poll the same Azure Storage Queue. The project's own documentation says this configuration is unsafe and must not happen, so today the shipped behaviour and the documented promise disagree.

**Why this priority**: This is the defect. Every other item in this feature is either a latent trap (Story 2) or documentation of a truth (Story 3). This is the only story where a user's cluster does something today that the project says it never does.

**Independent Test**: On a cluster running the operator, trigger an upgrade of the operator workload and watch its pods for the whole rollout. At no observation is more than one operator pod in a running state. Repeat for both install paths.

**Acceptance Scenarios**:

1. **Given** the operator is installed and running, **When** the operator workload is upgraded to a new image, **Then** the old instance reaches a fully terminated state before the replacement instance starts, and at no point during the rollout do two instances run at the same time.
2. **Given** a default chart render and the rendered manifest install, **When** their operator workloads are compared, **Then** both declare the same rollout behaviour, so an upgrade behaves identically whichever install method was used.
3. **Given** the equivalence check between the two install paths, **When** the rollout behaviour is changed in only one of them, **Then** the check fails and reports the difference.

---

### User Story 2 - A queue listener never runs where nothing consumes its events (Priority: P2)

A maintainer, or anyone who later enables leader election on this operator, gets a queue listener that runs only where the reconcile loop that consumes its output also runs.

The listener currently declares that it does not need leadership, which means it would start on every replica, including ones where the reconcile loop is not running. On such a replica nothing drains the channel the listener writes matched events into. Once the channel's buffer is full, the listener's send blocks, its poll loop stops mid-batch, and the messages it already pulled off the queue stay held with no deadline. Azure returns those messages to the queue after its default visibility window, each redelivery raises the message's dequeue count, and once that count passes the operator's poison threshold the message is deleted without ever being acted on. The visible result would be rotation events silently disappearing, with the operator falling back to the multi-hour periodic reconcile — and no failure signal to show why.

**Why this priority**: Nothing is broken today, because leader election is never enabled. But the declaration is exactly backwards, its comment explains the wrong reason ("not necessary" rather than "not safe"), and the failure it would cause is silent data staleness rather than a crash. It is a trap laid for the next person who touches this.

**Independent Test**: Start the operator in a configuration where the reconcile loop is gated on leadership and the instance does not hold leadership, then confirm the queue listener does not start and no queue messages are consumed by that instance.

**Acceptance Scenarios**:

1. **Given** the operator process, **When** it runs an instance whose reconcile loop is not active, **Then** the queue listener on that instance does not start and does not consume messages from the queue.
2. **Given** the operator process running normally as the single, active instance, **When** the queue is configured, **Then** the queue listener starts and behaves exactly as it does today — this change alters nothing about the shipped single-instance configuration.
3. **Given** the code that declares the listener's leadership requirement, **When** a maintainer reads it, **Then** the stated reason is the safety consequence described above, not merely that leadership is currently unused.

---

### User Story 3 - An operator can find out what happens when kvsynk8s is down (Priority: P3)

A cluster operator evaluating or running kvsynk8s reads the documentation and learns, without reading source code, what actually happens while the operator is not running: existing synced Kubernetes Secrets keep existing and keep working, pods that mount them are unaffected, and the only cost is that a Key Vault change does not reach the cluster until the operator is back. They also learn that upgrades now include a short gap with no operator running, and that this gap is the deliberate trade for never running two.

**Why this priority**: It changes no behaviour, but without it Story 1 looks like a regression ("you made upgrades slower") instead of a correctness fix, and the existing single-replica notes read as an unexplained limitation rather than a reasoned choice.

**Independent Test**: Give the documentation to someone who has not read the code and ask them what happens to their applications if the operator pod is deleted, and how long an upgrade leaves them without an operator. They can answer both from the docs alone.

**Acceptance Scenarios**:

1. **Given** the project documentation, **When** a reader looks for the operator's failure behaviour, **Then** they find a plain statement that the operator is not on any request path, that already-synced Secrets and the pods using them are unaffected while it is down, and that convergence resumes on its own when it restarts.
2. **Given** the project documentation, **When** a reader looks at the single-replica and upgrade notes, **Then** they find that an upgrade briefly leaves no operator running, why that is preferred over two running at once, and what the practical consequence is (a delayed sync, never a broken one).
3. **Given** the existing statements about replica count in the chart values and the README, **When** they are read after this change, **Then** they describe the same behaviour the shipped manifests actually produce, with no remaining claim that is true only in principle.

---

### Edge Cases

- **A rotation lands during the upgrade gap.** The queue message is not lost: nothing consumes it while no operator runs, so it waits in the queue and is picked up by the new instance. If it is lost anyway (queue unconfigured, message expired, event never delivered), the periodic reconcile still converges the Secret. Convergence is delayed, never abandoned.
- **The new operator instance fails to start after the old one stopped.** There is now no operator running at all, where the previous rolling behaviour would have kept the old instance alive. Existing Secrets are untouched and applications keep working; the visible symptom is that Key Vault changes stop reaching the cluster. This is an accepted consequence, and the documentation must say so rather than leave it to be discovered.
- **A user manually scales the operator workload above one replica.** Nothing in this feature prevents that, and it remains unsupported and undocumented as a supported mode. The behaviour is the same as today.
- **A user's own manifest passes the leader-election flag.** The flag is still accepted and still ignored, so their manifest keeps working unchanged.
- **A node is drained while the operator runs on it.** Only one instance exists, so the workload is unavailable until it is rescheduled. Same trade as the upgrade gap, same consequence.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Both install paths (the Helm chart and the manifest install) MUST declare a rollout behaviour that fully terminates the running operator instance before starting its replacement, so that two operator instances never run at the same time during an upgrade.
- **FR-002**: The two install paths MUST declare the same rollout behaviour, and the existing equivalence check between them MUST cover it, so that changing one without the other fails CI.
- **FR-003**: The rollout behaviour MUST NOT be configurable. No new chart value, no new command-line flag, and no new environment variable are introduced by this feature (Constitution III).
- **FR-004**: The queue listener MUST NOT be eligible to run on an operator instance whose reconcile loop is not running, because nothing on such an instance consumes the events it produces.
- **FR-005**: The operator MUST continue to accept the existing leader-election flag and continue to ignore it, so hand-written manifests that pass it keep working.
- **FR-006**: This feature MUST NOT change any observable behaviour of the operator in its shipped single-instance configuration: same reconcile loop, same queue consumption, same metrics, same status handling.
- **FR-007**: The documentation MUST state the operator's failure behaviour: it is not on any request path; while it is down, already-synced Kubernetes Secrets and the workloads consuming them are unaffected; the only cost is delayed propagation of Key Vault changes; and convergence resumes without intervention when it restarts.
- **FR-008**: The documentation MUST state that an upgrade now leaves a short window with no operator running, that this is deliberate, and what the consequence is.
- **FR-009**: Existing statements about replica count and leader election in the chart values, the README, and the shipped manifests MUST be brought into agreement with what the manifests actually produce, with no claim left that holds only in principle.
- **FR-010**: Each behaviour change in this feature MUST be covered by an automated test that fails if the change is reverted (Constitution IV). Where a change is a declaration with no logic and cannot be meaningfully tested, the pull request MUST say so explicitly.
- **FR-011**: The code comment explaining the listener's leadership requirement MUST state the safety consequence of running a listener without a consumer, not merely that leadership is currently unused.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Across a full upgrade of the operator, observed continuously from start to finish, the maximum number of simultaneously running operator instances is 1 — for both install methods.
- **SC-002**: A change to the rollout behaviour in one install path but not the other is caught automatically before merge, with a message naming the difference.
- **SC-003**: On a cluster where the operator image is already present, the gap between the old instance stopping and the new instance being ready is under 60 seconds, so a normal upgrade does not risk exceeding the propagation expectations the project already documents.
- **SC-004**: A reader who has not seen the source code can answer, from the documentation alone: what happens to their applications while the operator is down, and roughly how long an upgrade leaves them without an operator.
- **SC-005**: The configuration surface is unchanged: the number of chart values and the number of operator flags are identical before and after this feature.
- **SC-006**: Reverting any single behaviour change in this feature causes at least one automated test to fail.
- **SC-007**: Zero SecretSync objects change state as a result of this feature on an existing installation: after upgrading, every previously in-sync declaration is still in sync, with no re-sync required by hand.

## Out of Scope

### Rejected alternative: multiple replicas with leader election

Running two or more operator replicas coordinated by a leadership lease was considered as the way to remove the upgrade gap entirely, and rejected. Recorded here so it is not re-proposed without new evidence:

- **It buys no throughput.** Leadership makes one instance active and the rest idle. Only the active instance reconciles, so nothing is processed faster.
- **It multiplies copies of secret material.** Each standby still holds a cache of the managed Kubernetes Secrets, so every extra replica is another process holding secret values in memory for no functional benefit. That works against the project's first principle (Constitution I: secret values live in as few places as possible).
- **It makes the operator less available exactly when availability matters.** Losing the lease terminates the instance. A control-plane hiccup that today costs one failed reconcile and a backoff would instead restart every replica, during the moment the cluster is already degraded.
- **The availability it buys is narrow.** Failover on a lease takes about as long as restarting a pod on a healthy node. The only case it genuinely improves is losing the whole node, where eviction is slow — a real scenario, but one that only matters if a Key Vault rotation lands inside that window and the previous version is revoked immediately.
- **It is not a small change.** It would need lease permissions the operator does not currently hold, the listener gating from Story 2, a fix for the per-instance metrics collector (which lists at scrape time and would publish a full set of state gauges from every replica, doubling any aggregate over them), plus a disruption budget, anti-affinity so the replicas do not share a node, a new replica-count value, and a test that kills the active instance.

**What would change this decision**: a concrete user or incident where the node-loss window caused a real problem — for example a rotation policy that revokes the previous secret version immediately, combined with a cluster where node loss is common. Absent that, Constitution III applies: do not build it.

### Also out of scope

- Changing the default periodic reconcile interval, or any other tuning of convergence timing.
- Adding a pod disruption budget, anti-affinity, or topology spread constraints.
- Changing what the operator does on shutdown beyond what the existing termination grace period already provides.
- Any change to the SecretSync API. This feature does not touch the published CRD, so it is not a breaking change for users.

## Assumptions

- "Fully terminates before starting the replacement" is taken to mean Kubernetes' `Recreate` deployment strategy, as named in the feature request. An equivalent rolling configuration that permits no surge would satisfy the same requirement; `Recreate` is chosen because it states the intent directly and cannot be defeated by percentage rounding at one replica.
- The upgrade gap is acceptable to users. This is the core trade of the feature: a short, predictable window with no operator is preferred over a short window with two uncoordinated ones. The rationale is that the operator is not on any request path, so its absence delays convergence rather than breaking anything.
- The equivalence check between the Helm render and the kustomize output is the right place to prevent the two install paths from drifting, since it already compares the operator workload's other fields field by field.
- Users who wrote their own Deployment manifests are not covered by this change and are not expected to be. The flag they may pass keeps working; how their own manifest rolls out remains theirs to set.
- No release-version decision is made here. This feature changes no published API and no configuration surface, so it does not on its own require a major or minor bump; the version for whatever release carries it is decided separately, as the project's release process requires.
