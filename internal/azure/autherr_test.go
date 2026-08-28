// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

// These tests pin isAuthFailure (autherr.go) and the order classifySentinel
// applies it in. Nothing here needs a live Azure: both error shapes the
// credential chain returns are constructible from outside azidentity, and the
// azcore wrapper that sits between them and us is simulated by a local type,
// so the whole "workload identity is not wired up" path is covered by plain
// unit tests.
package azure

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// bearerWrapper stands in for errorinfo.NonRetriableError, the wrapper
// azcore's bearer-token policy puts around whatever the credential returned.
// We cannot import errorinfo (it lives under the SDK's internal/ path), so
// this reproduces the only property that matters to us: it hides the original
// error behind an Unwrap.
//
// It deliberately does NOT implement NonRetriable() itself, even though the
// real wrapper does. If it did, errors.As would match the wrapper and every
// test below would pass without ever reaching the credential error inside —
// exactly the false green we need to avoid. Without the marker, a passing test
// proves isAuthFailure really walks the chain down to azidentity's own error.
type bearerWrapper struct{ inner error }

func (w *bearerWrapper) Error() string { return w.inner.Error() }
func (w *bearerWrapper) Unwrap() error { return w.inner }

// doubleWrapped nests bearerWrapper twice, because the real pipeline wraps the
// credential error in errorinfo.NonRetriableError twice on its way out.
func doubleWrapped(err error) error {
	return &bearerWrapper{inner: &bearerWrapper{inner: err}}
}

// TestIsAuthFailure_CredentialChainShapes covers both errors azidentity's
// chained credential can hand back when no token could be acquired, each seen
// through the wrapping azcore applies. Before ErrAuthFailure existed both of
// these landed on ErrTransient, whose advice ("wait, the controller retries")
// is wrong for every one of them: an unannotated ServiceAccount or a broken
// federated credential never fixes itself.
func TestIsAuthFailure_CredentialChainShapes(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			// The exported shape: a credential in the chain actually tried and
			// failed (wrong client ID, federated-credential subject mismatch).
			// Its unexported fields render empty; errors.As only needs the type.
			name: "AuthenticationFailedError, bare",
			err:  &azidentity.AuthenticationFailedError{},
		},
		{
			name: "AuthenticationFailedError, wrapped by the bearer-token policy",
			err:  doubleWrapped(&azidentity.AuthenticationFailedError{}),
		},
		{
			// The unexported shape: every credential in the chain declined
			// (no annotation, no projected token, no IMDS). It can only be
			// matched through the NonRetriable() marker method, which is why
			// autherr.go re-declares that interface.
			name: "credentialUnavailableError, bare",
			err:  azidentity.NewCredentialUnavailableError("no credential in the chain could provide a token"),
		},
		{
			name: "credentialUnavailableError, wrapped by the bearer-token policy",
			err:  doubleWrapped(azidentity.NewCredentialUnavailableError("no credential in the chain could provide a token")),
		},
		{
			// The shape an operator actually meets: the credential error is
			// buried under the SDK's own context before it reaches us.
			name: "wrapped again by an outer fmt.Errorf",
			err:  fmt.Errorf("GET secret: %w", doubleWrapped(azidentity.NewCredentialUnavailableError("DefaultAzureCredential: no credential available"))),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !isAuthFailure(tt.err) {
				t.Fatalf("isAuthFailure() = false, want true for %T", tt.err)
			}

			sentinel, statusCode := classifySentinel(tt.err)
			if !errors.Is(sentinel, ErrAuthFailure) {
				t.Fatalf("classifySentinel() = %v, want %v", sentinel, ErrAuthFailure)
			}
			// No token means the request never reached Key Vault, so there is
			// no HTTP status to report.
			if statusCode != 0 {
				t.Fatalf("classifySentinel() statusCode = %d, want 0 (no HTTP response)", statusCode)
			}
		})
	}
}

// TestIsAuthFailure_DoesNotOverMatch guards the marker-interface check in
// autherr.go from becoming a catch-all. nonRetriable is matched by method name
// alone, so anything that happens to grow a NonRetriable() method would be
// reported as an auth failure; these are the ordinary errors that must keep
// classifying as transient and retryable.
func TestIsAuthFailure_DoesNotOverMatch(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"plain error", errors.New("something went wrong")},
		{"network dial failure", &net.OpError{Op: "dial", Err: errors.New("connection refused")}},
		{"wrapped network failure", fmt.Errorf("get secret: %w", &net.OpError{Op: "dial", Err: errors.New("i/o timeout")})},
		{"http response error with no status", &azcore.ResponseError{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if isAuthFailure(tt.err) {
				t.Fatalf("isAuthFailure() = true, want false for %T", tt.err)
			}

			sentinel, _ := classifySentinel(tt.err)
			if errors.Is(sentinel, ErrAuthFailure) {
				t.Fatalf("classifySentinel() = %v, want anything but %v", sentinel, ErrAuthFailure)
			}
			if !errors.Is(sentinel, ErrTransient) {
				t.Fatalf("classifySentinel() = %v, want %v", sentinel, ErrTransient)
			}
		})
	}
}

