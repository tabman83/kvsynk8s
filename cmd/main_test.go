// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"
)

// sasSignature is a clearly-fake SAS signature. It is the substring every case
// below asserts is absent from the log arguments main() builds: asserting only
// on the expected string would still pass if the raw URL were appended
// somewhere as well, so each case checks the credential is gone too.
const sasSignature = "FAKEsignatureFAKEsignatureFAKE%3D"

const (
	bareQueueURL     = "https://mystorage.queue.core.windows.net/kvevents"
	sasQueueURL      = bareQueueURL + "?sv=2024-11-04&ss=q&sig=" + sasSignature
	redactedQueueURL = bareQueueURL + "?<redacted>"
)

// renderKeyValues flattens logr-style key/value pairs the way a log sink would
// render them, so a leak in any one of them (key or value, string or not) is
// caught by a single substring check.
func renderKeyValues(kvs []any) string {
	parts := make([]string, 0, len(kvs))
	for _, kv := range kvs {
		parts = append(parts, fmt.Sprintf("%v", kv))
	}
	return strings.Join(parts, " ")
}

// TestConfigLogKeyValues is the test the "Operator configuration" line's
// arguments have to pass: whatever queue URL is configured, its SAS signature
// must not reach the log, and the host and path must, so the line is still
// useful to an operator checking their configuration.
func TestConfigLogKeyValues(t *testing.T) {
	tests := []struct {
		name     string
		queueURL string
		want     string
	}{
		{"bare URL is logged as configured", bareQueueURL, bareQueueURL},
		{"SAS URL is logged redacted", sasQueueURL, redactedQueueURL},
		{"no queue configured stays empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kvs := configLogKeyValues(tt.queueURL, 4*time.Hour, "11111111-2222-3333-4444-555555555555")
			rendered := renderKeyValues(kvs)

			if strings.Contains(rendered, sasSignature) {
				t.Errorf("configLogKeyValues(%q, ...) = %q, must not contain the SAS signature", tt.queueURL, rendered)
			}
			if !containsValue(kvs, queueURLLogKey, tt.want) {
				t.Errorf("configLogKeyValues(%q, ...) = %q, want queueURL=%q", tt.queueURL, rendered, tt.want)
			}
		})
	}
}

// TestQueueListenerLogKeyValues covers the SAS warning branch: that it fires
// for a URL with a query string, stays silent for one without, and that both
// it and the "Queue listener enabled" line log the redacted URL.
func TestQueueListenerLogKeyValues(t *testing.T) {
	tests := []struct {
		name     string
		queueURL string
		wantWarn bool
		wantURL  string
	}{
		{"bare URL does not warn", bareQueueURL, false, bareQueueURL},
		{"SAS URL warns", sasQueueURL, true, redactedQueueURL},
		{"empty query marker warns", bareQueueURL + "?", true, redactedQueueURL},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warn, enabled := queueListenerLogKeyValues(tt.queueURL)

			if got := warn != nil; got != tt.wantWarn {
				t.Errorf("queueListenerLogKeyValues(%q) warn != nil = %v, want %v", tt.queueURL, got, tt.wantWarn)
			}
			for label, kvs := range map[string][]any{"warn": warn, "enabled": enabled} {
				if kvs == nil {
					continue
				}
				rendered := renderKeyValues(kvs)
				if strings.Contains(rendered, sasSignature) {
					t.Errorf("queueListenerLogKeyValues(%q) %s = %q, must not contain the SAS signature",
						tt.queueURL, label, rendered)
				}
				if !containsValue(kvs, queueURLLogKey, tt.wantURL) {
					t.Errorf("queueListenerLogKeyValues(%q) %s = %q, want queueURL=%q",
						tt.queueURL, label, rendered, tt.wantURL)
				}
			}
		})
	}
}

// containsValue reports whether kvs holds key immediately followed by want.
func containsValue(kvs []any, key string, want any) bool {
	for i := 0; i+1 < len(kvs); i += 2 {
		if k, ok := kvs[i].(string); ok && k == key {
			return kvs[i+1] == want
		}
	}
	return false
}

