/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Tests for the sync-path metrics in metrics.go. Plain Go tests, no envtest:
// the state collector only Lists SecretSyncs, so a fake client seeded with a
// fleet is a complete stand-in for the manager's cache, and it lets a spec
// state a fleet that would take a dozen reconciles to build for real.
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	kvsynk8sv1alpha1 "github.com/tabman83/kvsynk8s/api/v1alpha1"
)

// listFailingReader answers every List with an error, modelling an informer
// cache that has not synced yet. Get is never called by the collector; it is
// implemented only to satisfy client.Reader.
type listFailingReader struct{}

func (listFailingReader) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return errors.New("kvsynk8s: fake reader: Get is not expected here")
}

func (listFailingReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return errors.New("kvsynk8s: fake cache not synced")
}

var _ client.Reader = listFailingReader{}

// metricsScheme is the minimal scheme the fake client needs to List
// SecretSyncs. Built here rather than reusing the suite's global scheme.Scheme
// because these tests must pass on their own, without BeforeSuite having run.
func metricsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := kvsynk8sv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add SecretSync types to scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add core/v1 types to scheme: %v", err)
	}
	return s
}

// stateSync builds a SecretSync already in a given state, as the collector
// sees it: only status matters here. syncedAt is ignored when zero.
func stateSync(name string, state kvsynk8sv1alpha1.SecretSyncState, syncedAt time.Time) *kvsynk8sv1alpha1.SecretSync {
	ss := &kvsynk8sv1alpha1.SecretSync{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: kvsynk8sv1alpha1.SecretSyncSpec{
			Vault: kvsynk8sv1alpha1.VaultSpec{Name: fakeVaultName, Secret: fakeSecretName},
		},
		Status: kvsynk8sv1alpha1.SecretSyncStatus{State: state},
	}
	if !syncedAt.IsZero() {
		ss.Status.LastSyncTime = metav1.NewTime(syncedAt)
	}
	return ss
}

// useStateReader points the collector at reader for one test and restores
// whatever the suite had published before, so a plain Go test cannot leave the
// envtest specs (which publish a manager's client through the same global)
// collecting from a dead reader.
func useStateReader(t *testing.T, reader client.Reader) {
	t.Helper()
	previous := stateReader.Load()
	t.Cleanup(func() { stateReader.Store(previous) })
	if reader == nil {
		stateReader.Store(nil)
		return
	}
	registerSyncMetrics(reader)
}

