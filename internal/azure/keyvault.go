// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

// Package azure holds the thin wrappers around the Azure SDK for Go that the
// sync engine and event listener depend on through small interfaces
// (SecretReader here, QueueSource in queue.go).
package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	azruntime "github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
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

	// ErrAuthFailure means no Azure token could be acquired at all, so the
	// request never reached Key Vault: workload identity is not wired up
	// (federated credential subject mismatch, a missing
	// azure.workload.identity/client-id annotation, no projected token). Maps
	// to status.reason AuthenticationFailed.
	//
	// Deliberately distinct from ErrAccessDenied, which means the opposite
	// half of the same sentence: the token WAS accepted and the identity
	// simply lacks the role assignment on the vault. The two have completely
	// different fixes — one is in the cluster, one is in Azure RBAC — and
	// before this existed both landed on ErrTransient, whose documented advice
	// is "usually nothing, the controller retries". That advice is wrong for
	// every failure this sentinel covers.
	ErrAuthFailure = errors.New("could not acquire an Azure token")

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
		return "", "", classifyGetSecretError(ctx, vaultName, secretName, err)
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
//
// It also emits the one diagnostic line an operator needs and could not
// previously get anywhere. The engine drops this error after classifying it
// (engine.go's classifyReaderError uses it for errors.Is dispatch and returns
// only a fixed message), so if this function does not log the HTTP status
// code, nothing does: a SecretSync sits at Failing/TransientError with no way
// to tell a 429 from a 500 from a DNS failure. Three rules that log call has
// to keep:
//
//   - The first argument to Error is nil, never err. logr renders a non-nil
//     error's own string, and that string is the upstream text this package
//     refuses to echo. cmd/main.go uses the same Error(nil, ...) idiom for
//     the same reason.
//   - Only the status code — an int — and, for an auth failure, a fixed
//     kind literal cross the boundary. Nothing from the response body or from
//     the SDK error's own text does, exactly as isDisabledSecretResponse below
//     already reads the body to classify and never forwards it.
//   - This detail goes to the operator log and deliberately NOT into
//     status.Message, which is bound by the identifiers-and-fixed-text rule
//     because anyone with read access to the CR can see it.
func classifyGetSecretError(ctx context.Context, vaultName, secretName string, err error) error {
	sentinel, statusCode := classifySentinel(err)

	keys := []any{
		"vault", vaultName, "secret", secretName,
		"statusCode", statusCode, "classification", classificationName(sentinel),
	}
	// On an auth failure there is no status code to report (the request never
	// went out), so the log would otherwise say only "AuthenticationFailed"
	// and leave an operator to guess between a missing annotation and a
	// mismatched federated credential. authFailureKind answers that from the
	// error's TYPE, carrying no upstream characters -- see its comment for why
	// the SDK's own text is not logged instead.
	if kind := authFailureKind(err); kind != "" {
		keys = append(keys, "authFailure", kind)
	}

	logf.FromContext(ctx).Error(nil, "key vault read failed", keys...)

	return fmt.Errorf("vault %q secret %q: %w", vaultName, secretName, sentinel)
}

// classificationName renders a sentinel as the status.reason a caller will end
// up with, so the log line and `kubectl get secretsync` agree on what happened.
func classificationName(sentinel error) string {
	switch {
	case errors.Is(sentinel, ErrSecretNotFound):
		return "SecretNotFound"
	case errors.Is(sentinel, ErrAccessDenied):
		return "AccessDenied"
	case errors.Is(sentinel, ErrAuthFailure):
		return "AuthenticationFailed"
	case errors.Is(sentinel, ErrSecretDisabled):
		return "SourceDisabled"
	default:
		return "TransientError"
	}
}

