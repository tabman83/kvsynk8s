// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

// Package events implements the Event Grid -> Storage Queue notification
// path (contracts/queue-message.md, data-model.md "Change notification"):
// Parse turns one raw queue message into a ParsedEvent or a clean discard,
// and Listener (listener.go) drives the poll/match/dispatch/delete loop.
//
// CONSTITUTION I (non-negotiable): a queue message never carries a secret
// value by design (contracts/queue-message.md "Non-goals"), but the
// redaction rule applies unconditionally regardless of that design intent --
// nothing in this package ever logs or echoes raw message body content, only
// identifiers (event id, vault name, secret name, version) and fixed, static
// text.
package events

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/eventgrid/azsystemevents"
)

// ErrMalformedMessage is the sentinel a caller checks with errors.Is when
// Parse cannot make sense of a message at all (contracts/queue-message.md
// rule 1): invalid Base64, invalid JSON envelope, or a missing/invalid data
// payload for an otherwise-actionable event type. The caller's response is
// always the same regardless of which of these applies: delete the message
// after logging its id and dequeue metadata only, never the body.
var ErrMalformedMessage = errors.New("events: malformed queue message")

// ParsedEvent is the minimal information the listener needs to match a
// notification against SecretSync declarations and to log/correlate it.
// Never carries a secret value: VaultName/SecretName are identifiers, and
// Version is Key Vault's version identifier, not content.
type ParsedEvent struct {
	// ID is the Event Grid event id, for logging/correlation only.
	ID string
	// VaultName is compared case-insensitively against SecretSync
	// spec.vault.name (data-model.md).
	VaultName string
	// SecretName is compared case-insensitively against SecretSync
	// spec.vault.secret: Key Vault object names are case-insensitive and
	// case-preserving, so the casing the event carries is whatever the object
	// was created with and need not match the casing in the spec.
	SecretName string
	// Version is data.Version from the event: logging/correlation only. The
	// sync engine always fetches the latest version from Key Vault instead
	// of using this value as a fetch target (contracts/queue-message.md rule
	// 4, "latest wins" -- FR-005).
	Version string
}

// eventEnvelope is the handful of top-level fields Parse actually reads,
// decoded into a local struct instead of into azsystemevents.EventGridEvent.
//
// Event Grid lets whoever creates the event subscription choose the delivery
// schema, and CloudEvents v1.0 is a supported, documented choice
// (learn.microsoft.com/azure/event-grid/cloud-event-schema, selected with
// `--event-delivery-schema cloudeventschemav1_0`). The two envelopes carry the
// same event under different top-level names, and the Key Vault event schema
// page publishes a sample of each: the event type is `eventType` in the Event
// Grid schema and `type` in CloudEvents, while everything else that differs
// (`topic`/`source`, `eventTime`/`time`, `dataVersion`/`metadataVersion` vs
// `specversion`) is not read here at all. The `data` object is byte-for-byte
// identical between the two.
//
// azsystemevents.EventGridEvent models only the Event Grid spelling, so it left
// EventType nil for every CloudEvents message and Parse rejected all of them
// with ErrMalformedMessage: an operator who picked CloudEvents when creating
// the subscription lost 100% of the realtime path (FR-005) and got a log
// blaming the message body instead of the subscription's schema setting, which
// hides the real cause. Same class of silent death as the ObjectType and
// NBF/EXP bugs below, one level further out in the message, and the same fix:
// stop leaning on an SDK type that models a shape we do not control, and decode
// the few fields we read ourselves. The event-type constant still comes from
// the SDK.
type eventEnvelope struct {
	ID string `json:"id"`
	// EventType is the Event Grid schema spelling of the event type, Type the
	// CloudEvents one. A real message carries exactly one of them; Parse
	// resolves whichever is present.
	EventType string `json:"eventType"`
	Type      string `json:"type"`
	// Data stays raw so nothing inside it can make the envelope fail to
	// decode -- see the comment on the data struct in Parse. It also replaces
	// the `envelope.Data.([]byte)` type assertion the SDK envelope required,
	// which depended on an SDK implementation detail (its generated
	// UnmarshalJSON happening to stash raw bytes in an `any` field).
	Data json.RawMessage `json:"data"`
}

