// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

// This file is written first per constitution IV / tasks.md T016 (TDD): it
// exercises the Listener contract before listener.go exists, so `go build
// ./...` and `go test ./...` are expected to fail with "undefined: Listener"
// (and friends) until T019 lands.
//
// ASSUMED LISTENER API (contract for the implementer of listener.go):
//
//	type Listener struct {
//	    Queue           azure.QueueSource // internal/azure/queue.go, T017
//	    Client          client.Client     // the manager's CACHED client (informer-backed)
//	    Events          chan<- event.GenericEvent
//	    BatchSize       int32             // default 32
//	    PoisonThreshold int64             // default 5
//	    BusyPollInterval, IdlePollInterval time.Duration
//	}
//
//	func NewListener(q azure.QueueSource, cli client.Client, events chan event.GenericEvent) *Listener
//	func (l *Listener) Start(ctx context.Context) error // manager.Runnable
//
// pollOnce is an unexported single Receive-and-process cycle used directly by
// these white-box tests for determinism (avoiding real sleeps for the
// adaptive-poll behavior, which Start alone would need real or faked time
// for). It returns the number of messages the fake Receive call returned.
//
//	func (l *Listener) pollOnce(ctx context.Context) (int, error)
//
// Behavior under test (contracts/queue-message.md rules 3, 5, 6; data-model.md
// vault matching; tasks.md T016 / SC-005):
//
//  1. A matched event (case-insensitive vault name, exact secret name) against
//     one or more SecretSync objects across namespaces emits one
//     event.GenericEvent per match, then deletes the queue message.
//  2. An event matching no SecretSync is deleted with no event emitted
//     (FR-006).
//  3. DequeueCount > 5 (PoisonThreshold) => the message is deleted without
//     ever being parsed or matched; no event is emitted.
//  4. A batch of exactly 32 matched messages is fully processed: 32 events
//     emitted, 32 deletes issued, none lost.
//  5. A burst of 100+ events spread across multiple fake Receive batches
//     (SC-005) all end up as emitted events; none are lost regardless of
//     batch boundaries.
package events

import (
	"context"
	"fmt"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	kvsynk8sv1alpha1 "github.com/tabman83/kvsynk8s/api/v1alpha1"
	"github.com/tabman83/kvsynk8s/internal/azure"
)

// fakeQueueSource is a hand-written stand-in for azure.QueueSource
// (internal/azure/queue.go) -- no mocking framework, matching the style of
// fakeSecretReader in internal/sync/engine_test.go. batches is consumed one
// slice at a time, one per Receive call, so tests can simulate a burst
// spread across several dequeue round-trips.
type fakeQueueSource struct {
	mu       sync.Mutex
	batches  [][]azure.QueueMessage
	deleted  []string // message IDs deleted, in order
	receives int
}

func newFakeQueueSource(batches ...[]azure.QueueMessage) *fakeQueueSource {
	return &fakeQueueSource{batches: batches}
}

func (f *fakeQueueSource) Receive(_ context.Context, _ int32) ([]azure.QueueMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.receives++
	if len(f.batches) == 0 {
		return nil, nil
	}
	next := f.batches[0]
	f.batches = f.batches[1:]
	return next, nil
}

func (f *fakeQueueSource) Delete(_ context.Context, messageID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, messageID)
	return nil
}

func (f *fakeQueueSource) deletedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.deleted))
	copy(out, f.deleted)
	return out
}

var _ azure.QueueSource = (*fakeQueueSource)(nil)

// fakeScheme builds a runtime.Scheme with the SecretSync types registered, so
// the controller-runtime fake client can stand in for the manager's cached
// client without a real API server (envtest is reserved for the controller
// package's own tests).
func fakeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := kvsynk8sv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add SecretSync types to scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core/v1 types to scheme: %v", err)
	}
	return scheme
}

func newSecretSync(namespace, name, vaultName, vaultSecret string) *kvsynk8sv1alpha1.SecretSync {
	return &kvsynk8sv1alpha1.SecretSync{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: kvsynk8sv1alpha1.SecretSyncSpec{
			Vault: kvsynk8sv1alpha1.VaultSpec{Name: vaultName, Secret: vaultSecret},
		},
	}
}

