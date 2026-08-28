// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// sentinelValue looks like a real secret so a test failure would be obvious,
// but it must never appear in any error message produced by this package
// (constitution I). Every test below that touches an error's Error() string
// asserts this value is absent from it.
const sentinelValue = "SENTINEL-do-not-log-me-9f3a"

// opDial is net.OpError's Op for a failed connection attempt, shared by every
// test in this package that builds one.
const opDial = "dial"

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
			got := classifyGetSecretError(context.Background(), "my-vault", "my-secret", respErr)

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
	netErr := &net.OpError{Op: opDial, Err: errors.New("connection refused")}

	got := classifyGetSecretError(context.Background(), "my-vault", "my-secret", netErr)

	if !errors.Is(got, ErrTransient) {
		t.Fatalf("classifyGetSecretError() = %v, want wrapped %v", got, ErrTransient)
	}
}

func TestClassifyGetSecretError_MessageReferencesOnlyIdentifiers(t *testing.T) {
	respErr := &azcore.ResponseError{StatusCode: http.StatusNotFound}

	got := classifyGetSecretError(context.Background(), "my-vault", "my-secret", respErr)

	msg := got.Error()
	if !strings.Contains(msg, "my-vault") || !strings.Contains(msg, "my-secret") {
		t.Fatalf("expected error message %q to reference vault and secret name", msg)
	}
}

