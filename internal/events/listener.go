// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"context"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	kvsynk8sv1alpha1 "github.com/tabman83/kvsynk8s/api/v1alpha1"
	"github.com/tabman83/kvsynk8s/internal/azure"
)

const (
	// DefaultBatchSize is the queue receive batch size (research R6; scale
	// assumptions in data-model.md: bursts up to 100 events/min, queue batch
	// size 32).
	DefaultBatchSize = int32(32)

	// DefaultPoisonThreshold is the DequeueCount above which a message is
	// treated as poison and discarded without further processing
	// (contracts/queue-message.md rule 6).
	DefaultPoisonThreshold = int64(5)

	// DefaultBusyPollInterval is how soon Start polls again after a
	// non-empty Receive: short-poll while messages are flowing (research
	// R6, "near realtime" propagation target).
	DefaultBusyPollInterval = 2 * time.Second

	// DefaultIdlePollInterval is how long Start waits after an empty
	// Receive before polling again: comfortably within the <60s propagation
	// budget (research R6) while keeping the queue quiet when nothing is
	// happening.
	DefaultIdlePollInterval = 30 * time.Second
)

// Listener pulls Key Vault change notifications off the queue and turns
// matching ones into reconcile requests for the SecretSync controller,
// injected via a source.Channel (T020, cmd/main.go /
// internal/controller/secretsync_controller.go). It implements
// manager.Runnable (Start) so cmd/main.go can register it with
// mgr.Add(listener) directly, alongside the reconciler itself.
//
// Listener never fetches or writes a secret value itself (constitution I):
// it only maps a notification to the SecretSync objects it might concern and
// lets the existing reconcile/engine/writer path (T012, T011) do the actual
// value read and write, exactly as a periodic reconcile would.
type Listener struct {
	// Queue is the notification source (internal/azure/queue.go). Required.
	Queue azure.QueueSource

	// Client is the manager's CACHED client (informer-backed), used to find
	// SecretSync objects across namespaces without a live list-from-apiserver
	// call on every message.
	Client client.Client

	// Events is where matched SecretSync objects are sent as
	// event.GenericEvent, for a source.Channel to turn into reconcile
	// requests. Required.
	Events chan<- event.GenericEvent

	// BatchSize is how many messages to request per Receive call. Defaults
	// to DefaultBatchSize via NewListener.
	BatchSize int32
	// PoisonThreshold is the DequeueCount above which a message is deleted
	// without being parsed or matched. Defaults to DefaultPoisonThreshold.
	PoisonThreshold int64
	// BusyPollInterval/IdlePollInterval control Start's adaptive poll delay.
	// Default to DefaultBusyPollInterval/DefaultIdlePollInterval.
	BusyPollInterval time.Duration
	IdlePollInterval time.Duration

	// healthMu guards the queue-receive health state below, which pollOnce
	// maintains and mirrors into the Prometheus gauges in metrics.go (spec
	// US3 acceptance scenario 3: a degraded notification path must be
	// visible to an operator). Health state only -- it never gates
	// /healthz or /readyz, and it never changes poll behavior.
	healthMu sync.Mutex
	// consecutiveReceiveFailures counts failed Receive calls since the last
	// successful one (0 while healthy).
	consecutiveReceiveFailures int64
	// lastSuccessfulReceive is when the last successful Receive returned
	// (zero until the first success after startup).
	lastSuccessfulReceive time.Time
}

var (
	_ manager.Runnable               = (*Listener)(nil)
	_ manager.LeaderElectionRunnable = (*Listener)(nil)
)