func fakeClientWith(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(fakeScheme(t)).WithObjects(objs...).Build()
}

// drain reads exactly want events off ch, failing the test if that many
// don't arrive. It returns the NamespacedNames of the objects carried.
func drainEvents(t *testing.T, ch chan event.GenericEvent, want int) []types.NamespacedName {
	t.Helper()
	got := make([]types.NamespacedName, 0, want)
	for i := range want {
		select {
		case evt := <-ch:
			got = append(got, types.NamespacedName{Namespace: evt.Object.GetNamespace(), Name: evt.Object.GetName()})
		default:
			t.Fatalf("expected %d events, only received %d", want, i)
		}
	}
	select {
	case extra := <-ch:
		t.Fatalf("received an unexpected extra event: %+v", extra)
	default:
	}
	return got
}

func TestListener_MatchedEvent_EmitsReconcileRequests_CaseInsensitiveVaultMatch(t *testing.T) {
	matchA := newSecretSync("ns-a", "sync-a", "My-Vault", "app-password")
	matchB := newSecretSync("ns-b", "sync-b", "my-vault", "app-password") // different namespace, same target
	other := newSecretSync("ns-c", "sync-c", "other-vault", "app-password")
	cli := fakeClientWith(t, matchA, matchB, other)

	body := encodedBody(eventPayload(testEventID, newVersionET, "my-vault", secretType, "app-password", testVersion))
	msg := azure.QueueMessage{ID: "msg-1", PopReceipt: "pop-1", Text: string(body), DequeueCount: 1}
	queue := newFakeQueueSource([]azure.QueueMessage{msg})

	events := make(chan event.GenericEvent, 10)
	l := NewListener(queue, cli, events)

	n, err := l.pollOnce(context.Background())
	if err != nil {
		t.Fatalf("pollOnce() error = %v, want nil", err)
	}
	if n != 1 {
		t.Fatalf("pollOnce() returned %d messages, want 1", n)
	}

	got := drainEvents(t, events, 2)
	want := map[types.NamespacedName]bool{
		{Namespace: "ns-a", Name: "sync-a"}: true,
		{Namespace: "ns-b", Name: "sync-b"}: true,
	}
	for _, nn := range got {
		if !want[nn] {
			t.Errorf("unexpected reconcile request for %v", nn)
		}
		delete(want, nn)
	}
	if len(want) != 0 {
		t.Errorf("missing reconcile requests for %v", want)
	}

	if deleted := queue.deletedIDs(); len(deleted) != 1 || deleted[0] != "msg-1" {
		t.Errorf("deleted = %v, want [msg-1]", deleted)
	}
}

func TestListener_UnmatchedEvent_DeletedSilently(t *testing.T) {
	other := newSecretSync("ns-c", "sync-c", "other-vault", "app-password")
	cli := fakeClientWith(t, other)

	body := encodedBody(eventPayload(testEventID, newVersionET, "no-such-vault", secretType, "app-password", testVersion))
	msg := azure.QueueMessage{ID: "msg-2", PopReceipt: "pop-2", Text: string(body), DequeueCount: 1}
	queue := newFakeQueueSource([]azure.QueueMessage{msg})

	events := make(chan event.GenericEvent, 10)
	l := NewListener(queue, cli, events)

	if _, err := l.pollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce() error = %v, want nil", err)
	}

	select {
	case evt := <-events:
		t.Fatalf("no SecretSync should match, but got event %+v", evt)
	default:
	}

	if deleted := queue.deletedIDs(); len(deleted) != 1 || deleted[0] != "msg-2" {
		t.Errorf("deleted = %v, want [msg-2] (unmatched messages are still deleted, FR-006)", deleted)
	}
}

