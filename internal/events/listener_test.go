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
//  1. A matched event (vault name and secret name both matched
//     case-insensitively) against one or more SecretSync objects across
//     namespaces emits one event.GenericEvent per match, then deletes the
//     queue message.
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
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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
	deleted  []string // message IDs Delete was called for, in order
	receives int
	// deleteErr, when non-nil, is returned by every Delete call (the call is
	// still recorded in deleted first, so tests can assert the delete was
	// attempted). Exercises listener.go's deleteMessage error branch.
	deleteErr error
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
	return f.deleteErr
}

func (f *fakeQueueSource) receiveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.receives
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

// TestListener_SecretNameCasingDiffersFromSpec_StillMatches is the regression
// test for the silently-dropped-rotation bug: matchingSecretSyncs compared the
// vault name with strings.EqualFold but the secret name with ==. Key Vault
// object names are case-insensitive and case-preserving, and the CRD pattern
// for spec.vault.secret allows mixed case, so a spec whose casing differs from
// the vault's syncs fine on every direct read (initial sync, every periodic
// reconcile) and then matches no event at all: the message is deleted at V(1)
// as unmatched and the rotation is only picked up hours later by the periodic
// safety net. Here the spec and the event disagree on the casing of BOTH
// names, and the reconcile request must still be produced.
func TestListener_SecretNameCasingDiffersFromSpec_StillMatches(t *testing.T) {
	match := newSecretSync("ns-a", "sync-a", "My-Vault", "App-Password")
	cli := fakeClientWith(t, match)

	// What the vault actually emits: the casing the objects were created with.
	body := encodedBody(eventPayload(testEventID, newVersionET, "my-VAULT", secretType, "app-password", testVersion))
	msg := azure.QueueMessage{ID: "msg-casing", PopReceipt: "pop-casing", Text: string(body), DequeueCount: 1}
	queue := newFakeQueueSource([]azure.QueueMessage{msg})

	events := make(chan event.GenericEvent, 10)
	l := NewListener(queue, cli, events)

	if _, err := l.pollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce() error = %v, want nil", err)
	}

	got := drainEvents(t, events, 1)
	want := types.NamespacedName{Namespace: "ns-a", Name: "sync-a"}
	if got[0] != want {
		t.Errorf("reconcile request for %v, want %v", got[0], want)
	}

	if deleted := queue.deletedIDs(); len(deleted) != 1 || deleted[0] != "msg-casing" {
		t.Errorf("deleted = %v, want [msg-casing]", deleted)
	}
}

// TestListener_DifferentSecretName_DoesNotMatch keeps the secret-name
// comparison honest: case-insensitive must not mean "matches anything".
func TestListener_DifferentSecretName_DoesNotMatch(t *testing.T) {
	other := newSecretSync("ns-a", "sync-a", testVault, "some-other-secret")
	cli := fakeClientWith(t, other)

	body := encodedBody(eventPayload(testEventID, newVersionET, testVault, secretType, testObject, testVersion))
	msg := azure.QueueMessage{ID: "msg-other-name", PopReceipt: "pop-on", Text: string(body), DequeueCount: 1}
	queue := newFakeQueueSource([]azure.QueueMessage{msg})

	events := make(chan event.GenericEvent, 10)
	l := NewListener(queue, cli, events)

	if _, err := l.pollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce() error = %v, want nil", err)
	}

	select {
	case evt := <-events:
		t.Fatalf("a SecretSync for a different secret must not match, but got event %+v", evt)
	default:
	}

	if deleted := queue.deletedIDs(); len(deleted) != 1 || deleted[0] != "msg-other-name" {
		t.Errorf("deleted = %v, want [msg-other-name]", deleted)
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

// failFirstQueueSource wraps a fakeQueueSource and fails the first `failures`
// Receive calls with a fixed, static error before delegating. It simulates a
// transient Azure-side outage for the Start loop tests below.
type failFirstQueueSource struct {
	mu       sync.Mutex
	inner    *fakeQueueSource
	failures int
	receives int
}

func (f *failFirstQueueSource) Receive(ctx context.Context, batch int32) ([]azure.QueueMessage, error) {
	f.mu.Lock()
	f.receives++
	fail := f.failures > 0
	if fail {
		f.failures--
	}
	f.mu.Unlock()
	if fail {
		return nil, errors.New("kvsynk8s: fake transient queue outage")
	}
	return f.inner.Receive(ctx, batch)
}

func (f *failFirstQueueSource) Delete(ctx context.Context, messageID, popReceipt string) error {
	return f.inner.Delete(ctx, messageID, popReceipt)
}

func (f *failFirstQueueSource) receiveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.receives
}

