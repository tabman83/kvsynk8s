# Contract: Notification Queue Message

The operator's inbound contract with Azure. Messages arrive on an Azure Storage
Queue that the cluster operator configures as the destination of an Event Grid
event subscription on the Key Vault (system topic, Event Grid schema).

## Envelope

- Queue message body: Base64-encoded JSON of a single Event Grid schema event
  (Event Grid encodes the event this way when delivering to Storage Queues).
- Parsed with the Azure SDK for Go `azsystemevents` module: unmarshal the
  EventGridEvent envelope, then the data payload as
  `KeyVaultSecretNewVersionCreatedEventData`.

## Example event (as delivered, after Base64 decoding)

```json
{
  "id": "6a6cbc37-...-63c2fc7b",
  "topic": "/subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.KeyVault/vaults/<vault-name>",
  "subject": "my-app-password",
  "eventType": "Microsoft.KeyVault.SecretNewVersionCreated",
  "eventTime": "2026-08-21T10:12:33.1234567Z",
  "dataVersion": "1",
  "metadataVersion": "1",
  "data": {
    "Id": "https://<vault-name>.vault.azure.net/secrets/my-app-password/<version>",
    "VaultName": "<vault-name>",
    "ObjectType": "Secret",
    "ObjectName": "my-app-password",
    "Version": "<version>",
    "NBF": null,
    "EXP": null
  }
}
```

## Processing rules (FR-004, FR-005, FR-006; research R4, R8, R9)

1. Decode + parse. Malformed body ⇒ delete after logging message id/dequeue
   metadata only; never log the body (it is not expected to contain values, but
   the redaction rule is unconditional).
2. Act only when `eventType == "Microsoft.KeyVault.SecretNewVersionCreated"`
   **and** `data.ObjectType` equals `secret` **case-insensitively** — Azure
   sends `"Secret"` with a capital S, so a case-sensitive compare discards
   every real event. `SecretNearExpiry` / `SecretExpired` and all other object
   types ⇒ delete without action (v1 scope, clarification #4).
3. Match `(data.VaultName, data.ObjectName)` against all `SecretSync` specs.
   Both names are compared **case-insensitively**: Key Vault names are
   case-insensitive and case-preserving, so the event carries whatever casing
   the object was created with. No match ⇒ delete, done.
   If the `SecretSync` list itself fails (cache error), the message is left
   on the queue — not deleted — so the visibility timeout redelivers it on a
   later poll instead of the event being lost (`listener.go`,
   `matchingSecretSyncs` error path). Poison handling (rule 6) still bounds
   how often a message can come back this way.
4. On match: trigger the sync engine per matching declaration. The engine
   fetches the **latest** secret from Key Vault — `data.Version` is used for
   logging/correlation only, never as the fetch target (latest-wins; stale or
   out-of-order events can never roll a secret back).
5. Delete the queue message only after the triggered syncs have been accepted
   for processing (sync failures are retried via controller requeue, not by
   redelivering the queue message).
6. Poison handling: `DequeueCount > 5` ⇒ delete + log metadata; the periodic
   reconciliation covers whatever the message would have triggered.

## Non-goals

- The message never carries a secret value; the queue and Event Grid are
  untrusted for confidentiality of values by design — only Key Vault serves
  values (FR-004).
- No inbound webhook variant exists in v1 (clarification #1).
