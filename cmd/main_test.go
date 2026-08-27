// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// sasSignature is a clearly-fake SAS signature. It is the substring every
// redaction case below asserts is absent from the log output: asserting only
// on the expected string would still pass if redactQueueURL started appending
// the raw URL somewhere, so each case checks the credential is gone as well
// (constitution I).
const sasSignature = "FAKEsignatureFAKEsignatureFAKE%3D"

// bareQueueURL is a queue URL with nothing to redact, and redactedQueueURL is
// what redactQueueURL is expected to make of any of the credential-bearing
// variants of it below.
const (
	bareQueueURL     = "https://mystorage.queue.core.windows.net/kvevents"
	redactedQueueURL = bareQueueURL + "?<redacted>"
)

func TestRedactQueueURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
		// wantHadQuery is what the caller uses to decide whether to warn that
		// a SAS token was supplied; it must not fire on a bare URL.
		wantHadQuery bool
		// mustNotContain, when set, must not appear anywhere in the result.
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
			want:           "<unparseable>",
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
			got, hadQuery := redactQueueURL(tt.raw)
			if got != tt.want {
				t.Errorf("redactQueueURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
			if hadQuery != tt.wantHadQuery {
				t.Errorf("redactQueueURL(%q) hadQuery = %v, want %v", tt.raw, hadQuery, tt.wantHadQuery)
			}
			if tt.mustNotContain != "" && strings.Contains(got, tt.mustNotContain) {
				t.Errorf("redactQueueURL(%q) = %q, must not contain %q", tt.raw, got, tt.mustNotContain)
			}
		})
	}
}
