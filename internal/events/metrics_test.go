// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

// Tests for the queue-receive health gauges (spec US3 acceptance scenario 3):
// pollOnce must count consecutive Receive failures and stamp the last
// successful Receive, exposed via the Prometheus gauges in metrics.go. The
// gauges make a degraded notification path visible without ever touching
// /healthz or /readyz -- there is deliberately no test asserting a health
// check fails, because none may (constitution II).
package events

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/tabman83/kvsynk8s/internal/azure"
)

// flakyQueueSource fails its first failures Receive calls, then succeeds
// forever with empty batches. Deliberately separate from fakeQueueSource
// (listener_test.go) to keep this file self-contained.
type flakyQueueSource struct {
	failures int
	calls    int
}

func (f *flakyQueueSource) Receive(_ context.Context, _ int32) ([]azure.QueueMessage, error) {
	f.calls++
	if f.calls <= f.failures {
		return nil, errors.New("receive failed")
	}
	return nil, nil
}

func (f *flakyQueueSource) Delete(_ context.Context, _, _ string) error { return nil }

var _ azure.QueueSource = (*flakyQueueSource)(nil)

func TestListener_ReceiveFailures_TrackedInGauges_ResetOnSuccess(t *testing.T) {
	queue := &flakyQueueSource{failures: 2}
	events := make(chan event.GenericEvent, 1)
	l := NewListener(queue, fakeClientWith(t), events)

	ctx := context.Background()
	before := time.Now()

	// First failed Receive: 1 consecutive failure.
	if _, err := l.pollOnce(ctx); err == nil {
		t.Fatal("pollOnce() error = nil, want the fake Receive error")
	}
	if got := testutil.ToFloat64(queueConsecutiveReceiveFailures); got != 1 {
		t.Errorf("consecutive failures gauge after 1st failure = %v, want 1", got)
	}

	// Second failed Receive: the count grows.
	if _, err := l.pollOnce(ctx); err == nil {
		t.Fatal("pollOnce() error = nil, want the fake Receive error")
	}
	if got := testutil.ToFloat64(queueConsecutiveReceiveFailures); got != 2 {
		t.Errorf("consecutive failures gauge after 2nd failure = %v, want 2", got)
	}

	// A successful (empty) Receive resets the failure count and stamps the
	// last-successful-receive timestamp.
	if _, err := l.pollOnce(ctx); err != nil {
		t.Fatalf("pollOnce() error = %v, want nil", err)
	}
	if got := testutil.ToFloat64(queueConsecutiveReceiveFailures); got != 0 {
		t.Errorf("consecutive failures gauge after success = %v, want 0", got)
	}
	after := time.Now()
	ts := testutil.ToFloat64(queueLastSuccessfulReceiveTimestamp)
	if ts < float64(before.UnixNano())/1e9 || ts > float64(after.UnixNano())/1e9 {
		t.Errorf("last successful receive timestamp = %v, want within [%v, %v]",
			ts, before.Unix(), after.Unix())
	}

	// The internal state mirrors the gauges.
	l.healthMu.Lock()
	defer l.healthMu.Unlock()
	if l.consecutiveReceiveFailures != 0 {
		t.Errorf("internal consecutiveReceiveFailures = %d, want 0", l.consecutiveReceiveFailures)
	}
	if l.lastSuccessfulReceive.Before(before) || l.lastSuccessfulReceive.After(after) {
		t.Errorf("internal lastSuccessfulReceive = %v, want within [%v, %v]",
			l.lastSuccessfulReceive, before, after)
	}
}

