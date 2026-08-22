// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

// Package events will hold the queue-message parser (T018, parser.go) and the
// queue listener (T019, listener.go) that map Event Grid Key Vault
// notifications to reconcile requests. This file is written first per
// constitution IV / tasks.md T015 (TDD): it exercises Parse's contract before
// parser.go exists, so `go build ./...` and `go test ./...` are expected to
// fail with "undefined: Parse" (and friends) until T018 lands.
//
// ASSUMED PARSER API (contract for the implementer of parser.go):
//
//	type ParsedEvent struct {
//	    ID         string // Event Grid event id; logging/correlation only
//	    VaultName  string
//	    SecretName string
//	    Version    string // logging/correlation only (contracts/queue-message.md rule 4)
//	}
//
//	var ErrMalformedMessage = errors.New(...)
//
//	func Parse(body []byte) (*ParsedEvent, error)
//
// body is the raw queue message text exactly as azure.QueueMessage.Text
// carries it: still Base64-encoded, per contracts/queue-message.md ("Queue
// message body: Base64-encoded JSON of a single Event Grid schema event").
// Parse itself does the Base64 decode + JSON unmarshal (rule 1).
//
// Return contract (contracts/queue-message.md "Processing rules"):
//
//  1. Malformed body (bad Base64, bad JSON envelope, missing/bad data
//     payload) -> (nil, error) where error wraps ErrMalformedMessage. The
//     error's message NEVER echoes any byte of the message body (constitution
//     I: this rule is unconditional, not merely because values are not
//     expected on this path).
//  2. eventType != "Microsoft.KeyVault.SecretNewVersionCreated" (e.g.
//     SecretNearExpiry, SecretExpired, or any other/unknown type) -> (nil,
//     nil): a clean, silent discard, not an error (v1 scope).
//  3. data.ObjectType != "secret" -> (nil, nil): same silent discard.
//  4. Otherwise -> (&ParsedEvent{...}, nil) with VaultName/SecretName/Version
//     copied verbatim from data.VaultName/data.ObjectName/data.Version and ID
//     from the envelope's top-level id.
package events

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// eventPayload builds a JSON Event Grid event matching the shape documented
// in contracts/queue-message.md's example, with the given eventType/
// objectType/vault/object/version substituted in. It never embeds anything
// resembling a secret value (constitution I: test fixtures use fake,
// obviously-fake identifiers only). id is always testEventID today; it stays
// a real parameter since listener_test.go (T016) reuses this helper and a
// future test may want a distinct event id per message.
//
//nolint:unparam // see comment above
func eventPayload(id, eventType, vaultName, objectType, objectName, version string) string {
	return fmt.Sprintf(`{
  "id": %q,
  "topic": "/subscriptions/sub-id/resourceGroups/rg/providers/Microsoft.KeyVault/vaults/%s",
  "subject": %q,
  "eventType": %q,
  "eventTime": "2026-08-21T10:12:33.1234567Z",
  "dataVersion": "1",
  "metadataVersion": "1",
  "data": {
    "Id": "https://%s.vault.azure.net/secrets/%s/%s",
    "VaultName": %q,
    "ObjectType": %q,
    "ObjectName": %q,
    "Version": %q,
    "NBF": null,
    "EXP": null
  }
}`, id, vaultName, objectName, eventType, vaultName, objectName, version, vaultName, objectType, objectName, version)
}

// encodedBody Base64-encodes payload the way Event Grid encodes events when
// delivering to a Storage Queue (contracts/queue-message.md "Envelope").
func encodedBody(payload string) []byte {
	return []byte(base64.StdEncoding.EncodeToString([]byte(payload)))
}

const (
	testEventID  = "6a6cbc37-fake-event-id-63c2fc7b"
	testVault    = "fake-vault"
	testObject   = "fake-app-password"
	testVersion  = "fakeversion0123456789abcdef01234567"
	secretType   = "secret"
	certType     = "certificate"
	newVersionET = "Microsoft.KeyVault.SecretNewVersionCreated"
	nearExpiryET = "Microsoft.KeyVault.SecretNearExpiry"
	expiredET    = "Microsoft.KeyVault.SecretExpired"
	unknownET    = "Microsoft.KeyVault.SomeFutureEventType"
)

func TestParse_ValidSecretNewVersionCreated_ReturnsVaultAndSecretName(t *testing.T) {
	body := encodedBody(eventPayload(testEventID, newVersionET, testVault, secretType, testObject, testVersion))

	got, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	if got == nil {
		t.Fatal("Parse() = nil, want a ParsedEvent for a valid SecretNewVersionCreated event")
	}
	if got.ID != testEventID {
		t.Errorf("ID = %q, want %q", got.ID, testEventID)
	}
	if got.VaultName != testVault {
		t.Errorf("VaultName = %q, want %q", got.VaultName, testVault)
	}
	if got.SecretName != testObject {
		t.Errorf("SecretName = %q, want %q", got.SecretName, testObject)
	}
	if got.Version != testVersion {
		t.Errorf("Version = %q, want %q", got.Version, testVersion)
	}
}