// classifySentinel picks the sentinel for a GetSecret failure and reports the
// HTTP status code that decided it, or 0 when the request never got an answer.
// Split out from classifyGetSecretError so the classification and the logging
// of it cannot drift apart.
func classifySentinel(err error) (sentinel error, statusCode int) {
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		// Key Vault refuses GetSecret outright on a disabled secret instead
		// of returning a normal 200 body with attributes.enabled=false (that
		// shape only ever comes back from an already-successful GetLatest,
		// via secretIsDisabled below, for callers that reach it some other
		// way). The refusal is observed as a 403 on real Key Vault and as a
		// 404 against the Lowkey Vault emulator (T030) -- neither status
		// code alone is a reliable signal, so this checks the structured
		// error fields in the response body instead of guessing from the
		// status code. Nothing from that body is ever echoed into the error
		// this function returns (constitution I): it is only ever used to
		// pick which fixed sentinel applies.
		if isDisabledSecretResponse(respErr) {
			return ErrSecretDisabled, respErr.StatusCode
		}
		switch {
		case respErr.StatusCode == http.StatusNotFound:
			return ErrSecretNotFound, respErr.StatusCode
		case respErr.StatusCode == http.StatusUnauthorized || respErr.StatusCode == http.StatusForbidden:
			return ErrAccessDenied, respErr.StatusCode
		case respErr.StatusCode == http.StatusTooManyRequests || respErr.StatusCode >= http.StatusInternalServerError:
			return ErrTransient, respErr.StatusCode
		default:
			// Any other HTTP status from Key Vault is not one of the known,
			// actionable categories. Fail safe: treat it as retryable rather
			// than surfacing a new, unhandled reason to callers.
			return ErrTransient, respErr.StatusCode
		}
	}

	// No HTTP response from Key Vault. Before anything else, ask whether we
	// ever got a token: an unwired workload identity fails here, and calling
	// that "transient" tells the operator to wait for a thing that will never
	// happen. Checked only after the ResponseError branch above, so a real
	// answer from the vault always wins.
	if isAuthFailure(err) {
		return ErrAuthFailure, 0
	}

	// Genuinely no answer: network failure, timeout, DNS, TLS, etc.
	return ErrTransient, 0
}

// keyVaultErrorBody is the JSON error shape Key Vault (and Key-Vault-
// compatible emulators) return on a failed request:
// {"error":{"code":...,"message":...,"innererror":{"code":...}}}. Only the
// fields classification needs are decoded.
type keyVaultErrorBody struct {
	Error struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		InnerError struct {
			Code string `json:"code"`
		} `json:"innererror"`
	} `json:"error"`
}

// isDisabledSecretResponse reports whether respErr represents Key Vault
// refusing a get because the secret (or the specific version requested) is
// disabled. It inspects the structured error fields of the cached response
// body -- deliberately NOT respErr.Error(), whose rendering includes the full
// request URL: a secret whose *name* happens to contain the word "disabled"
// (e.g. "feature-disabled-flag") would otherwise turn a plain 404 into a
// bogus SourceDisabled, and a plain 403 would lose AccessDenied.
//
// Real Key Vault marks the condition authoritatively with the inner error
// code "SecretDisabled" (on its HTTP 403 refusal). The Lowkey Vault emulator
// (T030) answers 404 with no inner code, so the fixed phrase of the
// disabled-refusal message ("... is not allowed on a disabled ...") is
// accepted as a fallback marker -- the whole phrase, never the bare word
// "disabled", so a secret name echoed inside an unrelated error message
// cannot trigger it. The body is read only to classify the failure, never
// forwarded into this package's own error text (constitution I).
func isDisabledSecretResponse(respErr *azcore.ResponseError) bool {
	if respErr.RawResponse == nil {
		return false
	}
	body, err := azruntime.Payload(respErr.RawResponse)
	if err != nil || len(body) == 0 {
		return false
	}
	var kvErr keyVaultErrorBody
	if json.Unmarshal(body, &kvErr) != nil {
		return false
	}
	if kvErr.Error.InnerError.Code == "SecretDisabled" {
		return true
	}
	return strings.Contains(strings.ToLower(kvErr.Error.Message), "not allowed on a disabled")
}
