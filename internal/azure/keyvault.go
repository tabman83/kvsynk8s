// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

// Package azure holds the thin wrappers around the Azure SDK for Go that the
// sync engine and event listener depend on through small interfaces
// (SecretReader here, QueueSource in queue.go).
package azure

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
)

// Sentinel errors a caller classifies a SecretReader failure with, via
// errors.Is, to pick the right SecretSync status.reason (data-model.md):
// SecretNotFound, AccessDenied, SourceDisabled, TransientError.
//
// CONSTITUTION I (non-negotiable): none of these, nor any error returned by
// this file, ever carries the secret value. Every error message built here
// references only the vault name and secret name (both identifiers, never
// secret content) plus a fixed, static description.
var (
	// ErrSecretNotFound means the secret (or the requested version) does not
	// exist in the vault. HTTP 404. Maps to status.reason SecretNotFound.
	ErrSecretNotFound = errors.New("secret not found in key vault")

	// ErrAccessDenied means the caller's identity lacks permission to read
	// the secret. HTTP 401/403. Maps to status.reason AccessDenied.
	ErrAccessDenied = errors.New("access denied to key vault secret")

	// ErrSecretDisabled means the secret exists but is administratively
	// disabled in the vault. Maps to status.reason SourceDisabled.
	ErrSecretDisabled = errors.New("secret is disabled in key vault")

	// ErrTransient means the failure is expected to be temporary (429,
	// 5xx, or a network-level error with no HTTP response at all) and the
	// caller should retry with backoff. Maps to status.reason TransientError.
	ErrTransient = errors.New("transient error reading key vault secret")
)

// SecretReader fetches the current value and version of a Key Vault secret.
//
// Implementations MUST NOT log the returned value, include it in a returned
// error, or expose it anywhere other than the return value itself
// (constitution I). GetLatest always fetches the newest version of the
// secret; callers that only have a specific version from an event payload
// still call this — latest-wins is the sync rule (data-model.md).
type SecretReader interface {
	// GetLatest returns the current value and version identifier of
	// secretName in vaultName. On failure, err wraps one of the sentinel
	// errors declared above so the caller can classify it with errors.Is.
	GetLatest(ctx context.Context, vaultName, secretName string) (value, version string, err error)
}

// secretsClient is the subset of *azsecrets.Client that keyVaultReader needs.
// It exists so the vault-URL-to-client wiring stays testable in principle;
// the real azsecrets integration is exercised by T030, not here.
type secretsClient interface {
	GetSecret(ctx context.Context, name string, version string, options *azsecrets.GetSecretOptions) (azsecrets.GetSecretResponse, error)
}

// keyVaultReader is the azsecrets-backed SecretReader. Key Vault names are
// globally unique and map 1:1 to a vault URL, so it keeps one client per
// vault name and reuses it across calls instead of reconnecting every time.
type keyVaultReader struct {
	credential azcore.TokenCredential

	mu      sync.Mutex
	clients map[string]secretsClient
}

// NewSecretReader builds a SecretReader that authenticates with
// azidentity.NewDefaultAzureCredential (workload identity in-cluster; falls
// back through the standard credential chain locally, per constitution V:
// short-lived, platform-issued credentials only, never static secrets).
func NewSecretReader() (SecretReader, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create default azure credential: %w", err)
	}
	return &keyVaultReader{
		credential: cred,
		clients:    make(map[string]secretsClient),
	}, nil
}

// vaultURL builds the standard Azure public cloud Key Vault URL for a vault
// name. It never touches secret content.
func vaultURL(vaultName string) string {
	return fmt.Sprintf("https://%s.vault.azure.net/", vaultName)
}

// testEndpointOverrideEnv, when set, replaces vaultURL's public-cloud
// address with this fixed endpoint for every vault name, and disables
// azsecrets' challenge-resource-domain check (which otherwise assumes a
// *.vault.azure.net endpoint and rejects anything else). It exists solely so
// test/e2e can point a real, unmodified operator binary at a local
// Key-Vault-compatible emulator that cannot serve *.vault.azure.net
// hostnames (T032; research.md R10) -- a real deployment never sets this
// env var, so clientFor's behavior for every actual production vault is
// byte-for-byte what it was before this existed: same URL
// (vaultURL(vaultName)), same nil options.
const testEndpointOverrideEnv = "KVSYNK8S_KEYVAULT_TEST_ENDPOINT"