func TestListener_FailureTimestampUntouched_OnlyStalenessSignals(t *testing.T) {
	queue := &flakyQueueSource{failures: 0}
	events := make(chan event.GenericEvent, 1)
	l := NewListener(queue, fakeClientWith(t), events)
	ctx := context.Background()

	// One success stamps the gauge.
	if _, err := l.pollOnce(ctx); err != nil {
		t.Fatalf("pollOnce() error = %v, want nil", err)
	}
	stamped := testutil.ToFloat64(queueLastSuccessfulReceiveTimestamp)
	if stamped == 0 {
		t.Fatal("timestamp gauge still 0 after a successful receive")
	}

	// Subsequent failures must not move the timestamp: its growing age is
	// exactly the degradation signal an operator alerts on.
	queue.failures = queue.calls + 3
	for range 3 {
		if _, err := l.pollOnce(ctx); err == nil {
			t.Fatal("pollOnce() error = nil, want the fake Receive error")
		}
	}
	if got := testutil.ToFloat64(queueLastSuccessfulReceiveTimestamp); got != stamped {
		t.Errorf("timestamp gauge moved on failure: %v, want unchanged %v", got, stamped)
	}
	if got := testutil.ToFloat64(queueConsecutiveReceiveFailures); got != 3 {
		t.Errorf("consecutive failures gauge = %v, want 3", got)
	}
}

func TestQueueMetrics_RegisteredWithControllerRuntimeRegistry(t *testing.T) {
	// NewListener registers the gauges AND the message-outcome counter with the
	// controller-runtime Registry (the one the manager's /metrics endpoint
	// serves). A metric registered with the default prometheus registry instead
	// would never be scraped from this operator, so the name check is the whole
	// point of this test.
	_ = NewListener(&flakyQueueSource{}, fakeClientWith(t), make(chan event.GenericEvent, 1))

	// A CounterVec with no children produces no MetricFamily at all, so
	// materialise every outcome first. This doubles as the guard on the closed
	// vocabulary asserted below.
	for _, outcome := range queueOutcomes {
		recordMessageOutcome(outcome)
	}

	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	want := map[string]bool{
		"kvsynk8s_queue_last_successful_receive_timestamp_seconds": false,
		"kvsynk8s_queue_consecutive_receive_failures":              false,
		queueMessagesMetric: false,
	}
	var outcomeSeries []string
	for _, mf := range families {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
		if mf.GetName() != queueMessagesMetric {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() != "outcome" {
					t.Errorf("kvsynk8s_queue_messages_total carries an unexpected label %q; "+
						"only the closed-vocabulary \"outcome\" label may exist (constitution I, cardinality)",
						lp.GetName())
				}
				outcomeSeries = append(outcomeSeries, lp.GetValue())
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("metric %q not registered with the controller-runtime Registry", name)
		}
	}

	// The vocabulary is closed: exactly the five documented outcomes, no more.
	// A sixth series here means a new handleMessage branch invented a label
	// value without anyone deciding it belongs in the metric's contract.
	allowed := make(map[string]bool, len(queueOutcomes))
	for _, outcome := range queueOutcomes {
		allowed[outcome] = false
	}
	for _, got := range outcomeSeries {
		seen, ok := allowed[got]
		if !ok {
			t.Errorf("kvsynk8s_queue_messages_total has outcome=%q, outside the closed vocabulary %v", got, queueOutcomes)
			continue
		}
		if seen {
			t.Errorf("kvsynk8s_queue_messages_total has duplicate series for outcome=%q", got)
		}
		allowed[got] = true
	}
	for outcome, seen := range allowed {
		if !seen {
			t.Errorf("kvsynk8s_queue_messages_total has no series for outcome=%q", outcome)
		}
	}
}

// queueOutcomes is the closed label vocabulary of kvsynk8s_queue_messages_total
// (metrics.go). Kept here rather than exported from production code so the test
// pins the contract independently of whatever the implementation happens to
// pass to recordMessageOutcome.
var queueOutcomes = []string{outcomeDispatched, outcomeUnmatched, outcomeNonActionable, outcomeMalformed, outcomePoison}

