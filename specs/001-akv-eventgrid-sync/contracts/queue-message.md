# Contract: Notification Queue Message

The operator's inbound contract with Azure. Messages arrive on an Azure Storage
Queue that the cluster operator configures as the destination of an Event Grid
event subscription on the Key Vault (system topic).

## Envelope

- Queue message body: Base64-encoded JSON of a single event (Event Grid encodes
  the event this way when delivering to Storage Queues). The Base64 wrapping
  belongs to the Storage Queue destination, not to the event schema, so it is
  the same whichever schema was chosen.
- **Both delivery schemas are accepted.** `az eventgrid event-subscription
  create` takes `--event-delivery-schema`, and both `eventgridschema` (the
  default) and `cloudeventschemav1_0` are supported, documented choices
  ([cloud-event-schema](https://learn.microsoft.com/azure/event-grid/cloud-event-schema)).
  The two envelopes carry the same event under different top-level names, and
  the Key Vault schema page publishes a sample of each:

  | Event Grid schema | CloudEvents v1.0 | Read by the parser? |
  | --- | --- | --- |
  | `id` | `id` | yes — logging/correlation |
  | `eventType` | `type` | yes — see resolution below |
  | `data` | `data` | yes — identical object in both |
  | `topic` | `source` | no |
  | `eventTime` | `time` | no |
  | `dataVersion`, `metadataVersion` | `specversion` | no |

- **Event type resolution:** the type is the first non-empty of `eventType`
  and `type`. The *value* is the same string in both schemas
  (`Microsoft.KeyVault.SecretNewVersionCreated`), so a single comparison
  against the SDK's event-type constant covers both. An envelope carrying
  neither spelling is malformed (rule 1).
- The envelope is decoded into a small local struct in `parser.go`, **not**
  into the Azure SDK's `azsystemevents.EventGridEvent`. That type models only
  the Event Grid spelling, so it leaves `EventType` nil for every CloudEvents
  message: using it made a CloudEvents subscription reject 100% of its messages
  as malformed, with the log blaming the message body rather than the
  subscription's schema setting. Only `id`, the two event-type spellings and a
  raw `data` are decoded there; the SDK still supplies the event-type constant.
- The `data` payload is likewise **not** unmarshalled as
  `KeyVaultSecretNewVersionCreatedEventData`: that type declares `NBF`/`EXP` as
  `*float32` and rejects the quoted-string form Microsoft's own sample uses,
  which would make the documented payload "malformed" and lose the event. Only
  the four strings below are decoded, into a second local struct.

## Example event (as delivered, after Base64 decoding)

Event Grid schema:

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
    "NBF": "1559081980",
    "EXP": "1559082102"
  }
}
```

The same event in the CloudEvents v1.0 schema. Note that `data` is unchanged —
that is why both forms produce the identical parse result:

```json
{
  "id": "6a6cbc37-...-63c2fc7b",
  "source": "/subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.KeyVault/vaults/<vault-name>",
  "subject": "my-app-password",
  "type": "Microsoft.KeyVault.SecretNewVersionCreated",
  "time": "2026-08-21T10:12:33.1234567Z",
  "data": {
    "Id": "https://<vault-name>.vault.azure.net/secrets/my-app-password/<version>",
    "VaultName": "<vault-name>",
    "ObjectType": "Secret",
    "ObjectName": "my-app-password",
    "Version": "<version>",
    "NBF": "1559081980",
    "EXP": "1559082102"
  },
  "specversion": "1.0"
}
```

`NBF`/`EXP` are shown the way the sample on
[learn.microsoft.com/azure/event-grid/event-schema-key-vault](https://learn.microsoft.com/azure/event-grid/event-schema-key-vault)
shows them — quoted strings — even though the property table on that same page
calls them numbers. Both spellings, plus `null` and the fields being absent
altogether, must parse: the fields used are only `VaultName`, `ObjectType`,
`ObjectName` and `Version`, and nothing else in the payload can make a message
malformed.

## Processing rules (FR-004, FR-005, FR-006; research R4, R8, R9)

1. Decode + parse. Malformed body ⇒ delete after logging message id/dequeue
   metadata only; never log the body (it is not expected to contain values, but
   the redaction rule is unconditional).
2. Act only when the resolved event type (`eventType`, else `type`) equals
   `"Microsoft.KeyVault.SecretNewVersionCreated"`
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