var _ azure.QueueSource = (*failFirstQueueSource)(nil)

// TestListener_Start_ReceiveErrorBacksOffAndKeepsRunning pins the
// load-bearing branch of Start (constitution II): a Receive error is logged
// and followed by an idle backoff and another poll -- NOT a returned error,
// which would stop the whole manager on the first transient Azure blip. The
// first Receive fails, the second delivers a matched message; if a refactor
// ever turns that branch into `return err`, no event arrives and this test
// fails.
func TestListener_Start_ReceiveErrorBacksOffAndKeepsRunning(t *testing.T) {
	match := newSecretSync("ns-a", "sync-a", testVault, testObject)
	cli := fakeClientWith(t, match)

	body := encodedBody(eventPayload(testEventID, newVersionET, testVault, secretType, testObject, testVersion))
	msg := azure.QueueMessage{ID: "start-msg-1", PopReceipt: "pop-start-1", Text: string(body), DequeueCount: 1}
	queue := &failFirstQueueSource{inner: newFakeQueueSource([]azure.QueueMessage{msg}), failures: 1}

	events := make(chan event.GenericEvent, 1)
	l := NewListener(queue, cli, events)
	// Tiny intervals so the error backoff and the follow-up poll happen
	// within milliseconds instead of the production 30s.
	l.BusyPollInterval = time.Millisecond
	l.IdlePollInterval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- l.Start(ctx) }()

	select {
	case <-events:
		// Start survived the failed first Receive and processed the message
		// from the retried one.
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not keep polling after a Receive error: no event was ever delivered")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start() = %v, want nil on context cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return after context cancellation")
	}

	if got := queue.receiveCount(); got < 2 {
		t.Errorf("Receive was called %d times, want at least 2 (one failed, one retried)", got)
	}
	if deleted := queue.inner.deletedIDs(); len(deleted) != 1 || deleted[0] != "start-msg-1" {
		t.Errorf("deleted = %v, want [start-msg-1]", deleted)
	}
}

// TestListener_Start_AlreadyCancelledContext_ReturnsNilWithoutPolling covers
// Start's clean-shutdown contract: a context that is already done means
// return nil (a normal manager stop), not an error and not a Receive call.
func TestListener_Start_AlreadyCancelledContext_ReturnsNilWithoutPolling(t *testing.T) {
	queue := newFakeQueueSource()
	cli := fakeClientWith(t)
	l := NewListener(queue, cli, make(chan event.GenericEvent, 1))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := l.Start(ctx); err != nil {
		t.Fatalf("Start() = %v, want nil for an already-cancelled context", err)
	}
	if got := queue.receiveCount(); got != 0 {
		t.Errorf("Receive was called %d times, want 0 (the queue must not be polled after shutdown)", got)
	}
}

// TestListener_ApplyDefaults covers applyDefaults: zero-valued tuning fields
// on a hand-assembled Listener are filled with the package defaults, and
// explicitly-set fields are left alone.
func TestListener_ApplyDefaults(t *testing.T) {
	zeroed := &Listener{}
	zeroed.applyDefaults()
	if zeroed.BatchSize != DefaultBatchSize {
		t.Errorf("BatchSize = %d, want %d", zeroed.BatchSize, DefaultBatchSize)
	}
	if zeroed.PoisonThreshold != DefaultPoisonThreshold {
		t.Errorf("PoisonThreshold = %d, want %d", zeroed.PoisonThreshold, DefaultPoisonThreshold)
	}
	if zeroed.BusyPollInterval != DefaultBusyPollInterval {
		t.Errorf("BusyPollInterval = %v, want %v", zeroed.BusyPollInterval, DefaultBusyPollInterval)
	}
	if zeroed.IdlePollInterval != DefaultIdlePollInterval {
		t.Errorf("IdlePollInterval = %v, want %v", zeroed.IdlePollInterval, DefaultIdlePollInterval)
	}
	if zeroed.QueueCallTimeout != DefaultQueueCallTimeout {
		t.Errorf("QueueCallTimeout = %v, want %v", zeroed.QueueCallTimeout, DefaultQueueCallTimeout)
	}

	overridden := &Listener{
		BatchSize:        7,
		PoisonThreshold:  2,
		BusyPollInterval: 5 * time.Millisecond,
		IdlePollInterval: 7 * time.Millisecond,
		QueueCallTimeout: 9 * time.Millisecond,
	}
	overridden.applyDefaults()
	if overridden.BatchSize != 7 || overridden.PoisonThreshold != 2 ||
		overridden.BusyPollInterval != 5*time.Millisecond || overridden.IdlePollInterval != 7*time.Millisecond ||
		overridden.QueueCallTimeout != 9*time.Millisecond {
		t.Errorf("applyDefaults() overwrote explicitly-set fields: %+v", overridden)
	}
}

