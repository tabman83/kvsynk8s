// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azqueue/v2"
)

// sasSignature is a clearly-fake SAS signature. It stands for the one part of
// a Storage Queue URL that is a live bearer credential for the queue, and it
// must never appear in anything this package returns to a caller that logs it
// (Listener.Start logs Receive's error on every failed poll, and
// Listener.deleteMessage logs Delete's).
const sasSignature = "FAKEsignatureFAKEsignatureFAKE%3D"

// sasQueueURL is what azqueue puts into a *url.Error when a request never
// reaches the service: the full request URL, query string included.
const sasQueueURL = "https://mystorage.queue.core.windows.net/kvevents/messages?numofmessages=32&sig=" +
	sasSignature + "&sv=2024-11-04"

// bareQueueURL is that same URL with nothing left to redact.
const bareQueueURL = "https://mystorage.queue.core.windows.net/kvevents"

// bareQueueMessagesURL is what sasQueueURL redacts down to: azqueue appends
// the /messages sub-resource to the configured queue URL.
const bareQueueMessagesURL = bareQueueURL + "/messages"

// fakeQueueClient is a queueClient that fails both operations with a
// caller-supplied error, so the tests below can drive exactly the error shape
// the real SDK produces without a network.
type fakeQueueClient struct {
	err error
}

func (f *fakeQueueClient) DequeueMessages(context.Context, *azqueue.DequeueMessagesOptions) (azqueue.DequeueMessagesResponse, error) {
	return azqueue.DequeueMessagesResponse{}, f.err
}

func (f *fakeQueueClient) DeleteMessage(context.Context, string, string, *azqueue.DeleteMessageOptions) (azqueue.DeleteMessageResponse, error) {
	return azqueue.DeleteMessageResponse{}, f.err
}

// transportError is the error net/http hands back when a request never gets a
// response -- DNS failure, connection refused, TLS failure, timeout. azqueue
// returns it unwrapped, and its Error() text is the full request URL. This is
// the shape reproduced against the real SDK: dialing an unresolvable host with
// a SAS-bearing queue URL returns exactly this.
func transportError() error {
	return &url.Error{
		Op:  "Get",
		URL: sasQueueURL,
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("no such host")},
	}
}

// parseError is the other pre-HTTP *url.Error: azqueue.NewQueueClient never
// parses the URL it is given, so a URL net/url rejects is accepted at
// construction and only fails later, inside http.NewRequestWithContext, with
// the raw value in the message.
func parseError() error {
	return &url.Error{
		Op:  "parse",
		URL: "ht\ttps://mystorage.queue.core.windows.net/kvevents?sig=" + sasSignature,
		Err: errors.New("net/url: invalid control character in URL"),
	}
}

func TestStorageQueueSource_ErrorsNeverCarryTheSASToken(t *testing.T) {
	tests := []struct {
		name string
		err  error
		// wantContains is the safe remainder that must survive redaction, so
		// the error still tells an operator which queue failed.
		wantContains string
	}{
		{"transport failure", transportError(), bareQueueMessagesURL},
		{"unparseable URL", parseError(), UnparseableURLPlaceholder},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &storageQueueSource{client: &fakeQueueClient{err: tt.err}}

			_, receiveErr := s.Receive(context.Background(), 32)
			deleteErr := s.Delete(context.Background(), "msg-1", "pop-1")

			for label, got := range map[string]error{"Receive": receiveErr, "Delete": deleteErr} {
				if got == nil {
					t.Fatalf("%s() = nil, want an error", label)
				}
				if strings.Contains(got.Error(), sasSignature) {
					t.Errorf("%s() = %q, must not contain the SAS signature %q", label, got, sasSignature)
				}
				if !strings.Contains(got.Error(), tt.wantContains) {
					t.Errorf("%s() = %q, want it to still contain %q", label, got, tt.wantContains)
				}
			}
		})
	}
}

