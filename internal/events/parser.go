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

// Parse decodes and interprets one raw queue message body (still
// Base64-encoded, exactly as azure.QueueMessage.Text carries it) into a
// ParsedEvent, or reports that the message should be discarded.
//
// Return contract (contracts/queue-message.md "Processing rules"):
//   - Malformed body of any kind -> (nil, error) wrapping ErrMalformedMessage.
//     The error text is always fixed, static wording -- never anything
//     derived from body itself -- so it can never echo message content
//     (constitution I).
//   - eventType != "Microsoft.KeyVault.SecretNewVersionCreated" (rule 2:
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

	var envelope azsystemevents.EventGridEvent
	if err := json.Unmarshal(decoded, &envelope); err != nil {
		return nil, fmt.Errorf("%w: invalid event grid envelope", ErrMalformedMessage)
	}
	if envelope.EventType == nil {
		return nil, fmt.Errorf("%w: missing eventType", ErrMalformedMessage)
	}

	id := derefString(envelope.ID)

	// Rule 2: act only on SecretNewVersionCreated; every other type
	// (SecretNearExpiry, SecretExpired, anything unrecognized) is out of v1
	// scope and discarded without error.
	if *envelope.EventType != azsystemevents.TypeKeyVaultSecretNewVersionCreated {
		return nil, nil
	}

	dataBytes, ok := envelope.Data.([]byte)
	if !ok {
		return nil, fmt.Errorf("%w: missing event data", ErrMalformedMessage)
	}

	var data azsystemevents.KeyVaultSecretNewVersionCreatedEventData
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