// TestListener_CacheListFailure_KeepsMessageForRetry pins the
// keep-message-on-cache-list-failure contract (listener.go handleMessage): if
// listing SecretSyncs from the cache fails, the message must NOT be deleted --
// it stays on the queue and reappears after its visibility timeout, so the
// event is retried instead of lost.
func TestListener_CacheListFailure_KeepsMessageForRetry(t *testing.T) {
	listErr := errors.New("kvsynk8s: fake cache list failure")
	cli := fake.NewClientBuilder().
		WithScheme(fakeScheme(t)).
		WithObjects(newSecretSync("ns-a", "sync-a", testVault, testObject)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return listErr
			},
		}).
		Build()

	body := encodedBody(eventPayload(testEventID, newVersionET, testVault, secretType, testObject, testVersion))
	msg := azure.QueueMessage{ID: "keep-msg-1", PopReceipt: "pop-keep-1", Text: string(body), DequeueCount: 1}
	queue := newFakeQueueSource([]azure.QueueMessage{msg})

	events := make(chan event.GenericEvent, 1)
	l := NewListener(queue, cli, events)

	n, err := l.pollOnce(context.Background())
	if err != nil {
		t.Fatalf("pollOnce() error = %v, want nil (a per-message failure must not fail the poll)", err)
	}
	if n != 1 {
		t.Fatalf("pollOnce() returned %d messages, want 1", n)
	}

	select {
	case evt := <-events:
		t.Fatalf("no event must be emitted when matching failed, but got %+v", evt)
	default:
	}

	if deleted := queue.deletedIDs(); len(deleted) != 0 {
		t.Errorf("deleted = %v, want none: the message must stay queued for a later retry", deleted)
	}
}

// TestListener_DeleteFailure_LoggedAndProcessingContinues covers
// deleteMessage's error branch: a failed Delete is logged and swallowed --
// the message simply reappears after its visibility timeout -- rather than
// failing the poll or losing the already-emitted events.
func TestListener_DeleteFailure_LoggedAndProcessingContinues(t *testing.T) {
	match := newSecretSync("ns-a", "sync-a", testVault, testObject)
	cli := fakeClientWith(t, match)

	body := encodedBody(eventPayload(testEventID, newVersionET, testVault, secretType, testObject, testVersion))
	msg := azure.QueueMessage{ID: "msg-delete-fails", PopReceipt: "pop-df", Text: string(body), DequeueCount: 1}
	queue := newFakeQueueSource([]azure.QueueMessage{msg})
	queue.deleteErr = errors.New("kvsynk8s: fake delete failure")

	events := make(chan event.GenericEvent, 1)
	l := NewListener(queue, cli, events)

	n, err := l.pollOnce(context.Background())
	if err != nil {
		t.Fatalf("pollOnce() error = %v, want nil (a failed delete must be swallowed)", err)
	}
	if n != 1 {
		t.Fatalf("pollOnce() returned %d messages, want 1", n)
	}

	// The event was still dispatched before the delete failed.
	drainEvents(t, events, 1)

	// And the delete was genuinely attempted (then its error swallowed).
	if deleted := queue.deletedIDs(); len(deleted) != 1 || deleted[0] != "msg-delete-fails" {
		t.Errorf("delete attempts = %v, want [msg-delete-fails]", deleted)
	}
}

