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
//  3. data.ObjectType that is not "secret" case-insensitively -> (nil, nil):
//     same silent discard. Azure's documented payload carries "Secret" with a
//     capital S, so the shared fixtures below use that literal.
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
	return eventPayloadWithTimestamps(id, eventType, vaultName, objectType, objectName, version, docNBF, docEXP)
}

// eventPayloadWithTimestamps is eventPayload with NBF/EXP spelled out by the
// caller, as a raw JSON literal each. It exists because Microsoft documents
// those two fields inconsistently on the same page: the
// SecretNewVersionCreated sample sends them quoted ("NBF":"1559081980") while
// the property table below it calls them numbers, and a payload with neither
// (null, or the field absent) is equally plausible from a vault that has no
// dates set. Parse reads neither field, so every one of those spellings must
// come out as the same ParsedEvent -- see TestParse_NBFAndEXPSpellings.
func eventPayloadWithTimestamps(id, eventType, vaultName, objectType, objectName, version, nbf, exp string) string {
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
    "NBF": %s,
    "EXP": %s
  }
}`, id, vaultName, objectName, eventType, vaultName, objectName, version, vaultName, objectType, objectName, version, nbf, exp)
}

// encodedBody Base64-encodes payload the way Event Grid encodes events when
// delivering to a Storage Queue (contracts/queue-message.md "Envelope").
func encodedBody(payload string) []byte {
	return []byte(base64.StdEncoding.EncodeToString([]byte(payload)))
}

const (
	testEventID = "6a6cbc37-fake-event-id-63c2fc7b"
	testVault   = "fake-vault"
	testObject  = "fake-app-password"
	testVersion = "fakeversion0123456789abcdef01234567"
	// secretType is deliberately the exact literal a real Key Vault emits --
	// "Secret", capital S, as in the documented event schema
	// (learn.microsoft.com/azure/event-grid/event-schema-key-vault). Every
	// fixture in this package goes through it, so the whole suite runs against
	// the production shape of the payload; the older lowercase spelling is
	// still covered explicitly by TestParse_ObjectTypeCasing_Variants.
	secretType = "Secret"
	// docNBF/docEXP are the raw JSON literals Microsoft's own
	// SecretNewVersionCreated sample uses for those two fields on
	// learn.microsoft.com/azure/event-grid/event-schema-key-vault: quoted
	// strings, not numbers. Every fixture in this package carries them, so the
	// whole suite -- parser and listener alike -- runs against a payload that
	// is byte-for-byte the shape of the published example instead of a
	// convenient one. TestParse_NBFAndEXPSpellings covers the other spellings
	// the same page leaves open.
	docNBF = `"1559081980"`
	docEXP = `"1559082102"`
	// nullJSON is the spelling every fixture in this package used before the
	// NBF/EXP typing fix, and the one a vault object with no not-before/expiry
	// dates set produces. It is the only spelling the old parser accepted.
	nullJSON        = "null"
	secretTypeLower = "secret"
	secretTypeUpper = "SECRET"
	certType        = "certificate"
	keyType         = "key"
	newVersionET    = "Microsoft.KeyVault.SecretNewVersionCreated"
	nearExpiryET    = "Microsoft.KeyVault.SecretNearExpiry"
	expiredET       = "Microsoft.KeyVault.SecretExpired"
	unknownET       = "Microsoft.KeyVault.SomeFutureEventType"
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

// TestParse_ObjectTypeCasing_Variants pins rule 2's object-type guard as a
// case-insensitive match, and pins that it is still a real guard.
//
// This is the regression test for the bug that made the whole realtime path
// (FR-005) dead in production while every test in the repo passed: the guard
// compared *data.ObjectType against the lowercase literal "secret", but Azure
// sends "Secret" (learn.microsoft.com/azure/event-grid/event-schema-key-vault),
// so every genuine event was treated as a clean discard -- the listener
// deleted the message and logged it at V(1) as non-actionable. The suite never
// noticed because every fixture used the lowercase spelling. Hence: all three
// casings must parse, and a genuinely different object type must still be
// discarded.
func TestParse_ObjectTypeCasing_Variants(t *testing.T) {
	tests := []struct {
		name       string
		objectType string
		wantParsed bool
	}{
		{name: "azure casing", objectType: secretType, wantParsed: true},
		{name: "lowercase", objectType: secretTypeLower, wantParsed: true},
		{name: "uppercase", objectType: secretTypeUpper, wantParsed: true},
		{name: "certificate", objectType: certType, wantParsed: false},
		{name: "key", objectType: keyType, wantParsed: false},
		{name: "certificate in azure casing", objectType: "Certificate", wantParsed: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := encodedBody(eventPayload(testEventID, newVersionET, testVault, tt.objectType, testObject, testVersion))

			got, err := Parse(body)
			if err != nil {
				t.Fatalf("Parse() error = %v, want nil (a wrong object type is a discard, not an error)", err)
			}
			if tt.wantParsed {
				if got == nil {
					t.Fatalf("Parse() = nil for ObjectType %q, want a ParsedEvent (object types are matched case-insensitively)", tt.objectType)
				}
				if got.SecretName != testObject {
					t.Errorf("SecretName = %q, want %q", got.SecretName, testObject)
				}
				return
			}
			if got != nil {
				t.Fatalf("Parse() = %+v for ObjectType %q, want nil (only secrets are actioned)", got, tt.objectType)
			}
		})
	}
}

// TestParse_NBFAndEXPSpellings pins that NBF and EXP cannot decide whether an
// event is actionable, whichever way Azure spells them.
//
// This is a regression test with a real failure behind it. Parse used to decode
// data into azsystemevents.KeyVaultSecretNewVersionCreatedEventData, which
// types NBF/EXP as *float32 with a strict generated UnmarshalJSON. Microsoft's
// published SecretNewVersionCreated sample sends them as quoted strings
// ("NBF":"1559081980") while the property table on that same page calls them
// numbers (learn.microsoft.com/azure/event-grid/event-schema-key-vault) -- so
// the documented payload failed to decode, Parse returned ErrMalformedMessage,
// and listener.go deleted the message as malformed. A rotation lost, repaired
// only hours later by the periodic reconcile: exactly the FR-005 failure this
// file's ObjectType tests exist to prevent, one step earlier in Parse. Nothing
// caught it because every fixture used "NBF": null, which happens to decode.
//
// Parse reads neither field, so the fix was to stop decoding them at all. This
// test holds that: same event, four spellings of two fields Parse ignores, one
// identical ParsedEvent.
func TestParse_NBFAndEXPSpellings(t *testing.T) {
	tests := []struct {
		name string
		nbf  string
		exp  string
	}{
		{name: "quoted strings, as in the documented sample", nbf: docNBF, exp: docEXP},
		{name: "numbers, as in the documented property table", nbf: "1559081980", exp: "1559082102"},
		{name: "null, as when the object has no dates set", nbf: nullJSON, exp: nullJSON},
		{name: "one of each, in case a vault is inconsistent", nbf: docNBF, exp: "1559082102"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := encodedBody(eventPayloadWithTimestamps(
				testEventID, newVersionET, testVault, secretType, testObject, testVersion, tt.nbf, tt.exp))

			got, err := Parse(body)
			if err != nil {
				t.Fatalf("Parse() error = %v, want nil (NBF/EXP are never read, so they cannot make a message malformed)", err)
			}
			if got == nil {
				t.Fatal("Parse() = nil, want a ParsedEvent")
			}
			want := ParsedEvent{ID: testEventID, VaultName: testVault, SecretName: testObject, Version: testVersion}
			if *got != want {
				t.Errorf("Parse() = %+v, want %+v", *got, want)
			}
		})
	}
}

// TestParse_NBFAndEXPAbsent_Parses covers the remaining shape: a data payload
// that omits the two timestamps entirely. Same reason as
// TestParse_NBFAndEXPSpellings -- Parse must not depend on fields it does not
// read.
func TestParse_NBFAndEXPAbsent_Parses(t *testing.T) {
	body := encodedBody(fmt.Sprintf(`{
  "id": %q,
  "eventType": %q,
  "data": {
    "Id": "https://%s.vault.azure.net/secrets/%s/%s",
    "VaultName": %q,
    "ObjectType": %q,
    "ObjectName": %q,
    "Version": %q
  }
}`, testEventID, newVersionET, testVault, testObject, testVersion, testVault, secretType, testObject, testVersion))

	got, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	if got == nil {
		t.Fatal("Parse() = nil, want a ParsedEvent when NBF/EXP are absent")
	}
	want := ParsedEvent{ID: testEventID, VaultName: testVault, SecretName: testObject, Version: testVersion}
	if *got != want {
		t.Errorf("Parse() = %+v, want %+v", *got, want)
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
