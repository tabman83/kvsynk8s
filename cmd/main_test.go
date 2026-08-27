// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
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
//
// It registers the flag through registerQueueURLFlag -- the same function
// main() uses -- rather than declaring its own StringVar: a helper that
// hardcoded the empty default would be asserting against itself, and the
// registration is exactly where the SAS default could come back.
func withParsedQueueURLFlag(t *testing.T, supplied bool, value string) {
	t.Helper()
	saved := flag.CommandLine
	t.Cleanup(func() { flag.CommandLine = saved })

	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	var dst string
	registerQueueURLFlag(flag.CommandLine, &dst)

	var args []string
	if supplied {
		args = []string{"--queue-url=" + value}
	}
	if err := flag.CommandLine.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
}

// TestQueueURLFlagHasNoDefault guards the flag registration itself, which is
// the other half of the leak: Go's flag package stores a flag's default as
// DefValue and PrintDefaults writes it verbatim, so registering --queue-url
// with os.Getenv("QUEUE_URL") as its default puts a SAS token on stderr for
// `--help` and for every flag parse error (flag.CommandLine is ExitOnError, so
// a mistyped flag calls Usage). Nothing else in the repo -- no test, no
// hack/*.sh, no e2e spec -- looks at DefValue or PrintDefaults, so without this
// case that one-token regression stays green.
func TestQueueURLFlagHasNoDefault(t *testing.T) {
	t.Setenv("QUEUE_URL", sasQueueURL)

	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	var dst string
	registerQueueURLFlag(fs, &dst)

	if got := fs.Lookup("queue-url").DefValue; got != "" {
		t.Errorf("--queue-url DefValue = %q, want %q: the default is printed verbatim by --help", got, "")
	}

	// Belt and braces, and the assertion that stays honest if the default ever
	// moves somewhere else in the usage text: render the usage the operator
	// would actually see and require the credential not to be in it.
	var buf bytes.Buffer
	fs.SetOutput(&buf)
	fs.PrintDefaults()
	if strings.Contains(buf.String(), sasSignature) {
		t.Errorf("PrintDefaults() = %q, must not contain the SAS signature from QUEUE_URL", buf.String())
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

// TestDurationFromEnv covers the RECONCILE_INTERVAL fallback, which README.md
// documents and which manifest-install users configure the operator through.
// Its sibling QUEUE_URL has had dedicated cases since it was written; this one
// had none, so a refactor dropping the env read — or breaking the
// empty/unparseable fallback — would have left every test in the repo green
// while every operator that set RECONCILE_INTERVAL silently ran at 4h instead.
func TestDurationFromEnv(t *testing.T) {
	const key = "RECONCILE_INTERVAL"
	def := 4 * time.Hour

	tests := []struct {
		name string
		set  bool
		env  string
		want time.Duration
	}{
		{name: "unset falls back to the default", want: def},
		{name: "empty falls back to the default", set: true, env: "", want: def},
		{name: "unparseable falls back to the default", set: true, env: "4 hours", want: def},
		{name: "bare number is not a duration", set: true, env: "30", want: def},
		{name: "a valid duration is honoured", set: true, env: "30m", want: 30 * time.Minute},
		{name: "a compound duration is honoured", set: true, env: "1h30m", want: 90 * time.Minute},
		// Parsed as given: the sign check lives in effectiveReconcileInterval,
		// which is what the startup line and the reconciler both go through.
		{name: "zero parses to zero", set: true, env: "0s", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv(key, tt.env)
			} else {
				// t.Setenv restores the previous value on cleanup, so an unset
				// case has to clear it the same way rather than assuming the
				// test process never had one.
				t.Setenv(key, "")
			}
			if got := durationFromEnv(key, def); got != tt.want {
				t.Errorf("durationFromEnv(%q=%q) = %v, want %v", key, tt.env, got, tt.want)
			}
		})
	}
}

// TestEffectiveReconcileInterval pins the other half: a value the controller
// cannot honour is replaced HERE, so the "Operator configuration" line reports
// the cadence the reconciler actually runs at. Before this, --reconcile-interval=0
// (a common way of asking for "off") and a typo'd -4h were both accepted,
// echoed back as configured, and then silently replaced with 4h by
// internal/controller.
func TestEffectiveReconcileInterval(t *testing.T) {
	tests := []struct {
		name            string
		configured      time.Duration
		want            time.Duration
		wantSubstituted bool
	}{
		{name: "a positive interval is used as given", configured: 30 * time.Minute, want: 30 * time.Minute},
		{name: "the default is positive and passes through", configured: defaultReconcileInterval, want: defaultReconcileInterval},
		{name: "zero is replaced", configured: 0, want: defaultReconcileInterval, wantSubstituted: true},
		{name: "negative is replaced", configured: -4 * time.Hour, want: defaultReconcileInterval, wantSubstituted: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, substituted := effectiveReconcileInterval(tt.configured)
			if got != tt.want || substituted != tt.wantSubstituted {
				t.Errorf("effectiveReconcileInterval(%v) = (%v, %t), want (%v, %t)",
					tt.configured, got, substituted, tt.want, tt.wantSubstituted)
			}
		})
	}
}

// TestConfigLogKeyValues_ReportsTheEffectiveInterval is the reason the
// substitution happens in main() rather than being left to the reconciler: the
// startup line is built from the same variable the reconciler is handed, so
// normalizing before it is built is what keeps the two in agreement. A future
// change that logged the raw flag and normalized later would pass every case
// above and still print a cadence nothing runs at.
func TestConfigLogKeyValues_ReportsTheEffectiveInterval(t *testing.T) {
	configured := -4 * time.Hour
	effective, substituted := effectiveReconcileInterval(configured)
	if !substituted {
		t.Fatalf("effectiveReconcileInterval(%v) reported no substitution", configured)
	}

	kvs := configLogKeyValues("", effective, "")
	if !containsValue(kvs, "reconcileInterval", effective) {
		t.Errorf("configLogKeyValues() = %v, want reconcileInterval=%v (what the controller will use)", kvs, effective)
	}
	if containsValue(kvs, "reconcileInterval", configured) {
		t.Errorf("configLogKeyValues() reported the rejected value %v as the operator configuration", configured)
	}
}
