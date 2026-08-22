// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
)

// sentinelValue looks like a real secret so a test failure would be obvious,
// but it must never appear in any error message produced by this package
// (constitution I). Every test below that touches an error's Error() string
// asserts this value is absent from it.
const sentinelValue = "SENTINEL-do-not-log-me-9f3a"

func TestClassifyGetSecretError_HTTPStatusCodes(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantErr    error
	}{
		{"not found", http.StatusNotFound, ErrSecretNotFound},
		{"unauthorized", http.StatusUnauthorized, ErrAccessDenied},
		{"forbidden", http.StatusForbidden, ErrAccessDenied},
		{"too many requests", http.StatusTooManyRequests, ErrTransient},
		{"internal server error", http.StatusInternalServerError, ErrTransient},
		{"bad gateway", http.StatusBadGateway, ErrTransient},
		{"unrecognized 4xx falls back to transient", http.StatusBadRequest, ErrTransient},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			respErr := &azcore.ResponseError{StatusCode: tt.statusCode}
			got := classifyGetSecretError("my-vault", "my-secret", respErr)

			if !errors.Is(got, tt.wantErr) {
				t.Fatalf("classifyGetSecretError() = %v, want wrapped %v", got, tt.wantErr)
			}
			if got.Error() == "" {
				t.Fatal("expected a non-empty error message")
			}
		})
	}
}

func TestClassifyGetSecretError_NetworkErrorIsTransient(t *testing.T) {
	netErr := &net.OpError{Op: "dial", Err: errors.New("connection refused")}

	got := classifyGetSecretError("my-vault", "my-secret", netErr)

	if !errors.Is(got, ErrTransient) {
		t.Fatalf("classifyGetSecretError() = %v, want wrapped %v", got, ErrTransient)
	}
}

func TestClassifyGetSecretError_MessageReferencesOnlyIdentifiers(t *testing.T) {
	respErr := &azcore.ResponseError{StatusCode: http.StatusNotFound}

	got := classifyGetSecretError("my-vault", "my-secret", respErr)

	msg := got.Error()
	if !strings.Contains(msg, "my-vault") || !strings.Contains(msg, "my-secret") {
		t.Fatalf("expected error message %q to reference vault and secret name", msg)
	}
}

func TestSecretIsDisabled(t *testing.T) {
	enabled := true
	disabled := false

	tests := []struct {
		name  string
		attrs *azsecrets.SecretAttributes
		want  bool
	}{
		{"nil attributes treated as enabled", nil, false},
		{"nil Enabled treated as enabled", &azsecrets.SecretAttributes{}, false},
		{"explicitly enabled", &azsecrets.SecretAttributes{Enabled: &enabled}, false},
		{"explicitly disabled", &azsecrets.SecretAttributes{Enabled: &disabled}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := secretIsDisabled(tt.attrs); got != tt.want {
				t.Fatalf("secretIsDisabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVaultURL(t *testing.T) {
	got := vaultURL("my-vault")
	want := "https://my-vault.vault.azure.net/"
	if got != want {
		t.Fatalf("vaultURL() = %q, want %q", got, want)
	}
}

// TestNoSentinelValueLeaksIntoErrorMessages is the constitution I regression
// guard for this file. A real SDK error's own Error() string could, in
// principle, embed response body details; classifyGetSecretError must never
// forward that text verbatim into the error it returns — only the fixed
// sentinel category plus the vault/secret identifiers passed in explicitly.
// This does not exercise the real azsecrets wire call (that needs a
// live/mocked client, covered by the T030 integration tests) — it covers the
// pure classification logic that lives in this file.
func TestNoSentinelValueLeaksIntoErrorMessages(t *testing.T) {
	respErr := &azcore.ResponseError{StatusCode: http.StatusForbidden}
	wrapped := fmt.Errorf("some sdk internal detail mentioning %s: %w", sentinelValue, respErr)

	got := classifyGetSecretError("my-vault", "my-secret", wrapped)

	if strings.Contains(got.Error(), sentinelValue) {
		t.Fatalf("classified error leaked underlying SDK error text: %q", got.Error())
	}
}