// TestClassifySentinel_ResponseErrorWinsOverAuthShape is the case that keeps
// the auth-failure fix from swallowing a real 403. Key Vault answering the
// request is the authoritative signal: if there is an *azcore.ResponseError in
// the chain, its status code decides, no matter what else is in there. A 403
// means the token WAS accepted and the role assignment is missing
// (AccessDenied); calling it AuthenticationFailed would send the operator to
// fix the cluster instead of Azure RBAC.
func TestClassifySentinel_ResponseErrorWinsOverAuthShape(t *testing.T) {
	tests := []struct {
		name       string
		authShape  error
		statusCode int
		wantErr    error
	}{
		{"403 alongside AuthenticationFailedError", &azidentity.AuthenticationFailedError{}, http.StatusForbidden, ErrAccessDenied},
		{"403 alongside credentialUnavailableError", azidentity.NewCredentialUnavailableError("no credential available"), http.StatusForbidden, ErrAccessDenied},
		{"401 alongside credentialUnavailableError", azidentity.NewCredentialUnavailableError("no credential available"), http.StatusUnauthorized, ErrAccessDenied},
		{"404 alongside credentialUnavailableError", azidentity.NewCredentialUnavailableError("no credential available"), http.StatusNotFound, ErrSecretNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			respErr := &azcore.ResponseError{StatusCode: tt.statusCode}
			// Both errors in one chain, the way fmt.Errorf's multi-%w builds it.
			err := fmt.Errorf("get secret: %w: %w", respErr, doubleWrapped(tt.authShape))

			sentinel, statusCode := classifySentinel(err)
			if !errors.Is(sentinel, tt.wantErr) {
				t.Fatalf("classifySentinel() = %v, want %v", sentinel, tt.wantErr)
			}
			if errors.Is(sentinel, ErrAuthFailure) {
				t.Fatalf("classifySentinel() = %v: a real Key Vault answer was swallowed by the auth check", sentinel)
			}
			if statusCode != tt.statusCode {
				t.Fatalf("classifySentinel() statusCode = %d, want %d", statusCode, tt.statusCode)
			}
		})
	}
}

// TestClassifyGetSecretError_AuthFailureMessageReferencesOnlyIdentifiers keeps
// the new sentinel inside the same constitution I contract as the older ones:
// the returned message carries the vault and secret names plus fixed text, and
// never the credential error's own words (which for a real
// AuthenticationFailedError can include a whole HTTP response dump).
func TestClassifyGetSecretError_AuthFailureMessageReferencesOnlyIdentifiers(t *testing.T) {
	inner := azidentity.NewCredentialUnavailableError("chain detail mentioning " + sentinelValue)
	got := classifyGetSecretError(context.Background(), "my-vault", "my-secret", doubleWrapped(inner))

	if !errors.Is(got, ErrAuthFailure) {
		t.Fatalf("classifyGetSecretError() = %v, want wrapped %v", got, ErrAuthFailure)
	}
	msg := got.Error()
	if !strings.Contains(msg, "my-vault") || !strings.Contains(msg, "my-secret") {
		t.Fatalf("expected error message %q to reference vault and secret name", msg)
	}
	if strings.Contains(msg, sentinelValue) {
		t.Fatalf("classified error leaked the credential error text: %q", msg)
	}
}

// TestClassificationName covers the sentinel -> status.reason mapping the log
// line reports, so an operator reading "classification" in the log and an
// operator reading `kubectl get secretsync` see the same word.
func TestClassificationName(t *testing.T) {
	tests := []struct {
		name     string
		sentinel error
		want     string
	}{
		{"secret not found", ErrSecretNotFound, "SecretNotFound"},
		{"access denied", ErrAccessDenied, "AccessDenied"},
		{"auth failure", ErrAuthFailure, "AuthenticationFailed"},
		{"secret disabled", ErrSecretDisabled, "SourceDisabled"},
		{"transient", ErrTransient, "TransientError"},
		// An unrecognised sentinel must fall back to the retryable reason
		// rather than surfacing a status.reason the CRD does not define.
		{"unrecognised falls back to transient", errors.New("something unclassified"), "TransientError"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classificationName(tt.sentinel); got != tt.want {
				t.Fatalf("classificationName(%v) = %q, want %q", tt.sentinel, got, tt.want)
			}
		})
	}
}
