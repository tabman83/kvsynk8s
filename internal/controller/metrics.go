// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	kvsynk8sv1alpha1 "github.com/tabman83/kvsynk8s/api/v1alpha1"
)

// Sync-path metrics. Until these existed the operator's own job was invisible
// to Prometheus: the only metrics in the repo covered the queue-receive path
// (internal/events/metrics.go), and controller-runtime's built-in
// controller_runtime_reconcile_total actively misreports this controller,
// because every classified failure (SecretNotFound, AccessDenied,
// SourceDeleted, SourceDisabled, TargetConflict, TargetImmutable) is reported
// through status with err == nil and therefore counted as result="success".
//
// LABEL POLICY, applied to every metric here: label values come only from a
// fixed, closed, compile-time vocabulary. No per-object labels (name,
// namespace) — cardinality would grow with the fleet and, worse, every deleted
// SecretSync would leave an immortal series behind. No vault name and no vault
// secret name either. Those are identifiers rather than values, so constitution
// I does not forbid them, but a metrics endpoint has a much broader audience
// and a much longer retention than a namespaced CR, a name like
// "prod-stripe-live-key" is sensitive metadata on its own, and there is no
// diagnostic question they answer that `kubectl get secretsync -A` does not
// answer better.
var (
	syncTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kvsynk8s_sync_total",
		Help: "Terminal sync outcomes by result and reason. result is " +
			"\"success\" or \"failure\"; reason is the SecretSync status.reason on " +
			"a failure and \"None\" on success. Pending is not terminal and is " +
			"not counted. A rising failure rate for one reason is the signal; " +
			"which objects are affected is a kubectl question, not a metrics one.",
	}, []string{"result", "reason"})

	secretSyncState = prometheus.NewDesc(
		"kvsynk8s_secretsync_state",
		"Number of SecretSync objects currently in each state (Pending, InSync, Failing).",
		[]string{"state"}, nil,
	)

	oldestSuccessfulSync = prometheus.NewDesc(
		"kvsynk8s_secretsync_oldest_successful_sync_timestamp_seconds",
		"Unix timestamp of the oldest status.lastSyncTime among SecretSyncs that are "+
			"currently InSync, or 0 when none are. Restricted to InSync on purpose: a "+
			"Failing object keeps its last successful sync time frozen forever, so "+
			"including it would pin this gauge permanently and only duplicate what "+
			"kvsynk8s_secretsync_state already reports.",
		nil, nil,
	)

	// stateReader is the client the collector lists through, held in an atomic
	// rather than captured by the collector, because registration happens once
	// per process while the envtest suite builds a fresh manager (and so a
	// fresh client) per spec. A captured client would leave every spec after
	// the first collecting from a stopped manager's cache.
	stateReader atomic.Pointer[client.Reader]

	registerSyncMetricsOnce sync.Once
)

// stateCollector reports the per-state counts and the oldest successful sync
// at scrape time, by listing SecretSyncs from the manager's cache.
//
// This is deliberately a collector rather than a GaugeVec updated per
// reconcile. A GaugeVec cannot decrement another object's contribution and
// leaves a stale series behind on every deletion, so the numbers drift away
// from reality exactly when the fleet is changing. Listing at scrape time is
// always exactly right, costs no API-server call (it reads the informer
// cache), and adds no per-object series.
type stateCollector struct{}

func (stateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- secretSyncState
	ch <- oldestSuccessfulSync
}

func (stateCollector) Collect(ch chan<- prometheus.Metric) {
	readerPtr := stateReader.Load()
	if readerPtr == nil {
		// Scraped before SetupWithManager ran. Emitting nothing is right:
		// zeroes here would read as "no SecretSyncs exist", which is a
		// different and wrong statement.
		return
	}

	var list kvsynk8sv1alpha1.SecretSyncList
	if err := (*readerPtr).List(context.Background(), &list); err != nil {
		// The cache may not have synced yet. Same reasoning as above: report
		// nothing rather than something untrue.
		return
	}

	counts := map[kvsynk8sv1alpha1.SecretSyncState]int{
		kvsynk8sv1alpha1.SecretSyncStatePending: 0,
		kvsynk8sv1alpha1.SecretSyncStateInSync:  0,
		kvsynk8sv1alpha1.SecretSyncStateFailing: 0,
	}
	oldest := int64(0)
	for i := range list.Items {
		item := &list.Items[i]
		state := item.Status.State
		if state == "" {
			state = kvsynk8sv1alpha1.SecretSyncStatePending
		}
		counts[state]++

		if state != kvsynk8sv1alpha1.SecretSyncStateInSync || item.Status.LastSyncTime.IsZero() {
			continue
		}
		if synced := item.Status.LastSyncTime.Unix(); oldest == 0 || synced < oldest {
			oldest = synced
		}
	}

	for state, count := range counts {
		ch <- prometheus.MustNewConstMetric(
			secretSyncState, prometheus.GaugeValue, float64(count), string(state))
	}
	ch <- prometheus.MustNewConstMetric(oldestSuccessfulSync, prometheus.GaugeValue, float64(oldest))
}

// registerSyncMetrics registers the sync-path metrics with the
// controller-runtime Registry exactly once, and publishes reader as the client
// the state collector lists through.
//
// The sync.Once is load-bearing beyond tidiness: the envtest suite calls
// SetupWithManager once per spec, and an unguarded MustRegister panics the
// second time.
func registerSyncMetrics(reader client.Reader) {
	stateReader.Store(&reader)
	registerSyncMetricsOnce.Do(func() {
		metrics.Registry.MustRegister(syncTotal, stateCollector{})
	})
}

// noFailureReason is the reason label on a successful sync. A non-empty
// sentinel rather than "", so a PromQL query never has to reason about the
// difference between an empty label value and an absent label.
const noFailureReason = "None"

// recordSyncResult counts one terminal reconcile outcome. reason is ignored
// for a success, where the label is always noFailureReason.
func recordSyncResult(state kvsynk8sv1alpha1.SecretSyncState, reason string) {
	switch state {
	case kvsynk8sv1alpha1.SecretSyncStateInSync:
		syncTotal.WithLabelValues("success", noFailureReason).Inc()
	case kvsynk8sv1alpha1.SecretSyncStateFailing:
		if reason == "" {
			reason = "Unknown"
		}
		syncTotal.WithLabelValues("failure", reason).Inc()
	}
}