func TestParse_SecretNearExpiry_Discarded(t *testing.T) {
	body := encodedBody(eventPayload(testEventID, nearExpiryET, testVault, secretType, testObject, testVersion))

	got, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil (discard, not an error)", err)
	}
	if got != nil {
		t.Fatalf("Parse() = %+v, want nil (SecretNearExpiry is out of v1 scope)", got)
	}
}

func TestParse_SecretExpired_Discarded(t *testing.T) {
	body := encodedBody(eventPayload(testEventID, expiredET, testVault, secretType, testObject, testVersion))

	got, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil (discard, not an error)", err)
	}
	if got != nil {
		t.Fatalf("Parse() = %+v, want nil (SecretExpired is out of v1 scope)", got)
	}
}

func TestParse_UnknownEventType_Discarded(t *testing.T) {
	body := encodedBody(eventPayload(testEventID, unknownET, testVault, secretType, testObject, testVersion))

	got, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil (discard, not an error)", err)
	}
	if got != nil {
		t.Fatalf("Parse() = %+v, want nil (unknown event types are discarded)", got)
	}
}

func TestParse_ObjectTypeNotSecret_Discarded(t *testing.T) {
	// eventType matches, but the object acted on is a certificate, not a
	// secret: contracts/queue-message.md rule 2 requires both conditions.
	body := encodedBody(eventPayload(testEventID, newVersionET, testVault, certType, testObject, testVersion))

	got, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil (discard, not an error)", err)
	}
	if got != nil {
		t.Fatalf("Parse() = %+v, want nil (ObjectType != secret must be discarded)", got)
	}
}

func TestParse_MalformedBase64_DiscardErrorWithoutBodyContent(t *testing.T) {
	// Not valid Base64 at all. The marker string below stands in for
	// "content that must never be echoed back" -- constitution I requires
	// this even though the queue contract says values never transit here;
	// the redaction rule is unconditional.
	const marker = "SENTINEL-should-never-appear-in-any-error-abc123"
	body := []byte("not-valid-base64!!! " + marker)

	got, err := Parse(body)
	if got != nil {
		t.Fatalf("Parse() = %+v, want nil on malformed input", got)
	}
	if err == nil {
		t.Fatal("Parse() error = nil, want a wrapped ErrMalformedMessage for invalid Base64")
	}
	if !errors.Is(err, ErrMalformedMessage) {
		t.Errorf("Parse() error = %v, want it to wrap ErrMalformedMessage", err)
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("Parse() error echoes message body content: %q", err.Error())
	}
}

func TestParse_MalformedJSON_DiscardErrorWithoutBodyContent(t *testing.T) {
	const marker = "SENTINEL-invalid-json-marker-def456"
	body := encodedBody(`{"not": "a valid event grid envelope", ` + marker)

	got, err := Parse(body)
	if got != nil {
		t.Fatalf("Parse() = %+v, want nil on malformed input", got)
	}
	if err == nil {
		t.Fatal("Parse() error = nil, want a wrapped ErrMalformedMessage for invalid JSON")
	}
	if !errors.Is(err, ErrMalformedMessage) {
		t.Errorf("Parse() error = %v, want it to wrap ErrMalformedMessage", err)
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("Parse() error echoes message body content: %q", err.Error())
	}
}

func TestParse_MissingEventType_MalformedError(t *testing.T) {
	body := encodedBody(`{"id": "x", "data": {"VaultName": "v", "ObjectName": "s", "ObjectType": "secret"}}`)

	got, err := Parse(body)
	if got != nil {
		t.Fatalf("Parse() = %+v, want nil on malformed input", got)
	}
	if !errors.Is(err, ErrMalformedMessage) {
		t.Errorf("Parse() error = %v, want it to wrap ErrMalformedMessage (missing eventType)", err)
	}
}

func TestParse_MissingData_MalformedError(t *testing.T) {
	body := encodedBody(fmt.Sprintf(`{"id": %q, "eventType": %q}`, testEventID, newVersionET))

	got, err := Parse(body)
	if got != nil {
		t.Fatalf("Parse() = %+v, want nil on malformed input", got)
	}
	if !errors.Is(err, ErrMalformedMessage) {
		t.Errorf("Parse() error = %v, want it to wrap ErrMalformedMessage (missing data)", err)
	}
}
