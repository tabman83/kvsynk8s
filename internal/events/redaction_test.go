// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

// T025/T028 (US4, constitution I, FR-010, SC-004): a queue message never
// carries a secret value by design (contracts/queue-message.md "Non-goals"),
// so there is structurally nothing for this package to leak the way
// internal/sync can. What this suite proves instead is the T028 logging
// convention: every log call the Listener makes, across every processing
// path (malformed, poison, unmatched, matched-and-dispatched), uses only
// keys from a fixed allow-list -- never anything value-shaped -- and never
// echoes raw message body content even when that body contains something
// that looks exactly like a leaked secret.
package events

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/tabman83/kvsynk8s/internal/azure"
)

// allowedLogKeys is the fixed set of structured-log keys this package (and
// internal/sync/writer.go, mirrored there) may ever use, per T028. Every key
// here is an identifier or a count, never anything that could hold a secret
// value. matches/reason are this package's own necessary additions beyond
// the base list in tasks.md (a dispatch count, and a fixed discard-reason
// string) -- both identifiers/metadata, never value-shaped.
var allowedLogKeys = map[string]bool{
	"vault":        true,
	"secret":       true,
	"version":      true,
	"namespace":    true,
	"name":         true,
	"eventID":      true,
	"messageID":    true,
	"dequeueCount": true,
	"matches":      true,
	"reason":       true,
}

// recordedLogCall captures one Info/Error call a recordingLogSink observed.
type recordedLogCall struct {
	msg            string
	err            error
	keysAndValues  []any
	prefixedValues []any // WithValues accumulation, prepended ahead of keysAndValues
}

// allArgs returns the full flattened key/value sequence for this call
// (prefixedValues from any WithValues chain, then this call's own
// keysAndValues), exactly as a real sink would receive them.
func (c recordedLogCall) allArgs() []any {
	out := make([]any, 0, len(c.prefixedValues)+len(c.keysAndValues))
	out = append(out, c.prefixedValues...)
	out = append(out, c.keysAndValues...)
	return out
}

// recordingLogSink is a minimal hand-written logr.LogSink (no mocking
// framework, matching this codebase's style) that records every Info/Error
// call into calls, shared by pointer across any WithName/WithValues
// derivative so every call made through the context logger ends up in one
// place regardless of which derived logger produced it.
type recordingLogSink struct {
	calls  *[]recordedLogCall
	prefix []any
}

func newRecordingLogger() (logr.Logger, *[]recordedLogCall) {
	calls := &[]recordedLogCall{}
	sink := &recordingLogSink{calls: calls}
	return logr.New(sink), calls
}

func (s *recordingLogSink) Init(logr.RuntimeInfo) {}
func (s *recordingLogSink) Enabled(int) bool      { return true }

func (s *recordingLogSink) Info(_ int, msg string, keysAndValues ...any) {
	*s.calls = append(*s.calls, recordedLogCall{msg: msg, keysAndValues: keysAndValues, prefixedValues: s.prefix})
}

func (s *recordingLogSink) Error(err error, msg string, keysAndValues ...any) {
	*s.calls = append(*s.calls, recordedLogCall{msg: msg, err: err, keysAndValues: keysAndValues, prefixedValues: s.prefix})
}

func (s *recordingLogSink) WithValues(keysAndValues ...any) logr.LogSink {
	next := append(append([]any{}, s.prefix...), keysAndValues...)
	return &recordingLogSink{calls: s.calls, prefix: next}
}

func (s *recordingLogSink) WithName(string) logr.LogSink { return s }

var _ logr.LogSink = (*recordingLogSink)(nil)

// assertOnlyAllowedKeys fails t if any recorded call used a key outside
// allowedLogKeys, or if any recorded message/error/value contains sentinel.
func assertRedactedAndAllowedKeys(t *testing.T, calls []recordedLogCall, sentinel string) {
	t.Helper()
	for _, c := range calls {
		if strings.Contains(c.msg, sentinel) {
			t.Fatalf("log message leaked the sentinel: %q", c.msg)
		}
		if c.err != nil && strings.Contains(c.err.Error(), sentinel) {
			t.Fatalf("logged error leaked the sentinel: %q", c.err.Error())
		}
		args := c.allArgs()
		for i := 0; i+1 < len(args); i += 2 {
			key, _ := args[i].(string)
			if !allowedLogKeys[key] {
				t.Fatalf("log call %q used a key outside the allow-list: %q (full args: %+v)", c.msg, key, args)
			}
			val := fmt.Sprintf("%v", args[i+1])
			if strings.Contains(val, sentinel) {
				t.Fatalf("log call %q leaked the sentinel via key %q: %q", c.msg, key, val)
			}
		}
	}
}

// TestListener_LogRedaction_AcrossEveryPath drives handleMessage through
// malformed, poison, unmatched, and matched-and-dispatched messages, with a
// SENTINEL-shaped marker planted directly in the raw (undecodable) message
// body -- the one place closest to looking like leaked content this package
// ever touches -- and asserts nothing logged anywhere ever contains it, and
// every logged key is on the allow-list (T025, T028, SC-004).
func TestListener_LogRedaction_AcrossEveryPath(t *testing.T) {
	const sentinel = "SENTINEL-events-log-redaction-4b7e21"

	matchA := newSecretSync("ns-a", "sync-a", testVault, testObject)
	cli := fakeClientWith(t, matchA)

	malformed := azure.QueueMessage{
		ID: "msg-malformed", PopReceipt: "pop-malformed",
		Text: "not-valid-base64!!! " + sentinel, DequeueCount: 1,
	}
	poison := azure.QueueMessage{
		ID: "msg-redact-poison", PopReceipt: "pop-redact-poison",
		Text:         string(encodedBody(eventPayload(testEventID, newVersionET, testVault, secretType, testObject, testVersion))),
		DequeueCount: 6,
	}
	unmatched := azure.QueueMessage{
		ID: "msg-unmatched", PopReceipt: "pop-unmatched",
		Text:         string(encodedBody(eventPayload(testEventID, newVersionET, "no-such-vault", secretType, testObject, testVersion))),
		DequeueCount: 1,
	}
	matched := azure.QueueMessage{
		ID: "msg-matched", PopReceipt: "pop-matched",
		Text:         string(encodedBody(eventPayload(testEventID, newVersionET, testVault, secretType, testObject, testVersion))),
		DequeueCount: 1,
	}

	queue := newFakeQueueSource([]azure.QueueMessage{malformed, poison, unmatched, matched})
	events := make(chan event.GenericEvent, 10)
	l := NewListener(queue, cli, events)

	logger, calls := newRecordingLogger()
	ctx := logf.IntoContext(context.Background(), logger)

	if _, err := l.pollOnce(ctx); err != nil {
		t.Fatalf("pollOnce() error = %v, want nil", err)
	}

	// Drain whatever was dispatched so the test doesn't depend on channel
	// capacity/ordering; the point of this test is the logs, not the events.
	for {
		select {
		case <-events:
			continue
		default:
		}
		break
	}

	assertRedactedAndAllowedKeys(t, *calls, sentinel)

	if len(*calls) == 0 {
		t.Fatal("expected at least one log call across malformed/poison/unmatched/matched paths")
	}
}