// TestQueueURLFromEnv covers the reason --queue-url has no flag default: the
// env value is applied after flag.Parse so PrintDefaults (--help, and any flag
// parse error) has nothing to print. That only works if the fallback still
// behaves like a default in every other respect -- including letting an
// explicit empty --queue-url= mean "no queue" rather than silently picking up
// a set QUEUE_URL.
func TestQueueURLFromEnv(t *testing.T) {
	tests := []struct {
		name string
		// supplied is the flag's parsed value; suppliedOnCmdline is whether it
		// was actually given on the command line.
		supplied          string
		suppliedOnCmdline bool
		env               string
		want              string
	}{
		{name: "neither set", want: ""},
		{name: "env only", env: sasQueueURL, want: sasQueueURL},
		{name: "flag only", supplied: bareQueueURL, suppliedOnCmdline: true, want: bareQueueURL},
		{
			name:              "flag wins over env",
			supplied:          bareQueueURL,
			suppliedOnCmdline: true,
			env:               sasQueueURL,
			want:              bareQueueURL,
		},
		{
			name:              "explicit empty flag wins over env",
			supplied:          "",
			suppliedOnCmdline: true,
			env:               sasQueueURL,
			want:              "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("QUEUE_URL", tt.env)
			withParsedQueueURLFlag(t, tt.suppliedOnCmdline, tt.supplied)

			if got := queueURLFromEnv(tt.supplied); got != tt.want {
				t.Errorf("queueURLFromEnv(%q) = %q, want %q", tt.supplied, got, tt.want)
			}
		})
	}
}

// withParsedQueueURLFlag installs a fresh flag.CommandLine holding a
// --queue-url flag and parses it with or without that flag supplied, so
// queueURLFromEnv's flag.Visit lookup sees the real thing rather than a stub.
func withParsedQueueURLFlag(t *testing.T, supplied bool, value string) {
	t.Helper()
	saved := flag.CommandLine
	t.Cleanup(func() { flag.CommandLine = saved })

	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	var dst string
	flag.CommandLine.StringVar(&dst, "queue-url", "", "test")

	var args []string
	if supplied {
		args = []string{"--queue-url=" + value}
	}
	if err := flag.CommandLine.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
}

// TestSetupLogCallsNeverTakeTheRawQueueURL is the check that actually stands
// between this file's helpers and the bug they were written for: the helpers
// could be perfect and main() could still pass the raw queueURL to a log call
// beside them. It mirrors the static AST check in
// internal/sync/redaction_test.go -- parse main.go and fail if any setupLog
// call has an argument that references the `queueURL` identifier, which is the
// only variable in the file holding the unredacted, possibly SAS-bearing URL.
//
// Honest scope, same as the sync one: it inspects setupLog selector calls
// only, so a raw URL copied into another variable first, or reached through a
// helper that does not redact, would not be caught here. The helper tests
// above are the complementary check for that shape.
func TestSetupLogCallsNeverTakeTheRawQueueURL(t *testing.T) {
	// The name of the variable in main(), which happens to read the same as
	// queueURLLogKey but is a different thing -- rename the variable and this
	// constant has to follow, or the check quietly stops covering anything.
	const rawQueueURLIdent = "queueURL"

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	logSelectors := map[string]bool{"Info": true, "Error": true, "WithValues": true}
	seen := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !logSelectors[sel.Sel.Name] {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok || recv.Name != "setupLog" {
			return true
		}
		seen = true
		for _, arg := range call.Args {
			if referencesIdent(arg, rawQueueURLIdent) {
				pos := fset.Position(call.Pos())
				t.Errorf("main.go:%d: setupLog.%s(...) has an argument referencing the raw %q; "+
					"log the redacted form instead (configLogKeyValues/queueListenerLogKeyValues)",
					pos.Line, sel.Sel.Name, rawQueueURLIdent)
			}
		}
		return true
	})
	if !seen {
		t.Fatal("main.go: no setupLog.Info/Error/WithValues calls found at all -- " +
			"if logging moved elsewhere, this check must follow it")
	}
}

// referencesIdent reports whether expr is, or contains anywhere within it, a
// reference to the identifier name. Walking the whole sub-expression catches
// the identifier wrapped in a conversion, a call, or a fmt.Sprintf; matching
// on the parsed identifier rather than a substring of the source means it does
// not false-positive on names that merely contain the same letters, like
// safeQueueURL.
func referencesIdent(expr ast.Expr, name string) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		if ident, ok := n.(*ast.Ident); ok && ident.Name == name {
			found = true
			return false
		}
		return true
	})
	return found
}
