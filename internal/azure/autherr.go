// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"errors"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// nonRetriable is azcore's own marker interface for an error the SDK will not
// retry, re-declared here because the package that defines it
// (github.com/Azure/azure-sdk-for-go/sdk/internal/errorinfo) is under an
// internal/ path scoped to the SDK's own modules and cannot be imported from
// this repository.
//
// It is what makes credential-chain failures detectable at all.
// azidentity's chained credential returns one of exactly two shapes: an
// exported *AuthenticationFailedError (some credential failed for a real
// reason — the broken-federation and wrong-client-ID cases), or an
// UNexported credentialUnavailableError (every credential in the chain was
// unavailable — no annotation, no projected token, no IMDS). The second one
// cannot be matched by type or by its own interface, since both are
// unexported, but it does carry an exported-name NonRetriable() method.
type nonRetriable interface {
	error
	NonRetriable()
}

// Auth-failure kinds, reported on the log line for an ErrAuthFailure. They are
// derived from the error TYPE, never from its text, which is what makes them
// safe to log: see authFailureKind.
const (
	// authKindUnavailable: no credential in the chain could even attempt a
	// token request. In-cluster that means workload identity is not wired up
	// at all -- no azure.workload.identity/client-id annotation, or no
	// projected service-account token.
	authKindUnavailable = "credential-unavailable"
	// authKindRequestFailed: a credential tried and did not get a token. A
	// wrong client ID, a federated credential whose subject or issuer does not
	// match, or no egress to the Entra ID endpoint.
	authKindRequestFailed = "token-request-failed"
)

// authFailureKind reports which half of the credential chain failed, or "" if
// err is not an auth failure at all.
//
// The two kinds have completely different fixes, and this is as far as
// classification can honestly go without rendering the SDK's own error text.
// That text is deliberately never logged: an auth failure happens BEFORE any
// response exists, which is exactly the class redact.go warns about -- a
// transport error at that stage renders the full request URL, query string
// included. Reporting the kind from the error's type carries no upstream
// characters at all.
func authFailureKind(err error) string {
	if !isAuthFailure(err) {
		return ""
	}
	var authFailed *azidentity.AuthenticationFailedError
	if errors.As(err, &authFailed) {
		return authKindRequestFailed
	}
	return authKindUnavailable
}

// isAuthFailure reports whether err is the Azure credential chain failing to
// produce a token, rather than a service answering the request.
//
// Callers MUST check for an *azcore.ResponseError first. Key Vault's own
// answer always wins: a 403 from the vault is AccessDenied, and that is a
// completely different problem from never having got a token. This function
// only ever sees errors that carried no HTTP response of their own.
//
// Why the two checks are not one: azcore's bearer-token policy wraps whatever
// the credential returned in errorinfo.NonRetriableError, twice, and that
// wrapper implements Unwrap, so errors.As reaches through it to either shape.
func isAuthFailure(err error) bool {
	var authFailed *azidentity.AuthenticationFailedError
	if errors.As(err, &authFailed) {
		// The token endpoint answered, and it answered with something that
		// clears on its own: Entra ID having a bad day, or IMDS throttling.
		// azidentity funnels those through the same error type as a rejected
		// assertion, so without this check an Entra outage would be reported
		// as broken federation and send an operator auditing a federated
		// credential that is perfectly fine.
		//
		// Checked before the marker interface below, not instead of it:
		// *AuthenticationFailedError implements NonRetriable() too, so simply
		// falling through would match it there anyway.
		if r := authFailed.RawResponse; r != nil &&
			(r.StatusCode == http.StatusTooManyRequests || r.StatusCode >= http.StatusInternalServerError) {
			return false
		}
		return true
	}
	var unavailable nonRetriable
	return errors.As(err, &unavailable)
}