// queueOutcomeCounts snapshots the current value of every outcome series. The
// counter is a package-global shared by every test in this package, so the
// outcome assertions below all work on deltas around one pollOnce rather than
// on absolute values.
func queueOutcomeCounts() map[string]float64 {
	counts := make(map[string]float64, len(queueOutcomes))
	for _, outcome := range queueOutcomes {
		counts[outcome] = testutil.ToFloat64(queueMessagesTotal.WithLabelValues(outcome))
	}
	return counts
}

// TestListener_MessageOutcome_CountedExactlyOnce drives one message of each
// shape through the listener and asserts it bumps its own outcome series by
// exactly one and leaves the other four alone. This is what makes the metric
// diagnostic rather than decorative: "unmatched" in particular is the only
// signal an operator gets for a typo in spec.vault, and it is worthless if a
// branch double-counts or gets attributed to the wrong outcome.
func TestListener_MessageOutcome_CountedExactlyOnce(t *testing.T) {
	matchingBody := string(encodedBody(eventPayload(testEventID, newVersionET, testVault, secretType, testObject, testVersion)))

	tests := []struct {
		name string
		// declared is whether a SecretSync for testVault/testObject exists in
		// the fake cache.
		declared bool
		msg      azure.QueueMessage
		outcome  string
	}{
		{
			// Above the dequeue threshold: dropped before it is even parsed,
			// even though the body would otherwise have dispatched.
			name:     "poison",
			declared: true,
			msg: azure.QueueMessage{
				ID: "outcome-poison", PopReceipt: "pop-outcome-poison",
				Text: matchingBody, DequeueCount: DefaultPoisonThreshold + 1,
			},
			outcome: "poison",
		},
		{
			name:     "malformed body",
			declared: true,
			msg: azure.QueueMessage{
				ID: "outcome-malformed", PopReceipt: "pop-outcome-malformed",
				Text: "not-valid-base64!!!", DequeueCount: 1,
			},
			outcome: "malformed",
		},
		{
			// A valid envelope carrying an event type this operator ignores.
			name:     "non-actionable event type",
			declared: true,
			msg: azure.QueueMessage{
				ID: "outcome-nonactionable", PopReceipt: "pop-outcome-nonactionable",
				Text:         string(encodedBody(eventPayload(testEventID, nearExpiryET, testVault, secretType, testObject, testVersion))),
				DequeueCount: 1,
			},
			outcome: "nonactionable",
		},
		{
			// Well-formed secret event that no SecretSync declares.
			name:     "unmatched",
			declared: false,
			msg: azure.QueueMessage{
				ID: "outcome-unmatched", PopReceipt: "pop-outcome-unmatched",
				Text:         string(encodedBody(eventPayload(testEventID, newVersionET, "no-such-vault", secretType, testObject, testVersion))),
				DequeueCount: 1,
			},
			outcome: "unmatched",
		},
		{
			name:     outcomeDispatched,
			declared: true,
			msg: azure.QueueMessage{
				ID: "outcome-dispatched", PopReceipt: "pop-outcome-dispatched",
				Text: matchingBody, DequeueCount: 1,
			},
			outcome: outcomeDispatched,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objs []client.Object
			if tt.declared {
				objs = append(objs, newSecretSync("ns-a", "sync-a", testVault, testObject))
			}
			queue := newFakeQueueSource([]azure.QueueMessage{tt.msg})
			events := make(chan event.GenericEvent, 10)
			l := NewListener(queue, fakeClientWith(t, objs...), events)

			before := queueOutcomeCounts()
			if _, err := l.pollOnce(context.Background()); err != nil {
				t.Fatalf("pollOnce() error = %v, want nil", err)
			}
			after := queueOutcomeCounts()

			for _, outcome := range queueOutcomes {
				want := float64(0)
				if outcome == tt.outcome {
					want = 1
				}
				if got := after[outcome] - before[outcome]; got != want {
					t.Errorf("kvsynk8s_queue_messages_total{outcome=%q} moved by %v, want %v", outcome, got, want)
				}
			}

			// Drain so the channel capacity never influences a later subtest.
			for {
				select {
				case <-events:
					continue
				default:
				}
				break
			}
		})
	}
}
