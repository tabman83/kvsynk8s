// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Queue-receive health gauges (spec US3 acceptance scenario 3): they make a
// degraded notification path visible to an operator without ever touching
// liveness or readiness. /healthz and /readyz stay static on purpose --
// a queue outage degrades propagation speed, never correctness (the periodic
// reconcile still converges every SecretSync), so it must not restart the
// operator (constitution II) or flip readiness under `helm upgrade --wait`.
//
// The gauges are registered with the controller-runtime metrics Registry the
// first time a Listener is built, so they exist only when a queue URL is
// configured -- an operator running without the queue path sees no queue
// metrics at all, rather than a permanently-zero timestamp.
//
// Honest limitation: these gauges cover the queue-receive path only. A broken
// or deleted Event Grid subscription still yields successful, empty receives,
// so a healthy timestamp here does not prove events are being delivered
// upstream. See README "Troubleshooting".
var (
	queueLastSuccessfulReceiveTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "kvsynk8s_queue_last_successful_receive_timestamp_seconds",
		Help: "Unix timestamp of the last successful Storage Queue Receive call " +
			"(including empty receives). 0 until the first success after startup. " +
			"A stale value means the operator cannot reach the queue; periodic " +
			"reconciliation still converges secrets, only near-realtime propagation " +
			"is degraded.",
	})
	queueConsecutiveReceiveFailures = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "kvsynk8s_queue_consecutive_receive_failures",
		Help: "Number of consecutive failed Storage Queue Receive calls. Reset to 0 " +
			"by every successful receive. A persistently growing value means the " +
			"queue-notification path is down (network, auth, or queue configuration); " +
			"secrets still converge via periodic reconciliation.",
	})

	registerQueueMetricsOnce sync.Once
)

// registerQueueMetrics registers the queue health gauges with the
// controller-runtime metrics Registry exactly once. Called from NewListener
// (and defensively from Start via applyDefaults) so the metrics appear only
// when the queue listener actually exists.
func registerQueueMetrics() {
	registerQueueMetricsOnce.Do(func() {
		metrics.Registry.MustRegister(
			queueLastSuccessfulReceiveTimestamp,
			queueConsecutiveReceiveFailures,
		)
	})
}

// recordReceiveSuccess updates the listener's health state after a successful
// Receive (empty or not) and mirrors it into the gauges. healthMu guards the
// counter/timestamp pair so the state stays coherent if anything beyond the
// poll goroutine ever reads it.
func (l *Listener) recordReceiveSuccess(now time.Time) {
	l.healthMu.Lock()
	defer l.healthMu.Unlock()
	l.consecutiveReceiveFailures = 0
	l.lastSuccessfulReceive = now
	queueConsecutiveReceiveFailures.Set(0)
	queueLastSuccessfulReceiveTimestamp.Set(float64(now.UnixNano()) / float64(time.Second))
}

// recordReceiveFailure updates the listener's health state after a failed
// Receive and mirrors the new consecutive-failure count into its gauge. The
// last-successful-receive timestamp is deliberately left alone: its staleness
// is the signal.
func (l *Listener) recordReceiveFailure() {
	l.healthMu.Lock()
	defer l.healthMu.Unlock()
	l.consecutiveReceiveFailures++
	queueConsecutiveReceiveFailures.Set(float64(l.consecutiveReceiveFailures))
}