// collectState scrapes the state collector through a registry of its own and
// returns the per-state gauges plus the oldest-successful-sync gauge.
//
// A private registry rather than metrics.Registry: the collector's output is
// what is under test, and reading it in isolation keeps these assertions
// independent of whatever else the package has registered by the time they
// run. found reports whether the oldest-sync gauge was emitted at all, which
// is the whole assertion for the "reader missing or broken" cases.
func collectState(t *testing.T) (states map[string]float64, oldest float64, found bool) {
	t.Helper()

	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(stateCollector{}); err != nil {
		t.Fatalf("register state collector: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}

	states = map[string]float64{}
	for _, mf := range families {
		switch mf.GetName() {
		case "kvsynk8s_secretsync_state":
			for _, m := range mf.GetMetric() {
				for _, label := range m.GetLabel() {
					if label.GetName() == "state" {
						states[label.GetValue()] = m.GetGauge().GetValue()
					}
				}
			}
		case "kvsynk8s_secretsync_oldest_successful_sync_timestamp_seconds":
			oldest, found = mf.GetMetric()[0].GetGauge().GetValue(), true
		}
	}
	return states, oldest, found
}

// The envtest suite calls SetupWithManager once per spec, and SetupWithManager
// calls registerSyncMetrics — so a second MustRegister on the same collectors
// would panic and take the whole suite with it. That is what the sync.Once in
// registerSyncMetrics prevents, and it is load-bearing rather than tidiness,
// so it gets a test of its own.
func TestRegisterSyncMetrics_RegistersEveryFamily_AndIsIdempotent(t *testing.T) {
	// Seeded with one InSync object, because the oldest-successful-sync gauge
	// is deliberately not emitted when nothing is InSync (0 is not a
	// timestamp). Registration and emission are different things, and this
	// test is about registration, so the fleet has to give the collector
	// something true to say.
	reader := fake.NewClientBuilder().WithScheme(metricsScheme(t)).WithObjects(
		stateSync("registered-in-sync", kvsynk8sv1alpha1.SecretSyncStateInSync, time.Now()),
	).Build()
	useStateReader(t, reader)

	// Twice, exactly as two managers in one process would.
	registerSyncMetrics(reader)
	registerSyncMetrics(reader)

	// A CounterVec that has never been touched gathers as no family at all, so
	// materialise the child series without counting anything: the assertion
	// below is still about registration -- an unregistered CounterVec would
	// happily hand out this child and still be absent from the Registry.
	syncTotal.WithLabelValues("success", noFailureReason)

	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	want := map[string]bool{
		"kvsynk8s_sync_total":       false,
		"kvsynk8s_secretsync_state": false,
		"kvsynk8s_secretsync_oldest_successful_sync_timestamp_seconds": false,
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

func TestStateCollector_BucketsFleetByState(t *testing.T) {
	now := time.Now()
	reader := fake.NewClientBuilder().WithScheme(metricsScheme(t)).WithObjects(
		stateSync("in-sync-a", kvsynk8sv1alpha1.SecretSyncStateInSync, now),
		stateSync("in-sync-b", kvsynk8sv1alpha1.SecretSyncStateInSync, now),
		stateSync("failing", kvsynk8sv1alpha1.SecretSyncStateFailing, time.Time{}),
		stateSync("pending", kvsynk8sv1alpha1.SecretSyncStatePending, time.Time{}),
		// A CR whose very first reconcile has not run yet has no state at all.
		// It counts as Pending: leaving it out of every bucket would make the
		// three series stop summing to the size of the fleet, which is the one
		// property an operator reads this metric for.
		stateSync("unreconciled", "", time.Time{}),
	).Build()
	useStateReader(t, reader)

	states, _, _ := collectState(t)
	want := map[kvsynk8sv1alpha1.SecretSyncState]float64{
		kvsynk8sv1alpha1.SecretSyncStateInSync:  2,
		kvsynk8sv1alpha1.SecretSyncStateFailing: 1,
		kvsynk8sv1alpha1.SecretSyncStatePending: 2,
	}
	for state, count := range want {
		if states[string(state)] != count {
			t.Errorf("kvsynk8s_secretsync_state{state=%q} = %v, want %v", state, states[string(state)], count)
		}
	}
}

func TestStateCollector_EmptyFleet_EmitsEveryStateAtZero(t *testing.T) {
	useStateReader(t, fake.NewClientBuilder().WithScheme(metricsScheme(t)).Build())

	states, oldest, found := collectState(t)
	// All three series must be present at zero rather than absent: an absent
	// series makes a Prometheus alert on it evaluate to "no data" instead of
	// "no failures", which is a different and much worse answer.
	for _, state := range []kvsynk8sv1alpha1.SecretSyncState{
		kvsynk8sv1alpha1.SecretSyncStatePending,
		kvsynk8sv1alpha1.SecretSyncStateInSync,
		kvsynk8sv1alpha1.SecretSyncStateFailing,
	} {
		value, ok := states[string(state)]
		if !ok {
			t.Errorf("kvsynk8s_secretsync_state{state=%q} missing for an empty fleet, want it present at 0", state)
			continue
		}
		if value != 0 {
			t.Errorf("kvsynk8s_secretsync_state{state=%q} = %v, want 0", state, value)
		}
	}
	// The timestamp gauge is the exception to the present-at-zero rule above.
	// A count of 0 is a true count; 0 is not a time. Emitting it would make
	// the obvious staleness alert, `time() - <gauge>`, report a sync in 1970
	// and fire on a cluster that simply has no SecretSyncs yet.
	if found {
		t.Errorf("oldest-successful-sync gauge present (%v) with nothing InSync, want the series absent", oldest)
	}
}

// TestStateCollector_BlockingCache_DoesNotHangTheScrape pins the deadline on
// the collector's List.
//
// The manager starts its metrics server before its caches, so a scrape can
// land while the SecretSync informer has started but not synced. In that
// window the cached client does not fail -- it BLOCKS in WaitForCacheSync
// until its context is done, and an informer that can never sync (RBAC drift,
// or the CRD removed) means never. Without the deadline this Collect would
// hang, and with it the entire /metrics response, including controller-runtime's
// own counters. The other failure test uses a reader that returns immediately,
// which cannot catch that at all.
func TestStateCollector_BlockingCache_DoesNotHangTheScrape(t *testing.T) {
	useStateReader(t, blockingReader{})

	done := make(chan struct{})
	var found bool
	go func() {
		defer close(done)
		_, _, found = collectState(t)
	}()

	select {
	case <-done:
	case <-time.After(collectTimeout + 5*time.Second):
		t.Fatal("collector did not return: the scrape-path List has no deadline")
	}
	if found {
		t.Error("collector reported a timestamp from a cache it never read")
	}
}

// blockingReader models a started-but-unsynced informer: List does not fail,
// it waits for the caller's context, exactly as the cached client does.
type blockingReader struct{ client.Reader }

func (blockingReader) List(ctx context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestStateCollector_NoReaderYet_EmitsNothing(t *testing.T) {
	// A scrape can land before SetupWithManager has published a reader.
	// Emitting zeroes then would claim the fleet is empty, which is a
	// different statement from "not known yet" -- and must not panic on the
	// nil pointer either.
	useStateReader(t, nil)

	states, _, found := collectState(t)
	if len(states) != 0 {
		t.Errorf("state series emitted with no reader published: %v, want none", states)
	}
	if found {
		t.Error("oldest-successful-sync gauge emitted with no reader published, want none")
	}
}

func TestStateCollector_ListError_EmitsNothing(t *testing.T) {
	// Same reasoning as the no-reader case: a cache that cannot answer must
	// report nothing rather than something untrue.
	useStateReader(t, listFailingReader{})

	states, _, found := collectState(t)
	if len(states) != 0 {
		t.Errorf("state series emitted after a failed List: %v, want none", states)
	}
	if found {
		t.Error("oldest-successful-sync gauge emitted after a failed List, want none")
	}
}

func TestStateCollector_OldestSuccessfulSync_CoversOnlyInSyncObjects(t *testing.T) {
	now := time.Now()
	recent := now.Add(-10 * time.Minute)
	oldestInSync := now.Add(-3 * time.Hour)
	// A Failing object keeps the lastSyncTime of its last good sync forever,
	// so counting it would pin this gauge at that frozen value for as long as
	// the failure lasts -- exactly when the gauge is supposed to be reporting
	// on the objects that are still working.
	frozen := now.Add(-30 * 24 * time.Hour)

	reader := fake.NewClientBuilder().WithScheme(metricsScheme(t)).WithObjects(
		stateSync("recent", kvsynk8sv1alpha1.SecretSyncStateInSync, recent),
		stateSync("stale", kvsynk8sv1alpha1.SecretSyncStateInSync, oldestInSync),
		stateSync("failing-long-ago", kvsynk8sv1alpha1.SecretSyncStateFailing, frozen),
	).Build()
	useStateReader(t, reader)

	_, oldest, found := collectState(t)
	if !found {
		t.Fatal("oldest-successful-sync gauge not emitted")
	}
	// metav1.Time serialises at second resolution, so compare on Unix seconds.
	if want := float64(metav1.NewTime(oldestInSync).Unix()); oldest != want {
		t.Errorf("oldest successful sync gauge = %v, want %v (the oldest InSync object, not the Failing one at %v)",
			oldest, want, float64(metav1.NewTime(frozen).Unix()))
	}
}