// TestClassifyGetSecretError_DisabledSecretRegardlessOfStatusCode locks in
// the T030 finding: Key Vault refuses GetSecret on a disabled secret with an
// error response instead of a 200 body carrying attributes.enabled=false,
// so secretIsDisabled (below) never actually fires for the "get the latest
// version" call GetLatest makes. Real Key Vault answers 403; the Lowkey
// Vault emulator (internal/azure/keyvault_integration_test.go) answers 404.
// classifyGetSecretError must catch both by the response wording, not the
// status code.
func TestClassifyGetSecretError_DisabledSecretRegardlessOfStatusCode(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
	}{
		{
			name:       "real key vault style 403",
			statusCode: http.StatusForbidden,
			body:       `{"error":{"code":"Forbidden","message":"Operation get is not allowed on a disabled secret."}}`,
		},
		{
			name:       "lowkey vault emulator style 404",
			statusCode: http.StatusNotFound,
			body:       `{"error":{"code":"NotFoundException","message":"Operation get is not allowed on a disabled entity."}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			respErr := responseErrorWithBody(tt.statusCode, tt.body)

			got := classifyGetSecretError(context.Background(), "my-vault", "my-secret", respErr)

			if !errors.Is(got, ErrSecretDisabled) {
				t.Fatalf("classifyGetSecretError() = %v, want wrapped %v", got, ErrSecretDisabled)
			}
			if strings.Contains(got.Error(), tt.body) {
				t.Fatalf("classified error leaked the raw response body: %q", got.Error())
			}
		})
	}
}

// responseErrorWithBody builds an *azcore.ResponseError whose Error() method
// renders the given status code and body, the same way one constructed by
// the real SDK pipeline from an HTTP response would.
func responseErrorWithBody(statusCode int, body string) *azcore.ResponseError {
	resp := &http.Response{
		StatusCode: statusCode,
		Status:     http.StatusText(statusCode),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
	return &azcore.ResponseError{StatusCode: statusCode, RawResponse: resp}
}

// responseErrorWithRequest is responseErrorWithBody plus the originating
// request attached, so respErr.Error() renders the full request URL exactly
// as the real SDK pipeline does. Used by the poison-name tests below: the
// URL contains the secret's name, and classification must never read it.
func responseErrorWithRequest(t *testing.T, statusCode int, body, rawURL string) *azcore.ResponseError {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse test url %q: %v", rawURL, err)
	}
	respErr := responseErrorWithBody(statusCode, body)
	respErr.RawResponse.Request = &http.Request{Method: http.MethodGet, URL: u}
	return respErr
}

// TestClassifyGetSecretError_DisabledSecretInnerErrorCode locks in the
// authoritative real-Key-Vault marker for a disabled secret: HTTP 403 with
// error.innererror.code == "SecretDisabled" in the JSON body. The outer
// message here deliberately avoids the "not allowed on a disabled" phrase so
// this test proves the inner code alone is enough.
func TestClassifyGetSecretError_DisabledSecretInnerErrorCode(t *testing.T) {
	body := `{"error":{"code":"Forbidden","message":"Access denied.","innererror":{"code":"SecretDisabled"}}}`
	respErr := responseErrorWithBody(http.StatusForbidden, body)

	got := classifyGetSecretError(context.Background(), "my-vault", "my-secret", respErr)

	if !errors.Is(got, ErrSecretDisabled) {
		t.Fatalf("classifyGetSecretError() = %v, want wrapped %v", got, ErrSecretDisabled)
	}
}

// TestClassifyGetSecretError_SecretNameContainingDisabled_NotMisclassified is
// the regression test for the poison-name bug: classification used to search
// respErr.Error() -- which includes the full request URL -- for the word
// "disabled", so a secret literally named "feature-disabled-flag" turned a
// plain 404 into a bogus SourceDisabled and a plain 403 lost AccessDenied.
// The 404 body below also echoes the name inside the error *message* (real
// Key Vault does that too), so this additionally proves the message check is
// a whole-phrase match, not a bare substring "disabled".
func TestClassifyGetSecretError_SecretNameContainingDisabled_NotMisclassified(t *testing.T) {
	const poisonName = "feature-disabled-flag"
	poisonURL := "https://my-vault.vault.azure.net/secrets/" + poisonName + "?api-version=7.4"

	tests := []struct {
		name       string
		statusCode int
		body       string
		wantErr    error
	}{
		{
			name:       "plain 404 for a poison-named secret is not found, not disabled",
			statusCode: http.StatusNotFound,
			body:       `{"error":{"code":classSecretNotFound,"message":"A secret with (name/id) feature-disabled-flag was not found in this key vault."}}`,
			wantErr:    ErrSecretNotFound,
		},
		{
			name:       "plain 403 for a poison-named secret is access denied, not disabled",
			statusCode: http.StatusForbidden,
			body:       `{"error":{"code":"Forbidden","message":"The user, group or application does not have secrets get permission on key vault."}}`,
			wantErr:    ErrAccessDenied,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			respErr := responseErrorWithRequest(t, tt.statusCode, tt.body, poisonURL)

			got := classifyGetSecretError(context.Background(), "my-vault", poisonName, respErr)

			if !errors.Is(got, tt.wantErr) {
				t.Fatalf("classifyGetSecretError() = %v, want wrapped %v", got, tt.wantErr)
			}
			if errors.Is(got, ErrSecretDisabled) {
				t.Fatalf("classifyGetSecretError() = %v: poison-named secret misclassified as disabled", got)
			}
		})
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

	got := classifyGetSecretError(context.Background(), "my-vault", "my-secret", wrapped)

	if strings.Contains(got.Error(), sentinelValue) {
		t.Fatalf("classified error leaked underlying SDK error text: %q", got.Error())
	}
}

// bodySentinel and messageSentinel are planted in the two places an upstream
// SDK error carries text of its own: the cached HTTP response body, and the
// error's rendered message. Two distinct values so a failure says which of the
// two routes leaked.
const (
	bodySentinel    = "SENTINEL-body-do-not-log-me-4c71"
	messageSentinel = "SENTINEL-message-do-not-log-me-8e02"
)

// capturingLogger builds a real logr.Logger backed by zap, writing JSON lines
// into an in-memory buffer the test can parse afterwards. It is a real
// structured sink rather than a hand-rolled double so what is asserted below
// is exactly what an operator's log would hold in production (cmd/main.go
// installs zap too), including how zapr renders each value's type.
func capturingLogger() (logr.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(buf), zapcore.DebugLevel)
	return zapr.NewLogger(zap.New(core)), buf
}

// TestClassifyGetSecretError_LogsStatusCodeAndClassification pins the one
// diagnostic line classifyGetSecretError emits. The engine throws the returned
// error away after classifying it, so if this line is missing or wrong an
// operator staring at Failing/TransientError has no way to tell a 429 from a
// 500 from a DNS failure — that is the whole reason the log call exists.
//
// It also pins the Error(nil, ...) idiom: the planted sentinels sit in both
// the response body and the error's own message, so passing err as the first
// argument to Error instead of nil makes logr render the upstream text and
// this test fails on the leak check. Never "fix" that by dropping the check.
func TestClassifyGetSecretError_LogsStatusCodeAndClassification(t *testing.T) {
	// errorBody is a Key-Vault-shaped JSON error carrying the sentinel in its
	// message. It deliberately avoids the disabled-secret wording so the case
	// classifies purely on the status code.
	errorBody := func(code string) string {
		return `{"error":{"code":"` + code + `","message":"` + bodySentinel + `"}}`
	}

	tests := []struct {
		name               string
		err                error
		wantErr            error
		wantStatusCode     int
		wantClassification string
	}{
		{
			name:               "not found",
			err:                fmt.Errorf("%s: %w", messageSentinel, responseErrorWithBody(http.StatusNotFound, errorBody(classSecretNotFound))),
			wantErr:            ErrSecretNotFound,
			wantStatusCode:     http.StatusNotFound,
			wantClassification: classSecretNotFound,
		},
		{
			name:               "forbidden",
			err:                fmt.Errorf("%s: %w", messageSentinel, responseErrorWithBody(http.StatusForbidden, errorBody("Forbidden"))),
			wantErr:            ErrAccessDenied,
			wantStatusCode:     http.StatusForbidden,
			wantClassification: classAccessDenied,
		},
		{
			name:               "throttled",
			err:                fmt.Errorf("%s: %w", messageSentinel, responseErrorWithBody(http.StatusTooManyRequests, errorBody("Throttled"))),
			wantErr:            ErrTransient,
			wantStatusCode:     http.StatusTooManyRequests,
			wantClassification: classTransientError,
		},
		{
			name:               "server error",
			err:                fmt.Errorf("%s: %w", messageSentinel, responseErrorWithBody(http.StatusInternalServerError, errorBody("InternalError"))),
			wantErr:            ErrTransient,
			wantStatusCode:     http.StatusInternalServerError,
			wantClassification: classTransientError,
		},
		{
			// An unrecognised status still has to reach the log with its real
			// number: "some 4xx we have never seen" is precisely the case an
			// operator cannot diagnose without it.
			name:               "unrecognised status",
			err:                fmt.Errorf("%s: %w", messageSentinel, responseErrorWithBody(http.StatusTeapot, errorBody("Weird"))),
			wantErr:            ErrTransient,
			wantStatusCode:     http.StatusTeapot,
			wantClassification: classTransientError,
		},
		{
			// No HTTP response at all: statusCode must be 0, not omitted and
			// not invented.
			name:               "no http response",
			err:                fmt.Errorf("%s: %w", messageSentinel, &net.OpError{Op: opDial, Err: errors.New("connection refused")}),
			wantErr:            ErrTransient,
			wantStatusCode:     0,
			wantClassification: classTransientError,
		},
		{
			name:               "auth failure",
			err:                fmt.Errorf("%s: %w", messageSentinel, azidentity.NewCredentialUnavailableError(bodySentinel)),
			wantErr:            ErrAuthFailure,
			wantStatusCode:     0,
			wantClassification: classAuthenticationFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, logBuf := capturingLogger()
			ctx := logf.IntoContext(context.Background(), logger)

			got := classifyGetSecretError(ctx, "my-vault", "my-secret", tt.err)

			if !errors.Is(got, tt.wantErr) {
				t.Fatalf("classifyGetSecretError() = %v, want wrapped %v", got, tt.wantErr)
			}

			entry := singleLogEntry(t, logBuf)

			if entry["msg"] != "key vault read failed" {
				t.Fatalf("log msg = %v, want %q", entry["msg"], "key vault read failed")
			}
			if entry["vault"] != "my-vault" || entry["secret"] != "my-secret" {
				t.Fatalf("log did not carry the identifiers: vault=%v secret=%v", entry["vault"], entry["secret"])
			}
			// A JSON number decodes as float64; a statusCode logged as a string
			// (or left out) fails here. Operators and log queries need the
			// number.
			statusCode, ok := entry["statusCode"].(float64)
			if !ok {
				t.Fatalf("log statusCode = %#v, want a number", entry["statusCode"])
			}
			if int(statusCode) != tt.wantStatusCode {
				t.Fatalf("log statusCode = %d, want %d", int(statusCode), tt.wantStatusCode)
			}
			if entry["classification"] != tt.wantClassification {
				t.Fatalf("log classification = %v, want %q", entry["classification"], tt.wantClassification)
			}
			// zapr renders a non-nil error under its own key. Its absence is
			// the direct proof that Error was called with nil.
			if _, present := entry["error"]; present {
				t.Fatalf("log carried an %q key: classifyGetSecretError passed err to Error() instead of nil: %v", "error", entry["error"])
			}

			// Constitution I: neither the response body nor the SDK error's own
			// message may reach the log.
			logs := logBuf.String()
			for _, leak := range []string{bodySentinel, messageSentinel} {
				if strings.Contains(logs, leak) {
					t.Fatalf("upstream error text %q leaked into the operator log: %s", leak, logs)
				}
			}
		})
	}
}

// singleLogEntry decodes the one JSON log line the buffer must hold and fails
// the test otherwise. classifyGetSecretError logs exactly one line per
// failure: a second line would mean a caller-visible failure gets reported
// twice, and none would mean the diagnostic is gone.
func singleLogEntry(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("expected exactly one log line, got %d: %s", len(lines), buf.String())
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("decode log line %q: %v", lines[0], err)
	}
	return entry
}
