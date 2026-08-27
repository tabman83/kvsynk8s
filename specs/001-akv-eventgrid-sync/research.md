# Phase 0 Research: Event-Driven Azure Key Vault to Kubernetes Secret Sync

All Technical Context unknowns resolved. Sources: Microsoft Learn (Key Vault Event Grid integration, AKS Workload ID, Azure SDK for Go library index — azidentity/azqueue/azsystemevents), akv2k8s.io docs (prior art), controller-runtime docs, queried 2026-08-21 via Context7 / Microsoft Docs MCP.

## R1. Language and runtime

- **Decision**: Go (current stable, 1.25+). Chosen on task fit, not maintainer preference (revisited 2026-08-21 at the maintainer's request — an earlier draft picked C#/.NET for toolchain familiarity).
- **Rationale**: Kubernetes operators are Go's home turf. Every directly comparable project — akv2k8s itself, external-secrets-operator, azure-service-operator, azure-workload-identity — is Go on controller-runtime, so battle-tested patterns can be borrowed rather than reinvented, and outside contributors from that community can actually contribute. The Azure SDK for Go covers every integration first-party (R4, R5). Operationally Go wins for a cluster-resident daemon: small static binary in a distroless image (tens of MB, no runtime to patch), low memory per pod, fast start.
- **Alternatives considered**: C#/.NET 10 + KubeOps (rejected: KubeOps is a small community project compared to controller-runtime's SIG-maintained informer/workqueue machinery; larger images and memory footprint; no comparable ecosystem of operators to learn from); Rust + kube-rs (solid but a much smaller operator ecosystem, no first-party advantage for this task).

## R2. Operator framework

- **Decision**: controller-runtime with kubebuilder scaffolding (the reference stack maintained by Kubernetes SIG API Machinery).
- **Rationale**: gives CRD generation from Go types (`controller-gen`), cached informer-based watches, per-item workqueue with rate-limited requeue/backoff, finalizer helpers, status subresource handling, and owned-resource watches (the managed Secret is watched automatically → drift repair is event-driven, not poll-driven). `source.Channel` provides exactly the hook needed to inject external (queue) events into the reconcile queue. envtest gives reconciler tests against a real API server.
- **Alternatives considered**: Operator SDK (a wrapper over the same controller-runtime; extra layer, no benefit here — simplicity first); raw client-go watch loop (rejected: re-implements requeue/backoff/informer plumbing, more code to test).

## R3. Notification transport (queue choice)

- **Decision**: Azure Storage Queue as the Event Grid event handler destination.
- **Rationale**: Event Grid supports Storage Queues as a native handler; Storage Queues are the cheapest and operationally simplest queue in Azure, support `DefaultAzureCredential` auth (`Storage Queue Data Message Processor` role), at-least-once delivery with visibility timeout and `DequeueCount` for poison handling. The spec's clarification #1 mandates pull-based delivery; this is its simplest realization.
- **Alternatives considered**: Service Bus queue (rejected: more features than needed — sessions, ordering — at higher cost and setup complexity); Event Hubs (rejected: streaming semantics and checkpointing are overkill for low-volume notifications).

## R4. Queue message format and parsing

- **Decision**: Parse queue messages as Event Grid schema events using the Azure SDK for Go `azsystemevents` module (v1.0.0+); act only on `eventType == "Microsoft.KeyVault.SecretNewVersionCreated"` deserialized to `KeyVaultSecretNewVersionCreatedEventData`.
- **Rationale**: when Event Grid delivers to a Storage Queue, the message body is the EventGridEvent JSON (Base64-encoded in the queue message). The system-events module gives typed access to `VaultName`, `ObjectName`, `ObjectType`, `Version`, `ID` — everything needed to map to declarations, and no secret value ever transits the queue. Per Microsoft's consumption guidance: verify the event comes from an expected vault (check topic/vaultName), check eventType explicitly, ignore unknown fields/types.
- **Alternatives considered**: CloudEvents schema (viable, but Event Grid system topics default to Event Grid schema and the SDK types target it; no benefit for a single consumer); hand-rolled JSON parsing (rejected: fragile, SDK exists).
- **v1 behavior**: `SecretNearExpiry` / `SecretExpired` events are recognized and discarded without action (clarification #4).
- **Revision (event-path casing/typing fix)**: at the time of this fix the envelope was still `azsystemevents.EventGridEvent` and the event type came from the SDK constant (the envelope has since moved out of the SDK too — see the next bullet; the constant has not), but the `data` payload is no longer deserialized to `KeyVaultSecretNewVersionCreatedEventData`. Two properties of the real payload each killed the realtime path with the SDK type in place. First, `ObjectType` is `"Secret"` with a capital S, not `"secret"`, so the object-type guard discarded every genuine event; it now compares case-insensitively. Second, `KeyVaultSecretNewVersionCreatedEventData` types `NBF`/`EXP` as `*float32` and its generated `UnmarshalJSON` is strict, while [Microsoft's own sample](https://learn.microsoft.com/azure/event-grid/event-schema-key-vault) sends them as quoted strings (the property table on the same page calls them numbers — the docs contradict themselves, and no live-vault run has settled which one a vault actually emits). The documented payload therefore failed to unmarshal, `Parse` reported it malformed, and the listener deleted it. Since `Parse` reads only `VaultName`, `ObjectType`, `ObjectName` and `Version` — all strings — the `data` payload is now decoded into a small local struct holding exactly those four. That is narrower than the "hand-rolled JSON parsing" the alternatives above rejected: it is four fields the contract pins, and it makes the parser indifferent to how `NBF`/`EXP` are spelled (string, number, `null`, or absent) instead of betting the whole realtime path on a docs ambiguity.
- **Revision (CloudEvents delivery schema)**: the envelope is no longer `azsystemevents.EventGridEvent` either — it is a small local struct in `parser.go` holding `id`, both spellings of the event type and a raw `data`. Only the event-type *constant* (`azsystemevents.TypeKeyVaultSecretNewVersionCreated`) still comes from the SDK. The reason is that the Decision above assumed a delivery schema the operator, not us, chooses: `az eventgrid event-subscription create` takes `--event-delivery-schema` (`eventDeliverySchema` in ARM/Bicep), and `cloudeventschemav1_0` is a supported, documented choice for a Key Vault system topic delivering to a Storage Queue ([cloud-event-schema](https://learn.microsoft.com/azure/event-grid/cloud-event-schema)). A CloudEvents envelope has no `eventType` field at all — the type lives in `type` — so `EventGridEvent` left `EventType` nil for every such message and `Parse` rejected 100% of them as `ErrMalformedMessage`: the whole realtime path (FR-005) dead for that subscription, with the log blaming the message body instead of the subscription's schema setting, which hides the real cause. The event type is now the first non-empty of `eventType` (Event Grid schema) and `type` (CloudEvents v1.0); the *value* is the same string in both schemas, so one comparison against the SDK constant still covers both, and an envelope carrying neither spelling is the only malformed case. This settles the "CloudEvents schema" alternative above the other way round: it is no longer rejected — both delivery schemas are accepted, and the operator does not have to know which one this parser prefers. Nothing about `data` changes: it is byte-for-byte the same object in both schemas, so everything the previous bullet says about it still holds.

## R5. Azure authentication

- **Decision**: `azidentity.DefaultAzureCredential` (azidentity ≥ 1.3 for workload identity; current 1.14.x), which resolves to `WorkloadIdentityCredential` in-cluster via Microsoft Entra Workload ID federation. Same credential for Key Vault (`azsecrets`) and Storage Queue (`azqueue`).
- **Rationale**: platform-issued, short-lived tokens (constitution: Security Requirements); the workload-identity webhook injects the env/projected token automatically once the ServiceAccount is annotated. `DefaultAzureCredential` also lets developers run locally against az CLI credentials without code changes.
- **Azure roles (least privilege)**: `Key Vault Secrets User` scoped to the vault; `Storage Queue Data Message Processor` scoped to the queue.
- **Alternatives considered**: client secret / certificate in a K8s Secret (rejected: violates constitution — long-lived static credential); AKS identity bindings (preview) (rejected for v1: preview-only, beta SDK requirement).

## R6. "Near realtime" and queue polling cadence

- **Decision**: short-poll the Storage Queue with an adaptive delay — 1–2 s while messages are flowing, backing off to a max of ~30 s when idle. Batch-receive (up to 32 messages) for burst handling.
- **Rationale**: Storage Queues have no push; short-polling the queue is a few cheap storage GETs per minute and touches Key Vault zero times when nothing changed. This preserves the project's premise — no polling *of Key Vault* — while comfortably meeting the <60 s propagation target (Event Grid delivery is typically sub-second to seconds; queue poll adds ≤30 s worst case, ~1–2 s typical).
- **Alternatives considered**: fixed 1 s poll (rejected: needless constant traffic); long visibility-timeout tricks (unnecessary complexity).

## R7. Ownership and drift control of managed Secrets

- **Decision**: the managed Secret carries an `ownerReference` to its `SecretSync` (same namespace → garbage collection on CR delete as backstop to the finalizer) plus a `app.kubernetes.io/managed-by: kvsynk8s` label. The controller refuses to create/overwrite a Secret that exists without that ownership marker (FR-012) and reconciles drift on its own Secrets.
- **Rationale**: ownerReferences are the Kubernetes-native lifecycle mechanism; the label makes "managed by us" checkable before any write.
- **Alternatives considered**: annotations with a content hash only (kept as an addition — a hash annotation of the synced version avoids no-op writes — but not as the ownership signal).

## R8. Reconciliation strategy

- **Decision**: two triggers into one idempotent sync-engine path: (a) queue events mapped to matching declarations and injected into the reconcile queue via `source.Channel`, (b) controller-runtime timed requeue (`RequeueAfter`) of every `SecretSync` at the configured interval (default 4 h — clarification #3 originally said 1 h; amended to 4h in PR #15). The engine always reads the *latest* secret from Key Vault and writes only on change ("latest wins", FR-005).
- **Rationale**: single code path means event handling and reconciliation cannot diverge; latest-wins makes duplicates and out-of-order events harmless.
- **Alternatives considered**: version-targeted fetches from event payloads (rejected: enables rollback on stale events, violates FR-005).

## R9. Retry, backoff, failure isolation

- **Decision**: rely on Azure SDK built-in retry (with jittered exponential backoff) for transient HTTP/throttling, plus controller-runtime's rate-limited workqueue for per-declaration requeue-with-backoff on failure. Poison queue messages: after `DequeueCount` > 5, delete and log metadata (never content interpreted as value) — reconciliation covers whatever the message would have triggered.
- **Rationale**: per-declaration requeue isolates failures (FR-008); SDK retry handles 429s from bursts (SC-005 edge case).

## R10. Testing approach

- **Decision**: three layers, all automated and Azure-free.
  1. **Unit + envtest**: standard `go test`. Small interfaces (`SecretReader` for Key Vault, `QueueSource` for the queue) with hand-written fakes make the sync engine, parser, backoff, and ownership logic unit-testable. Reconciler and finalizer behavior tested with controller-runtime's envtest (real API server + etcd binaries, no cluster). Redaction tests plant sentinel values (`SENTINEL-...`) in fake secrets and assert captured log/status output never contains them (SC-004).
  2. **Integration** (build tag `integration`, requires Docker): testcontainers-go runs **Azurite** (Microsoft's official Storage emulator — validates the azqueue-backed QueueSource against the real wire protocol) and **Lowkey Vault** (community Key Vault emulator — validates the azsecrets-backed SecretReader). The HTTPS-stub fallback this section originally flagged as a fallback was **not needed**: Lowkey Vault's real challenge-auth flow works against the unmodified Go SDK once the client sets `DisableChallengeResourceVerification` (needed only because the emulator's address isn't a real `*.vault.azure.net` host, not because of anything Lowkey Vault itself gets wrong) — see `internal/azure/keyvault_integration_test.go`. One real finding from building this layer: Key Vault refuses `GetSecret` outright on a disabled secret (403 on real Key Vault, 404 on Lowkey Vault) instead of returning a normal body with `attributes.enabled=false`, so `keyvault.go`'s `secretIsDisabled` check the sync engine relied on could never actually fire; `classifyGetSecretError` now also matches the server's own wording for "disabled" regardless of status code, fixed and covered by both a unit test and the integration test.
  3. **E2E**: **kind** cluster (kubebuilder's native e2e target — chosen over k3s/k3d for scaffolding fit and ecosystem prevalence; either would work) running the built operator image against the emulators, asserting the full loop including the <60 s event-to-Secret path and drift repair. Event Grid itself is simulated by writing schema-correct messages to the queue — Event Grid's own delivery plus workload identity remain the only things verified manually against real Azure (quickstart validation).

     **The credential problem and how it was solved.** `cmd/main.go` always builds its Azure clients via `azidentity.NewDefaultAzureCredential` — there is no flag to swap in a different credential type, by design (constitution V: only the real, production credential path is ever exercised). That chain cannot obtain a token from anywhere reachable in a kind cluster with no real Azure AD tenant, confirmed by running it standalone in this environment: every sub-credential fails in ~3s with no viable fallback. Two standard, unmodified azidentity/MSAL behaviors unblock it without touching that code path: `AZURE_AUTHORITY_HOST` redirects `EnvironmentCredential`'s token requests to any HTTPS endpoint, and `AZURE_TENANT_ID=adfs` makes MSAL treat that authority as an ADFS deployment and skip the hardcoded call to real Azure AD's instance-discovery endpoint that would otherwise reject an unrecognized host. `test/e2e/testdata/authstub` is a small stand-in ADFS token endpoint built for this (see its package doc for the full explanation and the interactive verification it's based on). Neither emulator validates the resulting token's signature — Lowkey Vault does not check bearer tokens at all, and Azurite's `--oauth basic` mode (source-verified: `QueueTokenAuthenticator.js`) only checks claim shape/expiry/issuer-prefix/audience, never a real signature — so authstub's fabricated, unsigned token satisfies both. The other piece: `internal/azure/keyvault.go`'s `KVSYNK8S_KEYVAULT_TEST_ENDPOINT` env var override lets the suite point the real `SecretReader` at Lowkey Vault's non-`*.vault.azure.net` address; unset (the case for every real deployment), `clientFor`'s behavior is unchanged from before this override existed.

     **Status: resolved, and running in CI.** The suite is green and un-gated:
     `make test-e2e` runs 8 of 8 specs with nothing skipped.

     **The failure, and why it was misdiagnosed for so long.** For a long time
     this Context was opt-in behind `KVSYNK8S_E2E_EMULATORS=1`, because under
     `make test-e2e` the authstub container appeared "unreachable from pods over
     the cluster's own DNS", while the identical docker/kubectl sequence run by
     hand always worked. Alias naming, atomic deployment patching, resource
     limits, security-context parity, an in-cluster pre-flight check and
     recreating the container three times were all investigated and none of them
     changed anything — correctly, because none of them was the cause.

     **Nothing about DNS was wrong.** authstub was dying on startup. It is the
     only emulator container that both bind-mounts the shared cert directory and
     runs as a non-root uid (`gcr.io/distroless/static:nonroot`, `USER
     65532:65532`). `os.MkdirTemp` creates that directory `0700` and the private
     key was written `0600`, both owned by the host user, so uid 65532 could
     neither traverse the directory nor read the key; `ListenAndServeTLS`
     returned a permission error straight into `log.Fatal` and the container
     exited within milliseconds. Azurite hid this by running as root (its image
     sets no `USER`, so DAC never applies) and Lowkey Vault by mounting no certs
     at all. `docker run -d` exits 0 whether or not the process inside survives,
     so the suite went on to curl a container that was already gone: `curl` exit
     6 (could not resolve) and then exit 7 (resolved, nothing listening), which
     reads exactly like a network fault. `authstub/main.go` compounded it by
     logging "authstub listening on :9911" *before* calling
     `ListenAndServeTLS`, so `docker logs` showed a container that looked
     healthy and had already exited.

     **Why it was never reproducible by hand.** `AfterAll` deletes `certDir`, so
     any manual retry had to create its own certs — `mkdir` under the default
     umask 022 gives `0755` and `openssl` writes `0644`, i.e. world-readable.
     The bug could only ever occur under the suite. That is why "works by hand"
     kept pointing the investigation at the environment, and why the DNS theory
     was unfalsifiable rather than merely unproven.

     **The fix.** Make the cert directory and key readable by the container's
     uid (`0755`/`0644` — throwaway per-run self-signed material, in a temp dir,
     never a credential for anything), and assert after every `docker run -d`
     that the container is actually running, failing immediately with its own
     logs. That second part is the one that matters: it makes this class of bug
     impossible to misdiagnose again. The three-times recreate loop was deleted
     — it was a workaround for the wrong cause and simply repeated the same
     failure three times.

     **Two further defects surfaced only once these scenarios actually ran.**
     `go test` defaults to a 10 minute timeout and `make test-e2e` set none, so
     the un-gated suite died at 600s during teardown (now `E2E_TIMEOUT ?= 30m`).
     And teardown deadlocked: every SecretSync carries a finalizer only the
     running operator clears, while `make undeploy` deletes the operator and the
     namespace together, so a single survivor left the namespace stuck
     `Terminating` and `kubectl` blocked — a 30 minute hang with no failure
     message. SecretSyncs are now drained while the operator is still alive, and
     any survivor has its finalizer stripped so teardown degrades to a logged
     warning instead of a hang.

     **Still true from the original investigation**, and worth keeping: kind
     nodes resolve `.local` names via mDNS (RFC 6762), not ordinary DNS, so an
     alias ending in `.local` fails deterministically. That is why the alias is
     `authstub.e2e`.

  All three layers run in CI (GitHub Actions) on every PR, per constitution IV, including the emulator-backed sync-loop scenarios.
- **Alternatives considered**: mocking frameworks (unnecessary — Go interfaces + fakes are idiomatic and simpler); e2e against real Azure in CI (rejected: needs cloud credentials and a live vault in CI, flaky and slow; the emulator gap is covered by the manual quickstart run); k3s/k3d instead of kind (viable, but kubebuilder scaffolds kind-based e2e out of the box).