func TestListener_PoisonMessage_DeletedWithoutProcessing(t *testing.T) {
	matchA := newSecretSync("ns-a", "sync-a", testVault, testObject)
	cli := fakeClientWith(t, matchA)

	// A well-formed, matching event -- but DequeueCount already exceeds the
	// poison threshold, so it must never be parsed/matched/emitted at all
	// (contracts/queue-message.md rule 6).
	body := encodedBody(eventPayload(testEventID, newVersionET, testVault, secretType, testObject, testVersion))
	msg := azure.QueueMessage{ID: "msg-poison", PopReceipt: "pop-poison", Text: string(body), DequeueCount: 6}
	queue := newFakeQueueSource([]azure.QueueMessage{msg})

	events := make(chan event.GenericEvent, 10)
	l := NewListener(queue, cli, events)

	if _, err := l.pollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce() error = %v, want nil", err)
	}

	select {
	case evt := <-events:
		t.Fatalf("poison message must not be processed, but got event %+v", evt)
	default:
	}

	if deleted := queue.deletedIDs(); len(deleted) != 1 || deleted[0] != "msg-poison" {
		t.Errorf("deleted = %v, want [msg-poison]", deleted)
	}
}

func TestListener_BatchOf32_ProcessedWithoutLoss(t *testing.T) {
	matchA := newSecretSync("ns-a", "sync-a", testVault, testObject)
	cli := fakeClientWith(t, matchA)

	const batchSize = 32
	batch := make([]azure.QueueMessage, 0, batchSize)
	for i := range batchSize {
		id := fmt.Sprintf("msg-%02d", i)
		version := fmt.Sprintf("v%d", i)
		body := encodedBody(eventPayload(testEventID, newVersionET, testVault, secretType, testObject, version))
		batch = append(batch, azure.QueueMessage{ID: id, PopReceipt: "pop-" + id, Text: string(body), DequeueCount: 1})
	}
	queue := newFakeQueueSource(batch)

	events := make(chan event.GenericEvent, batchSize)
	l := NewListener(queue, cli, events)

	n, err := l.pollOnce(context.Background())
	if err != nil {
		t.Fatalf("pollOnce() error = %v, want nil", err)
	}
	if n != batchSize {
		t.Fatalf("pollOnce() processed %d messages, want %d", n, batchSize)
	}

	drainEvents(t, events, batchSize)

	if deleted := queue.deletedIDs(); len(deleted) != batchSize {
		t.Errorf("deleted %d messages, want %d (none lost)", len(deleted), batchSize)
	}
}

func TestListener_BurstAcrossMultipleBatches_NoneLost(t *testing.T) {
	matchA := newSecretSync("ns-a", "sync-a", testVault, testObject)
	cli := fakeClientWith(t, matchA)

	// SC-005: a burst of 100+ events, spread across several 32-message
	// dequeue batches (the last one partial), must all end up emitted.
	const total = 106
	const batchSize = 32
	var batches [][]azure.QueueMessage
	for start := 0; start < total; start += batchSize {
		end := min(start+batchSize, total)
		var batch []azure.QueueMessage
		for i := start; i < end; i++ {
			id := fmt.Sprintf("burst-%03d", i)
			version := fmt.Sprintf("v%d", i)
			body := encodedBody(eventPayload(testEventID, newVersionET, testVault, secretType, testObject, version))
			batch = append(batch, azure.QueueMessage{ID: id, PopReceipt: "pop-" + id, Text: string(body), DequeueCount: 1})
		}
		batches = append(batches, batch)
	}
	queue := newFakeQueueSource(batches...)

	events := make(chan event.GenericEvent, total)
	l := NewListener(queue, cli, events)

	ctx := context.Background()
	processed := 0
	// Drive pollOnce until the fake queue reports empty, mirroring how
	// Start's adaptive loop would drain a burst across several Receive
	// round-trips before backing off to the idle interval.
	for i := 0; i < len(batches)+1; i++ {
		n, err := l.pollOnce(ctx)
		if err != nil {
			t.Fatalf("pollOnce() error = %v, want nil", err)
		}
		processed += n
		if n == 0 {
			break
		}
	}

	if processed != total {
		t.Fatalf("processed %d messages across batches, want %d", processed, total)
	}

	drainEvents(t, events, total)

	if deleted := queue.deletedIDs(); len(deleted) != total {
		t.Errorf("deleted %d messages, want %d (SC-005: none lost across a burst)", len(deleted), total)
	}
}