// TestListener_NonActionableEvent_DeletedWithoutEmit drives a message with a
// valid envelope but an event type this operator does not act on
// (SecretNearExpiry: Parse returns nil, nil) through pollOnce, pinning
// handleMessage's clean-discard branch: deleted, nothing emitted.
func TestListener_NonActionableEvent_DeletedWithoutEmit(t *testing.T) {
	match := newSecretSync("ns-a", "sync-a", testVault, testObject)
	cli := fakeClientWith(t, match)

	body := encodedBody(eventPayload(testEventID, nearExpiryET, testVault, secretType, testObject, testVersion))
	msg := azure.QueueMessage{ID: "msg-near-expiry", PopReceipt: "pop-ne", Text: string(body), DequeueCount: 1}
	queue := newFakeQueueSource([]azure.QueueMessage{msg})

	events := make(chan event.GenericEvent, 1)
	l := NewListener(queue, cli, events)

	if _, err := l.pollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce() error = %v, want nil", err)
	}

	select {
	case evt := <-events:
		t.Fatalf("a non-actionable event must not be dispatched, but got %+v", evt)
	default:
	}

	if deleted := queue.deletedIDs(); len(deleted) != 1 || deleted[0] != "msg-near-expiry" {
		t.Errorf("deleted = %v, want [msg-near-expiry] (non-actionable events are still consumed)", deleted)
	}
}

// verbosityLogSink records the messages a logger emits together with their
// verbosity level, and — unlike recordingLogSink in redaction_test.go, which
// enables everything so it can audit keys on every path — it enables only the
// levels a real deployment would. That is what makes it able to tell "logged"
// from "logged, but invisible at the operator's verbosity".
type verbosityLogSink struct {
	maxLevel int
	msgs     *[]string
}

// newVerbosityLogger returns a logger that drops anything above maxLevel, plus
// the slice the messages that survived land in. maxLevel 0 is the operator's
// default verbosity (no -zap-log-level flag).
func newVerbosityLogger(maxLevel int) (logr.Logger, *[]string) {
	msgs := &[]string{}
	return logr.New(&verbosityLogSink{maxLevel: maxLevel, msgs: msgs}), msgs
}

func (s *verbosityLogSink) Init(logr.RuntimeInfo)        {}
func (s *verbosityLogSink) Enabled(level int) bool       { return level <= s.maxLevel }
func (s *verbosityLogSink) WithName(string) logr.LogSink { return s }

func (s *verbosityLogSink) Info(_ int, msg string, _ ...any) {
	*s.msgs = append(*s.msgs, msg)
}

func (s *verbosityLogSink) Error(_ error, msg string, _ ...any) {
	*s.msgs = append(*s.msgs, msg)
}

func (s *verbosityLogSink) WithValues(...any) logr.LogSink { return s }

var _ logr.LogSink = (*verbosityLogSink)(nil)

func containsMsg(msgs []string, want string) bool {
	for _, m := range msgs {
		if m == want {
			return true
		}
	}
	return false
}

// TestListener_UnmatchedEvent_LoggedAtDefaultVerbosity pins the split in
// verbosity between the two clean-discard branches of handleMessage. Both used
// to be V(1), which made them invisible in a normal deployment — and an
// unmatched Key Vault secret event is almost always a typo in a spec.vault or
// spec.vault.secret, which kills the realtime path for that SecretSync while
// every queue health gauge stays green. So "discarding unmatched event" must
// survive at default verbosity, while "discarding non-actionable event" must
// not: other event types on a shared queue are routine traffic and would drown
// the useful line.
//
// Only messages are asserted here, no log keys: redaction_test.go pins a closed
// allow-list for this package's keys and nothing new may be introduced.
func TestListener_UnmatchedEvent_LoggedAtDefaultVerbosity(t *testing.T) {
	// A SecretSync exists, but for another vault, so the secret event below
	// matches nothing.
	cli := fakeClientWith(t, newSecretSync("ns-a", "sync-a", "other-vault", testObject))

	unmatched := azure.QueueMessage{
		ID: "msg-verbosity-unmatched", PopReceipt: "pop-vu",
		Text:         string(encodedBody(eventPayload(testEventID, newVersionET, "no-such-vault", secretType, testObject, testVersion))),
		DequeueCount: 1,
	}
	nonActionable := azure.QueueMessage{
		ID: "msg-verbosity-nonactionable", PopReceipt: "pop-vn",
		Text:         string(encodedBody(eventPayload(testEventID, nearExpiryET, testVault, secretType, testObject, testVersion))),
		DequeueCount: 1,
	}

	queue := newFakeQueueSource([]azure.QueueMessage{unmatched, nonActionable})
	l := NewListener(queue, cli, make(chan event.GenericEvent, 10))

	logger, msgs := newVerbosityLogger(0) // default verbosity: V(1) is dropped
	if _, err := l.pollOnce(logf.IntoContext(context.Background(), logger)); err != nil {
		t.Fatalf("pollOnce() error = %v, want nil", err)
	}

	if !containsMsg(*msgs, "discarding unmatched event") {
		t.Errorf("no \"discarding unmatched event\" line at default verbosity; logged: %v", *msgs)
	}
	if containsMsg(*msgs, "discarding non-actionable event") {
		t.Errorf("\"discarding non-actionable event\" logged at default verbosity, want V(1) only; logged: %v", *msgs)
	}

	// And at V(1) both are visible, so the non-actionable discard is still
	// debuggable rather than silently gone.
	verbose, verboseMsgs := newVerbosityLogger(1)
	queue = newFakeQueueSource([]azure.QueueMessage{unmatched, nonActionable})
	l = NewListener(queue, cli, make(chan event.GenericEvent, 10))
	if _, err := l.pollOnce(logf.IntoContext(context.Background(), verbose)); err != nil {
		t.Fatalf("pollOnce() error = %v, want nil", err)
	}
	if !containsMsg(*verboseMsgs, "discarding non-actionable event") {
		t.Errorf("no \"discarding non-actionable event\" line at V(1); logged: %v", *verboseMsgs)
	}
}