// clientFor returns the cached secretsClient for vaultName, creating and
// caching one if this is the first time this vault is used.
func (r *keyVaultReader) clientFor(vaultName string) (secretsClient, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if c, ok := r.clients[vaultName]; ok {
		return c, nil
	}

	endpoint := vaultURL(vaultName)
	var opts *azsecrets.ClientOptions
	if override := os.Getenv(testEndpointOverrideEnv); override != "" {
		endpoint = override
		opts = &azsecrets.ClientOptions{DisableChallengeResourceVerification: true}
	}

	c, err := azsecrets.NewClient(endpoint, r.credential, opts)
	if err != nil {
		// Client construction failure (bad endpoint/credential wiring) is not
		// one of the classified vault-response outcomes, but GetLatest's
		// contract still requires every error to wrap a sentinel so callers
		// can classify it with errors.Is. Treat it as retryable rather than
		// leaving it unclassified.
		return nil, fmt.Errorf("vault %q: create key vault client: %w: %w", vaultName, ErrTransient, err)
	}
	r.clients[vaultName] = c
	return c, nil
}

// GetLatest implements SecretReader.
func (r *keyVaultReader) GetLatest(ctx context.Context, vaultName, secretName string) (string, string, error) {
	client, err := r.clientFor(vaultName)
	if err != nil {
		return "", "", err
	}

	// Empty version string means "latest" in the Key Vault REST API.
	resp, err := client.GetSecret(ctx, secretName, "", nil)
	if err != nil {
		return "", "", classifyGetSecretError(vaultName, secretName, err)
	}

	if secretIsDisabled(resp.Attributes) {
		return "", "", fmt.Errorf("vault %q secret %q: %w", vaultName, secretName, ErrSecretDisabled)
	}

	if resp.Value == nil {
		// Key Vault returned a bundle with no value; treat like any other
		// unexpected upstream condition the caller should retry.
		return "", "", fmt.Errorf("vault %q secret %q: %w", vaultName, secretName, ErrTransient)
	}

	version := ""
	if resp.ID != nil {
		version = resp.ID.Version()
	}

	return *resp.Value, version, nil
}

// secretIsDisabled reports whether the secret attributes mark the secret as
// disabled. A nil Attributes or nil Enabled is treated as enabled (Key
// Vault's own default).
func secretIsDisabled(attrs *azsecrets.SecretAttributes) bool {
	return attrs != nil && attrs.Enabled != nil && !*attrs.Enabled
}

// classifyGetSecretError maps a GetSecret failure to one of the sentinel
// errors above. The returned message references only vaultName and
// secretName (identifiers, never values) plus the fixed sentinel text —
// never the underlying SDK error's own message, which may echo response
// details we do not want to have to audit (constitution I).
func classifyGetSecretError(vaultName, secretName string, err error) error {
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		// Key Vault refuses GetSecret outright on a disabled secret instead
		// of returning a normal 200 body with attributes.enabled=false (that
		// shape only ever comes back from an already-successful GetLatest,
		// via secretIsDisabled below, for callers that reach it some other
		// way). The refusal is observed as a 403 on real Key Vault and as a
		// 404 against the Lowkey Vault emulator (T030) -- neither status
		// code alone is a reliable signal, so this checks the server's own
		// wording instead of guessing from the status code. Nothing here
		// echoes that wording into the error this function returns
		// (constitution I): it is only ever used to pick which fixed
		// sentinel applies.
		if isDisabledSecretResponse(respErr) {
			return fmt.Errorf("vault %q secret %q: %w", vaultName, secretName, ErrSecretDisabled)
		}
		switch {
		case respErr.StatusCode == http.StatusNotFound:
			return fmt.Errorf("vault %q secret %q: %w", vaultName, secretName, ErrSecretNotFound)
		case respErr.StatusCode == http.StatusUnauthorized || respErr.StatusCode == http.StatusForbidden:
			return fmt.Errorf("vault %q secret %q: %w", vaultName, secretName, ErrAccessDenied)
		case respErr.StatusCode == http.StatusTooManyRequests || respErr.StatusCode >= http.StatusInternalServerError:
			return fmt.Errorf("vault %q secret %q: %w", vaultName, secretName, ErrTransient)
		default:
			// Any other HTTP status from Key Vault is not one of the known,
			// actionable categories. Fail safe: treat it as retryable rather
			// than surfacing a new, unhandled reason to callers.
			return fmt.Errorf("vault %q secret %q: %w", vaultName, secretName, ErrTransient)
		}
	}

	// No HTTP response at all: network failure, timeout, DNS, TLS, etc.
	return fmt.Errorf("vault %q secret %q: %w", vaultName, secretName, ErrTransient)
}

// isDisabledSecretResponse reports whether respErr represents Key Vault
// refusing a get because the secret (or the specific version requested) is
// disabled. respErr.Error() renders the cached response body -- read only
// to classify the failure, never forwarded into this package's own error
// text.
func isDisabledSecretResponse(respErr *azcore.ResponseError) bool {
	return strings.Contains(strings.ToLower(respErr.Error()), "disabled")
}