// Parse decodes and interprets one raw queue message body (still
// Base64-encoded, exactly as azure.QueueMessage.Text carries it) into a
// ParsedEvent, or reports that the message should be discarded. Both delivery
// schemas Event Grid can be configured to send are accepted (see
// eventEnvelope); the Base64 wrapping is a property of the Storage Queue
// destination, not of the schema, so it is the same either way.
//
// Return contract (contracts/queue-message.md "Processing rules"):
//   - Malformed body of any kind -> (nil, error) wrapping ErrMalformedMessage.
//     The error text is always fixed, static wording -- never anything
//     derived from body itself -- so it can never echo message content
//     (constitution I).
//   - event type != "Microsoft.KeyVault.SecretNewVersionCreated" (rule 2:
//     SecretNearExpiry, SecretExpired, or anything else/unknown) -> (nil,
//     nil): a clean discard, not an error.
//   - data.ObjectType is not "secret" case-insensitively (rule 2; Azure sends
//     "Secret") -> (nil, nil): same clean discard.
//   - Otherwise -> (&ParsedEvent{...}, nil).
func Parse(body []byte) (*ParsedEvent, error) {
	decoded, err := base64.StdEncoding.DecodeString(string(body))
	if err != nil {
		return nil, fmt.Errorf("%w: invalid base64 encoding", ErrMalformedMessage)
	}

	var envelope eventEnvelope
	if err := json.Unmarshal(decoded, &envelope); err != nil {
		return nil, fmt.Errorf("%w: invalid event grid envelope", ErrMalformedMessage)
	}

	// Resolve the event type across both delivery schemas: whichever spelling
	// carries it wins, and only an envelope carrying neither is genuinely
	// unusable. The static wording names both spellings on purpose -- an error
	// naming only eventType points an operator who chose CloudEvents at the
	// message when the answer is in the subscription.
	eventType := envelope.EventType
	if eventType == "" {
		eventType = envelope.Type
	}
	if eventType == "" {
		return nil, fmt.Errorf("%w: missing eventType or type", ErrMalformedMessage)
	}

	id := envelope.ID

	// Rule 2: act only on SecretNewVersionCreated; every other type
	// (SecretNearExpiry, SecretExpired, anything unrecognized) is out of v1
	// scope and discarded without error. The type value is the same string in
	// both schemas, so one comparison against the SDK constant covers both.
	if eventType != azsystemevents.TypeKeyVaultSecretNewVersionCreated {
		return nil, nil
	}

	// A `data` that is absent or explicitly null is a malformed message for an
	// otherwise-actionable event type (rule 1). json.RawMessage happily accepts
	// a JSON null as the four bytes `null`, so it has to be rejected here
	// rather than being left to decode into an all-nil data struct.
	dataBytes := bytes.TrimSpace(envelope.Data)
	if len(dataBytes) == 0 || bytes.Equal(dataBytes, []byte("null")) {
		return nil, fmt.Errorf("%w: missing event data", ErrMalformedMessage)
	}

	// The data payload is decoded into this local struct on purpose, not into
	// azsystemevents.KeyVaultSecretNewVersionCreatedEventData. That SDK type
	// (v1.0.0, models.go) declares NBF and EXP as *float32 and its generated
	// UnmarshalJSON is strict, but the published sample for this very event
	// (learn.microsoft.com/azure/event-grid/event-schema-key-vault) spells them
	// as JSON strings -- "NBF":"1559081980" -- while the property table on the
	// same page calls them numbers. Decoding the documented sample with the SDK
	// type therefore fails with `cannot unmarshal string into Go value of type
	// float32`, Parse returns ErrMalformedMessage, and the listener deletes the
	// message as malformed: the rotation is lost until the 4h periodic
	// reconcile. That is the same silent death of FR-005's realtime path as the
	// ObjectType casing bug below, one step earlier.
	//
	// Parse reads neither NBF nor EXP, so it must not be able to fail on them.
	// These four strings are everything the listener needs, and any spelling of
	// NBF/EXP -- string, number, null, absent -- is simply ignored. Field names
	// are the documented PascalCase ones; encoding/json matches object keys
	// case-insensitively, so a camelCase variant would bind too. This object is
	// identical in both delivery schemas, so it is decoded the same way for
	// either. The event-type constant still comes from the SDK.
	var data struct {
		VaultName  *string
		ObjectType *string
		ObjectName *string
		Version    *string
	}
	if err := json.Unmarshal(dataBytes, &data); err != nil {
		return nil, fmt.Errorf("%w: invalid event data", ErrMalformedMessage)
	}

	// Rule 2 (continued): only secrets, never certificates/keys, even though
	// the eventType namespace is shared conceptually. The comparison is
	// case-insensitive because the object type Azure actually sends is
	// "Secret" with a capital S -- that is the literal in the documented
	// Microsoft.KeyVault.SecretNewVersionCreated payload
	// (learn.microsoft.com/azure/event-grid/event-schema-key-vault), and it is
	// what a real vault emits. A case-sensitive compare here silently drops
	// every genuine production event into the clean-discard branch below,
	// killing the whole realtime path (FR-005) while the 4h periodic reconcile
	// hides the breakage. Key Vault object types are not case-defined by
	// contract, so match them the same way the names are matched.
	if data.ObjectType == nil || !strings.EqualFold(*data.ObjectType, "secret") {
		return nil, nil
	}

	return &ParsedEvent{
		ID:         id,
		VaultName:  derefString(data.VaultName),
		SecretName: derefString(data.ObjectName),
		Version:    derefString(data.Version),
	}, nil
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