// hangingQueueSource models the half-open connection the queue call timeout
// exists for: Receive and Delete never answer on their own, they only return
// when the context they were given ends. Nothing below the listener imposes a
// deadline (azqueue is built with default client options: no HTTP client
// timeout, TryTimeout zero), so without QueueCallTimeout these calls would
// block forever.
type hangingQueueSource struct{}

func (hangingQueueSource) Receive(ctx context.Context, _ int32) ([]azure.QueueMessage, error) {
	<-ctx.Done()
	return nil, fmt.Errorf("dequeue messages: %w", ctx.Err())
}

func (hangingQueueSource) Delete(ctx context.Context, messageID string, _ string) error {
	<-ctx.Done()
	return fmt.Errorf("delete message %s: %w", messageID, ctx.Err())
}

var _ azure.QueueSource = hangingQueueSource{}

// TestListener_HungReceive_BoundedByQueueCallTimeout pins the contract that
// makes a stalled queue survivable: Start's loop is strictly sequential, so a
// Receive that never returns stops every later poll AND freezes
// consecutiveReceiveFailures at zero -- the operator's documented signal for
// a broken queue path would report healthy for the whole outage. The call must
// instead be cut off at QueueCallTimeout, counted as a failure, and the loop
// left free to poll again.
func TestListener_HungReceive_BoundedByQueueCallTimeout(t *testing.T) {
	l := NewListener(hangingQueueSource{}, fakeClientWith(t), make(chan event.GenericEvent, 1))
	l.QueueCallTimeout = 50 * time.Millisecond

	var (
		err  error
		done = make(chan struct{})
	)
	go func() {
		defer close(done)
		_, err = l.pollOnce(context.Background())
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("pollOnce() never returned: a hung Receive must be bounded by QueueCallTimeout")
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pollOnce() error = %v, want a wrapped context.DeadlineExceeded", err)
	}

	l.healthMu.Lock()
	failures := l.consecutiveReceiveFailures
	l.healthMu.Unlock()
	if failures != 1 {
		t.Errorf("consecutiveReceiveFailures = %d, want 1 (a timed-out Receive is a failed Receive)", failures)
	}
}

// TestListener_HungDelete_BoundedByQueueCallTimeout is the same contract for
// the other queue call. A delete that hangs would stall the message loop just
// as thoroughly as a hung Receive; bounded, it degrades to the behavior a
// failed delete already has -- logged, and the message reappears after its
// visibility timeout, which is safe because every handleMessage path is
// idempotent.
func TestListener_HungDelete_BoundedByQueueCallTimeout(t *testing.T) {
	l := NewListener(hangingQueueSource{}, fakeClientWith(t), make(chan event.GenericEvent, 1))
	l.QueueCallTimeout = 50 * time.Millisecond

	done := make(chan struct{})
	go func() {
		defer close(done)
		l.deleteMessage(context.Background(), azure.QueueMessage{ID: "msg-hung-delete", PopReceipt: "pop-hung"})
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("deleteMessage() never returned: a hung Delete must be bounded by QueueCallTimeout")
	}
}