// TestStorageQueueSource_ErrorsStayInspectable guards the redaction against
// the obvious over-correction: replacing the error outright would also drop
// the cause, and the listener's health gauges and any future retry
// classification need errors.Is/As to keep working.
func TestStorageQueueSource_ErrorsStayInspectable(t *testing.T) {
	cause := errors.New("no such host")
	s := &storageQueueSource{client: &fakeQueueClient{
		err: &url.Error{Op: "Get", URL: sasQueueURL, Err: cause},
	}}

	_, err := s.Receive(context.Background(), 32)
	if !errors.Is(err, cause) {
		t.Errorf("Receive() = %v, want it to still wrap the transport cause", err)
	}
	var ue *url.Error
	if !errors.As(err, &ue) {
		t.Fatalf("Receive() = %v, want it to still be a *url.Error", err)
	}
	if ue.URL != bareQueueMessagesURL+RedactedQueryMarker {
		t.Errorf("Receive() url.Error.URL = %q, want %q", ue.URL, bareQueueMessagesURL+RedactedQueryMarker)
	}
}

// TestStorageQueueSource_NonURLErrorsPassThrough: an error that is not a
// *url.Error carries no URL of ours, so redaction must leave it alone rather
// than silently rewriting SDK diagnostics.
func TestStorageQueueSource_NonURLErrorsPassThrough(t *testing.T) {
	cause := errors.New("some SDK failure")
	s := &storageQueueSource{client: &fakeQueueClient{err: cause}}

	_, err := s.Receive(context.Background(), 32)
	if !errors.Is(err, cause) {
		t.Fatalf("Receive() = %v, want it to wrap %v", err, cause)
	}
	if !strings.Contains(err.Error(), cause.Error()) {
		t.Errorf("Receive() = %q, want it to contain %q", err, cause.Error())
	}
}

func TestRedactURL(t *testing.T) {
	const redactedQueueURL = bareQueueURL + RedactedQueryMarker

	tests := []struct {
		name string
		raw  string
		want string
		// wantHadQuery is what cmd/main.go uses to decide whether to warn that
		// a SAS token was supplied; it must not fire on a bare URL.
		wantHadQuery bool
		// mustNotContain, when set, must not appear in the result.
		mustNotContain string
	}{
		{
			name: "bare URL is unchanged",
			raw:  bareQueueURL,
			want: bareQueueURL,
		},
		{
			name:           "SAS query string is redacted",
			raw:            bareQueueURL + "?sv=2024-11-04&ss=q&sig=" + sasSignature,
			want:           redactedQueueURL,
			wantHadQuery:   true,
			mustNotContain: sasSignature,
		},
		{
			name:           "userinfo is stripped",
			raw:            "https://account:" + sasSignature + "@mystorage.queue.core.windows.net/kvevents",
			want:           bareQueueURL,
			mustNotContain: sasSignature,
		},
		{
			name:           "userinfo and query are both stripped",
			raw:            "https://account:hunter2@mystorage.queue.core.windows.net/kvevents?sig=" + sasSignature,
			want:           redactedQueueURL,
			wantHadQuery:   true,
			mustNotContain: sasSignature,
		},
		{
			name:           "fragment is stripped",
			raw:            bareQueueURL + "#" + sasSignature,
			want:           bareQueueURL,
			mustNotContain: sasSignature,
		},
		{
			name:         "empty query marker still reports a query",
			raw:          bareQueueURL + "?",
			want:         redactedQueueURL,
			wantHadQuery: true,
		},
		{
			name:           "unparseable URL yields a placeholder, never the raw value",
			raw:            "ht\ttp://mystorage.queue.core.windows.net/kvevents?sig=" + sasSignature,
			want:           UnparseableURLPlaceholder,
			mustNotContain: sasSignature,
		},
		{
			// No queue configured is a real, non-secret state of the config:
			// it must stay empty so the log does not claim a broken URL was
			// supplied when none was.
			name: "empty stays empty",
			raw:  "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, hadQuery := RedactURL(tt.raw)
			if got != tt.want {
				t.Errorf("RedactURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
			if hadQuery != tt.wantHadQuery {
				t.Errorf("RedactURL(%q) hadQuery = %v, want %v", tt.raw, hadQuery, tt.wantHadQuery)
			}
			if tt.mustNotContain != "" && strings.Contains(got, tt.mustNotContain) {
				t.Errorf("RedactURL(%q) = %q, must not contain %q", tt.raw, got, tt.mustNotContain)
			}
		})
	}
}