// NewListener builds a Listener with the standard defaults for batch size,
// poison threshold, and poll cadence. Callers may override any field on the
// returned pointer before Start runs (tests use this to shrink the poll
// intervals).
func NewListener(queue azure.QueueSource, cli client.Client, events chan event.GenericEvent) *Listener {
	registerQueueMetrics()
	return &Listener{
		Queue:            queue,
		Client:           cli,
		Events:           events,
		BatchSize:        DefaultBatchSize,
		PoisonThreshold:  DefaultPoisonThreshold,
		BusyPollInterval: DefaultBusyPollInterval,
		IdlePollInterval: DefaultIdlePollInterval,
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: the listener
// always runs regardless of leader election. This operator never actually
// enables leader election (single replica, plan.md), so this only documents
// the intent defensively.
func (l *Listener) NeedLeaderElection() bool { return false }

// Start implements manager.Runnable: it polls the queue with an adaptive
// delay (research R6) until ctx is cancelled. Receive errors are logged and
// treated like an idle poll (backoff, keep running) rather than stopping the
// manager -- a transient queue outage must not crash the operator or block
// the periodic-reconciliation safety net (constitution II). Each poll also
// updates the queue health gauges (metrics.go), which is the only way queue
// degradation is surfaced: never via /healthz or /readyz.
func (l *Listener) Start(ctx context.Context) error {
	l.applyDefaults()
	log := logf.FromContext(ctx).WithName("events-listener")

	for {
		if ctx.Err() != nil {
			return nil
		}

		n, err := l.pollOnce(ctx)
		if err != nil {
			log.Error(err, "failed to receive queue messages")
			if !sleepOrDone(ctx, l.IdlePollInterval) {
				return nil
			}
			continue
		}

		wait := l.IdlePollInterval
		if n > 0 {
			wait = l.BusyPollInterval
		}
		if !sleepOrDone(ctx, wait) {
			return nil
		}
	}
}

// applyDefaults fills in zero-valued tuning fields so a Listener built with
// a plain struct literal (or with only some fields overridden) still behaves
// sensibly; NewListener already sets all of these, but Start defends against
// a Listener assembled by hand.
func (l *Listener) applyDefaults() {
	registerQueueMetrics()
	if l.BatchSize == 0 {
		l.BatchSize = DefaultBatchSize
	}
	if l.PoisonThreshold == 0 {
		l.PoisonThreshold = DefaultPoisonThreshold
	}
	if l.BusyPollInterval == 0 {
		l.BusyPollInterval = DefaultBusyPollInterval
	}
	if l.IdlePollInterval == 0 {
		l.IdlePollInterval = DefaultIdlePollInterval
	}
}

// pollOnce receives one batch and processes every message in it, returning
// how many messages were received (0 means the queue was empty). Kept
// unexported and separate from Start's sleep loop so tests can drive
// multiple receive/process cycles deterministically, without depending on
// real time (tasks.md T016: burst across multiple fake batches).
func (l *Listener) pollOnce(ctx context.Context) (int, error) {
	msgs, err := l.Queue.Receive(ctx, l.BatchSize)
	if err != nil {
		l.recordReceiveFailure()
		return 0, err
	}
	l.recordReceiveSuccess(time.Now())
	for _, m := range msgs {
		l.handleMessage(ctx, m)
	}
	return len(msgs), nil
}

// handleMessage applies contracts/queue-message.md's processing rules to one
// message: poison check, parse, match, dispatch, delete. Every discard path
// (poison, malformed, unmatched) still deletes the message -- the point of
// pulling it off the queue at all is that nothing here is retried via
// redelivery; periodic reconciliation is the safety net (rule 5/6, research
// R9).
func (l *Listener) handleMessage(ctx context.Context, m azure.QueueMessage) {
	log := logf.FromContext(ctx).WithName("events-listener")

	// Rule 6, checked first and unconditionally: a poison message is never
	// even parsed, so nothing about it is logged beyond queue metadata.
	if m.DequeueCount > l.PoisonThreshold {
		log.Info("discarding poison message",
			"messageID", m.ID, "dequeueCount", m.DequeueCount)
		l.deleteMessage(ctx, m)
		return
	}

	parsed, err := Parse([]byte(m.Text))
	if err != nil {
		// Rule 1: log message id / dequeue metadata only, never the body --
		// err itself is guaranteed to carry only fixed, static text (parser.go).
		log.Info("discarding malformed message",
			"messageID", m.ID, "dequeueCount", m.DequeueCount, "reason", err.Error())
		l.deleteMessage(ctx, m)
		return
	}
	if parsed == nil {
		// Rule 2: an event type/object type this operator does not act on.
		// A clean, expected discard -- not worth logging at normal verbosity.
		log.V(1).Info("discarding non-actionable event", "messageID", m.ID)
		l.deleteMessage(ctx, m)
		return
	}

	matches, err := l.matchingSecretSyncs(ctx, parsed)
	if err != nil {
		// Listing the cache failed: leave the message in place (do not
		// delete) so it is retried on a later poll instead of being lost.
		log.Error(err, "failed to list SecretSyncs for event",
			"messageID", m.ID, "eventID", parsed.ID)
		return
	}
	if len(matches) == 0 {
		// Rule 3: no declaration cares about this vault+secret.
		log.V(1).Info("discarding unmatched event",
			"messageID", m.ID, "eventID", parsed.ID, "vault", parsed.VaultName, "secret", parsed.SecretName)
		l.deleteMessage(ctx, m)
		return
	}

	// Rule 4: trigger the sync engine for every matching declaration by
	// injecting a reconcile request via the shared event.GenericEvent
	// channel (source.Channel, T020). The engine always fetches the latest
	// vault version itself; parsed.Version is not used as a fetch target.
	for i := range matches {
		select {
		case l.Events <- event.GenericEvent{Object: &matches[i]}:
		case <-ctx.Done():
			return
		}
	}

	log.Info("dispatched reconcile requests for event",
		"messageID", m.ID, "eventID", parsed.ID, "vault", parsed.VaultName, "secret", parsed.SecretName,
		"version", parsed.Version, "matches", len(matches))

	// Rule 5: delete only after every matching sync has been accepted for
	// processing (sent into Events above); sync failures are retried via
	// the controller's own requeue-with-backoff, not by redelivering this
	// message.
	l.deleteMessage(ctx, m)
}

// matchingSecretSyncs lists every SecretSync across all namespaces from the
// manager's cached client and returns those whose spec.vault matches parsed
// (vault name case-insensitively, per data-model.md; secret name exactly).
func (l *Listener) matchingSecretSyncs(ctx context.Context, parsed *ParsedEvent) ([]kvsynk8sv1alpha1.SecretSync, error) {
	var list kvsynk8sv1alpha1.SecretSyncList
	if err := l.Client.List(ctx, &list); err != nil {
		return nil, err
	}

	var matches []kvsynk8sv1alpha1.SecretSync
	for _, ss := range list.Items {
		if strings.EqualFold(ss.Spec.Vault.Name, parsed.VaultName) && ss.Spec.Vault.Secret == parsed.SecretName {
			matches = append(matches, ss)
		}
	}
	return matches, nil
}

// deleteMessage deletes m from the queue, logging (metadata only) rather
// than failing loudly if the delete itself errors -- the message simply
// reappears after its visibility timeout and is handled again, which is
// safe because every path through handleMessage is idempotent (latest-wins
// sync, or a discard that would repeat identically).
func (l *Listener) deleteMessage(ctx context.Context, m azure.QueueMessage) {
	if err := l.Queue.Delete(ctx, m.ID, m.PopReceipt); err != nil {
		logf.FromContext(ctx).WithName("events-listener").Error(err, "failed to delete queue message", "messageID", m.ID)
	}
}

// sleepOrDone waits for d or until ctx is cancelled, whichever comes first.
// It returns false when ctx was the reason it returned (caller should stop),
// true when the full duration elapsed normally.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
