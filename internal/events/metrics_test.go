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
	// NewListener registers the gauges with the controller-runtime Registry
	// (the one the manager's /metrics endpoint serves).
	_ = NewListener(&flakyQueueSource{}, fakeClientWith(t), make(chan event.GenericEvent, 1))

	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	want := map[string]bool{
		"kvsynk8s_queue_last_successful_receive_timestamp_seconds": false,
		"kvsynk8s_queue_consecutive_receive_failures":              false,
	}
	for _, mf := range families {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("metric %q not registered with the controller-runtime Registry", name)
		}
	}
}
