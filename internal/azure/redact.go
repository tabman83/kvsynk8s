// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"errors"
	"net/url"
)

// UnparseableURLPlaceholder stands in for a URL that net/url cannot parse. A
// malformed URL can still be a SAS URL with a typo in it, so the raw value is
// never echoed back: the operator gets a marker and looks at their own
// configuration instead.
const UnparseableURLPlaceholder = "<unparseable>"

// RedactedQueryMarker replaces a URL's query string in log and error output.
// It is appended rather than simply dropped so the text still tells an
// operator that the URL they configured had a query string: without it they
// would read back a bare URL that does not match what they set and think the
// flag was ignored.
const RedactedQueryMarker = "?<redacted>"

// RedactURL reduces a URL to the part that is safe to surface -- scheme, host
// and path -- and reports whether the original carried a query string.
//
// Azure hands Storage Queue URLs out with a SAS token in the query string
// (?sv=...&sig=...), and NewQueueSource passes whatever it is given straight
// to azqueue without parsing it, so a SAS URL is accepted as input. The
// signature in it is a live bearer credential for the queue, and the
// operator's stdout usually reaches a log aggregator with far wider read
// access than the queue itself -- so it is stripped here (constitution I's
// redaction rule is about synced secret *values*, but the same reasoning
// applies to any credential we would otherwise print). Userinfo
// (user:password@host) goes for the same reason.
//
// The empty string maps to the empty string, not to a placeholder: no queue
// configured is a real, non-secret state of the config, and the log must stay
// honest about it.
func RedactURL(raw string) (safe string, hadQuery bool) {
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return UnparseableURLPlaceholder, false
	}
	hadQuery = u.RawQuery != "" || u.ForceQuery
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	// A fragment never reaches the server, so it cannot itself be a working
	// credential, but a pasted token can end up in one -- drop it too.
	u.Fragment = ""
	u.RawFragment = ""
	safe = u.String()
	if hadQuery {
		safe += RedactedQueryMarker
	}
	return safe, hadQuery
}

// redactURLError rebuilds a *url.Error with its URL reduced by RedactURL, so
// that wrapping the result into a message the listener logs cannot leak a SAS
// token.
//
// This is the one error shape from azqueue that carries a query string.
// azcore's own *azcore.ResponseError deliberately prints only
// scheme/host/escaped-path (sdk/azcore/internal/exported/response_error.go),
// so every error that got as far as an HTTP status is already safe. What is
// not safe is everything that fails *before* a response exists: net/http
// returns a *url.Error whose text is the full request URL with the query
// string intact -- DNS failure, connection refused, TLS failure, timeout, and
// also http.NewRequestWithContext rejecting a URL net/url cannot parse. The
// listener retries a failed poll forever (internal/events/listener.go), so an
// unredacted one of these prints the SAS token every IdlePollInterval, not
// once.
//
// The rebuilt error deliberately replaces the whole chain above the
// *url.Error rather than wrapping it: azqueue returns the transport error
// unwrapped, so there is nothing above it to preserve, and keeping an outer
// wrapper would keep whatever text that wrapper had rendered from the same
// unredacted URL. errors.Is/As on the transport cause still work, because
// ue.Err is carried over untouched.
func redactURLError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	safe, _ := RedactURL(ue.URL)
	return &url.Error{Op: ue.Op, URL: safe, Err: ue.Err}
}
