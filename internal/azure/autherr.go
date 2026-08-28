// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"errors"

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
		return true
	}
	var unavailable nonRetriable
	return errors.As(err, &unavailable)
}
