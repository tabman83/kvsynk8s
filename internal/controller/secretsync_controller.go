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

package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	kvsynk8sv1alpha1 "github.com/tabman83/kvsynk8s/api/v1alpha1"
	"github.com/tabman83/kvsynk8s/internal/azure"
	"github.com/tabman83/kvsynk8s/internal/sync"
)

// finalizerName lets the reconciler intercept CR deletion long enough to
// delete the managed Secret first (data-model.md state transitions:
// "(CR deleted) --finalizer--> managed Secret deleted --> CR removed";
// FR-002). The Secret also carries an ownerReference back to the SecretSync
// as a garbage-collection backstop, but the finalizer is what lets deletion
// be observed and actioned deterministically rather than racing GC.
//
// Named finalizerName rather than secretSyncFinalizer because the envtest
// spec (secretsync_controller_test.go, T010) declares its own
// secretSyncFinalizer constant with the same value, independently of this
// implementation — the two must not collide as identifiers in the same
// package.
const finalizerName = "kvsynk8s.io/secretsync-finalizer"

// defaultReconcileInterval is the periodic full-reconciliation cadence used
// when ReconcileInterval is left unset: the safety net for missed events,
// vault-side deletions, and in-cluster drift (plan.md, data-model.md). It
// mirrors cmd/main.go's own default.
const defaultReconcileInterval = 4 * time.Hour

// maxConcurrentReconciles is how many SecretSync reconciles may run in
// parallel (FR-008: one secret's persistent failure must not delay others).
// The default used to be 1, which meant a single slow reconcile — e.g. a
// Key Vault call waiting out its retry budget — delayed every other
// SecretSync behind it in the workqueue. Two workers keep the operator's
// API-server and Key Vault footprint small while removing the single-file
// bottleneck; controller-runtime already guarantees the same object is never
// reconciled by two workers at once.
const maxConcurrentReconciles = 2

// reconcileTimeout bounds one Reconcile call. It exists so a hung Key Vault
// or API-server call cannot stall a worker forever — with a finite worker
// pool (maxConcurrentReconciles), a reconcile that never returns would
// permanently shrink it. One minute comfortably exceeds the Azure SDK's
// default retry budget (3 retries with sub-second exponential backoff) plus
// the handful of Kubernetes API calls a reconcile makes; a reconcile that is
// still running after a minute is hung, not slow. On expiry the in-flight
// call fails, Reconcile returns an error, and the rate-limited workqueue
// retries with backoff as for any other transient failure.
const reconcileTimeout = time.Minute

// statusPersistTimeout bounds the terminal status write, which deliberately
// runs on a context detached from the per-reconcile deadline above.
//
// The detachment exists because reconcileTimeout expiring is itself one of the
// outcomes status has to report. A black-holed Key Vault endpoint (as opposed
// to the common fast-error case) burns the entire reconcile budget; the
// resulting deadline error has no HTTP response, so internal/azure classifies
// it as ErrTransient; engine.Sync turns that into Failing/TransientError with
// err == nil — a status the CR must show. Written on the same, now-expired
// context, that update fails instantly and Reconcile returns before
// recordSyncOutcome ever runs, so no SyncFailed Event is emitted either
// (FR-009). Every backoff retry then repeats identically: for the whole
// outage the CR keeps reporting its last good state with a stale
// lastSyncTime, and anything alerting on status.state or on Events sees a
// perfectly healthy SecretSync.
//
// FR-008's worker protection is not weakened by this. Only the status write
// escapes the reconcile deadline — never the vault read, never the Secret
// write — and it gets its own short budget, so a wedged API server still
// cannot pin one of the maxConcurrentReconciles workers.
const statusPersistTimeout = 5 * time.Second

// statusWriteContext derives the context a terminal status write runs on:
// ctx's values with ctx's cancellation and deadline dropped, plus a fresh
// statusPersistTimeout of its own. context.WithoutCancel keeps values, so the
// controller-runtime logger stays in the returned context.
func statusWriteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), statusPersistTimeout)
}

// ManagedSecretCacheOptions returns the cache configuration the manager must
// be built with: the corev1.Secret informer behind Owns(&corev1.Secret{}) is
// restricted, with a label selector, to Secrets carrying
// app.kubernetes.io/managed-by=kvsynk8s — the label SecretWriter puts on
// every Secret it creates. Without this the manager caches every Secret in
// the cluster (values included), which is both a memory and an exposure
// concern for an operator that only ever needs its own.
//
// Deliberate consequence: unmanaged Secrets are invisible to every cached
// Get/List in this package. The reconciler's pre-write Get of the target
// therefore reports NotFound for an unmanaged conflicting Secret, and
// first-writer-wins (FR-012) is enforced by SecretWriter instead: its
// uncached Create fails with AlreadyExists, which it maps to
// ErrTargetConflict, and Reconcile turns that into the same
// Failing/TargetConflict status as before. The envtest suite builds its
// managers with these same options so tests exercise this exact
// configuration.
func ManagedSecretCacheOptions() cache.Options {
	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {
				Label: labels.SelectorFromSet(labels.Set{
					sync.LabelManagedBy: sync.LabelManagedByValue,
				}),
			},
		},
	}
}

// SecretSyncReconciler reconciles a SecretSync object
type SecretSyncReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Reader fetches secret values/versions from Azure Key Vault. Required;
	// wired in cmd/main.go from azure.NewSecretReader() (T007). The queue
	// listener that triggers reconciles on Key Vault change events is wired
	// separately in T020 — this reconciler only needs a way to read the
	// vault, not a way to be notified about it.
	Reader azure.SecretReader

	// ReconcileInterval overrides defaultReconcileInterval when non-zero.
	// Wired in cmd/main.go from --reconcile-interval/RECONCILE_INTERVAL
	// (T023, US3). Tests set this to a short duration so periodic-
	// reconciliation-driven assertions (drift repair, convergence on a
	// changed vault value with no event injected) don't need to wait an
	// hour; production leaves it at the flag's own default.
	ReconcileInterval time.Duration

	// APIReader is an uncached client.Reader (mgr.GetAPIReader()), handed to
	// SecretWriter so its AlreadyExists recovery can re-read the target
	// Secret straight from the API server instead of through the possibly
	// stale informer cache (a queue-triggered reconcile can race the informer
	// on the operator's own just-created Secret). SetupWithManager wires it
	// when left nil, like Recorder; a hand-built reconciler without it makes
	// the writer fall back to its cached Client, which is what the direct
	// (uncached) test clients already are.
	APIReader client.Reader

	// Recorder emits Kubernetes Events on the SecretSync (T027, FR-009):
	// "Synced" (Normal) on every reconcile that ends InSync, "SyncFailed"
	// (Warning) on every reconcile that ends Failing. SetupWithManager wires
	// it from mgr.GetEventRecorder when left nil, which is also what lets
	// every pre-T027 test in this package -- built by hand via
	// reconcilerFor/&SecretSyncReconciler{...} without ever calling
	// SetupWithManager -- keep working unchanged: recordEvent silently no-ops
	// on a nil Recorder rather than requiring every caller to supply one.
	//
	// events.EventRecorder (the events.k8s.io/v1 API) rather than the older
	// client-go/tools/record.EventRecorder: mgr.GetEventRecorderFor is
	// deprecated (SA1019) in favor of mgr.GetEventRecorder, which returns
	// this type.
	Recorder events.EventRecorder
}

// reconcileInterval returns ReconcileInterval when set, or
// defaultReconcileInterval otherwise. Every RequeueAfter this reconciler
// returns on a healthy (non-backoff) path goes through this method rather
// than referencing either constant/field directly, so there is exactly one
// place that resolves "how often do we fully reconcile".
func (r *SecretSyncReconciler) reconcileInterval() time.Duration {
	if r.ReconcileInterval > 0 {
		return r.ReconcileInterval
	}
	return defaultReconcileInterval
}

// recordEvent emits a Kubernetes Event on ss (T027, FR-009) if a Recorder is
// configured; it is a silent no-op otherwise, so nothing here ever requires
// every reconciler in every test to wire one up. reason is always one of the
// two fixed identifiers "Synced"/"SyncFailed" (never anything derived from
// upstream error text), and message is built exclusively from vault name,
// secret name, target namespace/name, and version identifiers -- constitution
// I / FR-010 forbid anything else, and every call site below sources message
// from status.Message/status.Reason, which classifyReaderError and this
// package's own status-building code already restrict the same way.
func (r *SecretSyncReconciler) recordEvent(ss *kvsynk8sv1alpha1.SecretSync, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}
	// action is set equal to reason: both "Synced" and "SyncFailed" already
	// describe what the controller did, and this operator has no separate
	// action taxonomy worth introducing (constitution III: simplicity first).
	r.Recorder.Eventf(ss, nil, eventType, reason, reason, message)
}

// recordSyncOutcome is the single choke point for everything an operator can
// observe about one reconcile's result: the Kubernetes Event (T027), the
// sync-result counter, and the log line.
//
// "Synced" on InSync, "SyncFailed" (carrying status.Reason in its message) on
// Failing. Pending is not terminal — it is only ever set once, transiently,
// before the very first conflict check or vault read — so it gets no event, no
// counter increment, and no log line of its own.
//
// The logs are here rather than at the three call sites for the same reason
// the Event is: all three terminal paths already funnel through this function,
// so nothing can grow a fourth exit that reports nothing. They log at default
// verbosity on purpose. Production zap runs at info level, so the V(1) trace
// at the end of Reconcile is invisible there, which used to mean a classified
// failure produced no operator log at all — everything lived in status and the
// Event, neither of which is where anyone looks while tailing logs. The
// recovery line is gated on the transition out of Failing, so a healthy fleet
// does not re-log every 4h and a brand-new declaration is not announced as a
// recovery; the failing line is not gated, because a failure that keeps
// repeating is itself the signal.
//
// status.Message is safe to log: every one of them is built from identifiers
// and fixed text, never from an upstream error string (constitution I /
// FR-010), which is what classifyReaderError and this file's own status
// literals guarantee.
func (r *SecretSyncReconciler) recordSyncOutcome(
	ctx context.Context,
	ss *kvsynk8sv1alpha1.SecretSync,
	previousState kvsynk8sv1alpha1.SecretSyncState,
	status kvsynk8sv1alpha1.SecretSyncStatus,
	targetName string,
) {
	log := logf.FromContext(ctx)

	switch status.State {
	case kvsynk8sv1alpha1.SecretSyncStateInSync:
		r.recordEvent(ss, corev1.EventTypeNormal, "Synced", fmt.Sprintf(
			"synced secret %q (version %q) from vault %q into %s/%s",
			ss.Spec.Vault.Secret, status.SyncedVersion, ss.Spec.Vault.Name, ss.Namespace, targetName))
		recordSyncResult(status.State, "")
		// Only a real recovery, not a first sync: Pending -> InSync is a new
		// declaration working as intended and says nothing an operator needs
		// at default verbosity.
		if previousState == kvsynk8sv1alpha1.SecretSyncStateFailing {
			log.Info("SecretSync recovered",
				"namespace", ss.Namespace, "name", ss.Name,
				"vault", ss.Spec.Vault.Name, "secret", ss.Spec.Vault.Secret,
				"version", status.SyncedVersion)
		}
	case kvsynk8sv1alpha1.SecretSyncStateFailing:
		r.recordEvent(ss, corev1.EventTypeWarning, "SyncFailed", fmt.Sprintf("%s: %s", status.Reason, status.Message))
		recordSyncResult(status.State, status.Reason)
		log.Info("SecretSync is failing",
			"namespace", ss.Namespace, "name", ss.Name,
			"vault", ss.Spec.Vault.Name, "secret", ss.Spec.Vault.Secret,
			"reason", status.Reason, "message", status.Message)
	}
}

// +kubebuilder:rbac:groups=kvsynk8s.io,resources=secretsyncs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kvsynk8s.io,resources=secretsyncs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kvsynk8s.io,resources=secretsyncs/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;delete
//
// events.k8s.io, not the core ("") group: the Recorder above comes from
// mgr.GetEventRecorder, which writes through the events.k8s.io/v1 API, and
// RBAC authorizes per API group — a core-group events grant does not cover
// it, so with the old marker every Eventf got 403 Forbidden in a real
// install (the broadcaster only logs the failure, nothing surfaces it).
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile drives one SecretSync towards the state data-model.md describes:
// on deletion, the managed Secret is removed before the finalizer is
// dropped; otherwise the finalizer is ensured, a namespace-scoped target
// conflict check runs (FR-012, "first writer wins"), and the sync engine
// (internal/sync/engine.go, T012) computes the desired status and Secret,
// which are written here since the engine itself does no Kubernetes API I/O.
//
// Errors returned here (as opposed to being captured in status) are the
// programmer-error class — a nil Reader, or a failed Kubernetes API call —
// plus one expected case that constitution II specifically calls out for
// backoff: TransientError. Both are deliberately propagated so
// controller-runtime's rate-limited workqueue applies exponential backoff
// (constitution II: retries, not a crash or a hot loop). Every other
// *expected* failure mode (vault secret missing/disabled/access-denied,
// target conflict) is instead reported via status with err == nil, and this
// reconcile still requeues after reconcileInterval() so a since-resolved
// cause is retried without needing an external trigger.
func (r *SecretSyncReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Per-reconcile deadline (FR-008): a hung upstream call fails the
	// reconcile after reconcileTimeout instead of occupying one of the
	// maxConcurrentReconciles workers forever.
	ctx, cancelTimeout := context.WithTimeout(ctx, reconcileTimeout)
	defer cancelTimeout()

	log := logf.FromContext(ctx)

	var ss kvsynk8sv1alpha1.SecretSync
	if err := r.Get(ctx, req.NamespacedName, &ss); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("kvsynk8s: get secretsync %s: %w", req.NamespacedName, err)
	}

	// Captured before anything below reassigns ss.Status wholesale, so
	// recordSyncOutcome can tell "still failing" from "just recovered" and log
	// the recovery exactly once instead of on every periodic pass.
	previousState := ss.Status.State

	targetName := resolveTargetName(&ss)
	targetKey := types.NamespacedName{Namespace: ss.Namespace, Name: targetName}
	dataKey := resolveDataKey(&ss)

	if !ss.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.reconcileDelete(ctx, &ss, targetKey)
	}

	// data-model.md: "(created) -> Pending" is a distinct, observable state
	// from the sync-outcome states below. Persist it once, on the very first
	// reconcile of a new CR, before any conflict check or vault read runs.
	if ss.Status.State == "" {
		ss.Status.State = kvsynk8sv1alpha1.SecretSyncStatePending
		setReadyCondition(&ss, &ss.Status)
		if err := r.Status().Update(ctx, &ss); err != nil {
			return ctrl.Result{}, fmt.Errorf("kvsynk8s: set pending status for %s: %w", req.NamespacedName, err)
		}
	}

	// FR-012, "first writer wins": at most one SecretSync may own a given
	// namespace+target.secretName. This runs before any vault read or
	// Secret write so a losing declaration never touches either.
	conflictingOwner, err := r.findConflictingOwner(ctx, &ss)
	if err != nil {
		return ctrl.Result{}, err
	}
	if conflictingOwner {
		conflictStatus := kvsynk8sv1alpha1.SecretSyncStatus{
			State:  kvsynk8sv1alpha1.SecretSyncStateFailing,
			Reason: sync.ReasonTargetConflict,
			Message: fmt.Sprintf(
				"secret %s/%s is already claimed by another SecretSync", ss.Namespace, targetName,
			),
			ObservedGeneration: ss.Generation,
			LastSyncTime:       ss.Status.LastSyncTime,
			SyncedVersion:      ss.Status.SyncedVersion,
		}
		// Built into a local first: setReadyCondition copies the existing
		// conditions forward off ss.Status, which assigning to it would have
		// already thrown away.
		setReadyCondition(&ss, &conflictStatus)
		ss.Status = conflictStatus
		// Terminal status, so it is persisted on a detached context like the
		// one at the end of Reconcile — see statusPersistTimeout. This path
		// cannot itself exhaust the reconcile deadline (it runs off cached
		// reads, before any vault call), but the rule that a terminal status
		// and its Event always survive the deadline is worth having hold at
		// every site rather than only where it is currently load-bearing.
		statusCtx, cancelStatus := statusWriteContext(ctx)
		defer cancelStatus()
		if err := r.Status().Update(statusCtx, &ss); err != nil {
			return ctrl.Result{}, fmt.Errorf("kvsynk8s: update status for %s: %w", req.NamespacedName, err)
		}
		r.recordSyncOutcome(ctx, &ss, previousState, ss.Status, targetName)
		return ctrl.Result{RequeueAfter: r.reconcileInterval()}, nil
	}

	// In production this Get reads through the manager's cache, which
	// ManagedSecretCacheOptions restricts to kvsynk8s-managed Secrets: an
	// unmanaged Secret squatting on the target name comes back NotFound here
	// and is instead detected by SecretWriter's Create (AlreadyExists ->
	// ErrTargetConflict) below.
	var existing *corev1.Secret
	fetched := &corev1.Secret{}
	if err := r.Get(ctx, targetKey, fetched); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("kvsynk8s: get secret %s: %w", targetKey, err)
		}
	} else {
		existing = fetched
	}

	// Captured before Sync runs: Sync mutates `existing` in place on its
	// write path (it is the same *corev1.Secret returned as `desired`), so
	// this is the only chance to see the pre-sync annotation, data and
	// ownerReferences for the idempotency comparison below. priorOwned in
	// particular MUST be read here: Sync's write path stamps the controller
	// ownerReference onto `existing` in memory, so after Sync returns there is
	// no way left to tell an already-owned Secret from one this reconcile is
	// about to adopt.
	var priorVersion string
	var priorData map[string][]byte
	var priorOwned bool
	if existing != nil {
		priorVersion = existing.Annotations[sync.AnnotationVersion]
		priorData = existing.DeepCopy().Data
		priorOwned = ownedBy(existing, &ss)
	}

	engine := sync.Engine{Reader: r.Reader}
	status, desired, err := engine.Sync(ctx, &ss, existing)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("kvsynk8s: sync %s: %w", req.NamespacedName, err)
	}

	if status.State != kvsynk8sv1alpha1.SecretSyncStateFailing {
		// Finalizer is only added once we know this SecretSync is not going
		// to be defeated by an ownership check (the CR-vs-CR conflict check
		// above, or engine.Sync's own pre-existing-unmanaged-Secret check,
		// have both already passed at this point). With the filtered cache
		// (ManagedSecretCacheOptions) an unmanaged conflicting Secret is not
		// visible to engine.Sync, so a losing declaration can still reach
		// here and acquire the finalizer before the write below fails with
		// ErrTargetConflict — which is why reconcileDelete independently
		// verifies ownership before deleting anything: a finalizer is never
		// treated as proof of ownership.
		if !controllerutil.ContainsFinalizer(&ss, finalizerName) {
			controllerutil.AddFinalizer(&ss, finalizerName)
			if err := r.Update(ctx, &ss); err != nil {
				return ctrl.Result{}, fmt.Errorf("kvsynk8s: add finalizer to %s: %w", req.NamespacedName, err)
			}
		}

		// Idempotency (FR-005, data-model.md): only write when the Secret is
		// new, this SecretSync does not own it yet, the synced version
		// changed, or the Secret's pre-sync data does not match what the
		// engine computed — a changed spec.target.dataKey, or in-cluster drift
		// on the managed Secret's data (US3 AS-2, FR-007), both of which leave
		// the version annotation intact. Without this check an Update would
		// fire on every reconcile even when nothing changed, which — combined
		// with Owns(&corev1.Secret{}) below — would retrigger a reconcile for
		// its own no-op write and loop forever; comparing against the pre-sync
		// snapshot keeps the no-op case write-free while still repairing real
		// differences.
		//
		// !priorOwned is what makes this gate agree with the engine's own
		// ownership rule (FR-012). A Secret carrying the managed-by label but
		// no controller ownerReference — left behind by `kubectl delete
		// secretsync --cascade=orphan`, restored by a backup/GitOps tool that
		// drops ownerReferences, or applied by hand — is not a conflict: the
		// writer adopts it. But adoption is a WRITE, and if version and data
		// happen to match already, the three checks above would all be false
		// and the ownerReference the engine stamped in memory would never
		// reach the API server. The CR would then report InSync over a Secret
		// nothing links back to it: Owns(&corev1.Secret{}) could not map
		// in-cluster edits of that Secret to any reconcile, and deleting the
		// CR would leave the Secret behind (reconcileDelete requires
		// ownership). Requiring ownership here forces exactly one adopting
		// write; the next reconcile sees priorOwned and goes quiet again.
		if existing == nil || !priorOwned || priorVersion != status.SyncedVersion ||
			!maps.EqualFunc(priorData, desired.Data, bytes.Equal) {
			// The actual Kubernetes write goes exclusively through
			// SecretWriter (constitution I: one value-carrying code path),
			// not r.Create/r.Update directly. value/version are read back off
			// `desired` — the Secret engine.Sync already built — rather than
			// issuing a second vault read.
			writer := sync.SecretWriter{Client: r.Client, Reader: r.APIReader}
			value := string(desired.Data[dataKey])
			writeErr := writer.CreateOrUpdate(ctx, &ss, targetKey.Namespace, targetKey.Name, dataKey, value, status.SyncedVersion)
			if writeErr != nil {
				if !isTerminalWriteError(writeErr) {
					// Any other write failure — an admission policy rejecting
					// the Secret, a ResourceQuota, RBAC drift, a wedged API
					// server — is returned for backoff retry, but the CR must
					// not go on advertising its last good state while that
					// lasts. Returning straight away would skip the status
					// update at the end of Reconcile: engine.Sync already
					// computed InSync at the new version, nothing would persist
					// it, and the CR would keep reporting InSync at the OLD
					// version with a stale lastSyncTime and no SyncFailed Event
					// for the whole outage — the same unobservable failure
					// statusPersistTimeout exists to prevent for a hung vault
					// read, on the write side.
					//
					// SecretWriteFailed, not TransientError: the reconcile is
					// still retried with backoff (this branch returns writeErr),
					// but the reason now says which half of the operator broke.
					// A Key Vault timeout and a cluster policy rejecting the
					// write used to be indistinguishable from kubectl, from the
					// Event and from any metric, while having completely
					// different fixes.
					status = kvsynk8sv1alpha1.SecretSyncStatus{
						State:  kvsynk8sv1alpha1.SecretSyncStateFailing,
						Reason: sync.ReasonSecretWriteFailed,
						// Identifiers and fixed text only, never writeErr's own
						// string: it comes from the Kubernetes API, and what an
						// admission webhook echoes back into it is not something
						// this package has audited (constitution I / FR-010).
						Message: fmt.Sprintf(
							"failed writing secret %s/%s; keeping last synced value", ss.Namespace, targetName,
						),
						ObservedGeneration: ss.Generation,
						LastSyncTime:       ss.Status.LastSyncTime,
						SyncedVersion:      ss.Status.SyncedVersion,
					}
					setReadyCondition(&ss, &status)
					ss.Status = status
					// Detached from the reconcile deadline for the same reason
					// as every other terminal status write here: a write that
					// failed because the API server is slow would otherwise fail
					// to report that it failed.
					statusCtx, cancelStatus := statusWriteContext(ctx)
					defer cancelStatus()
					if statusErr := r.Status().Update(statusCtx, &ss); statusErr != nil {
						// Logged, not returned: writeErr is the more useful
						// error to hand back for backoff, and the Event below
						// still gets the failure out even when status is stuck.
						log.Error(statusErr, "failed to persist Failing status after a secret write error",
							"namespace", targetKey.Namespace, "name", targetKey.Name)
					}
					r.recordSyncOutcome(ctx, &ss, previousState, status, targetName)
					return ctrl.Result{}, fmt.Errorf("kvsynk8s: write secret %s: %w", targetKey, writeErr)
				}
				// A terminal write failure: nothing in the cluster will change
				// on its own to make this write succeed. Reported through
				// status and an Event and then requeued at the ordinary
				// periodic cadence below — never returned as an error. Handing
				// a permanent condition to the rate-limited workqueue would
				// retry it forever, and every one of those retries pays a Key
				// Vault read for a state only a human can clear.
				//
				// Two shapes reach here. An unmanaged Secret occupies the
				// target name: with the filtered cache
				// (ManagedSecretCacheOptions) this is the normal detection
				// path for that case — the cached Get above cannot see
				// unmanaged Secrets — and it also covers the TOCTOU race where
				// another actor created the Secret between that Get and the
				// write just above. Or the managed Secret is immutable, so
				// Kubernetes refuses to change its data at all. Either way the
				// CR's status says which, rather than going stale for this
				// cycle.
				reason := sync.ReasonTargetConflict
				message := fmt.Sprintf(
					"secret %s/%s already exists and is not managed by kvsynk8s", ss.Namespace, targetName)
				if errors.Is(writeErr, sync.ErrTargetImmutable) {
					reason = sync.ReasonTargetImmutable
					message = fmt.Sprintf(
						"secret %s/%s is immutable and cannot be updated; keeping last synced value",
						ss.Namespace, targetName)
					// Kubernetes offers no way to unset immutable on an
					// existing Secret, so the remedy is not obvious and is
					// worth spelling out where an operator tailing logs will
					// see it. targetKey is a namespace/name pair; no value can
					// reach this call.
					log.Info("target secret is immutable, so its value can no longer be rotated; "+
						"delete the Secret to resume syncing (immutable cannot be unset in place)",
						"namespace", targetKey.Namespace, "name", targetKey.Name)
				}
				status = kvsynk8sv1alpha1.SecretSyncStatus{
					State:              kvsynk8sv1alpha1.SecretSyncStateFailing,
					Reason:             reason,
					Message:            message,
					ObservedGeneration: ss.Generation,
					LastSyncTime:       ss.Status.LastSyncTime,
					SyncedVersion:      ss.Status.SyncedVersion,
				}
			}
		}
	}

	setReadyCondition(&ss, &status)
	ss.Status = status
	// Detached from the per-reconcile deadline on purpose (statusPersistTimeout):
	// this is the write that makes a reconcile which ran out of budget — a hung
	// vault read is the realistic case — observable at all, and it is what lets
	// recordSyncOutcome below still emit the SyncFailed Event.
	statusCtx, cancelStatus := statusWriteContext(ctx)
	defer cancelStatus()
	if err := r.Status().Update(statusCtx, &ss); err != nil {
		return ctrl.Result{}, fmt.Errorf("kvsynk8s: update status for %s: %w", req.NamespacedName, err)
	}
	r.recordSyncOutcome(ctx, &ss, previousState, status, targetName)

	log.V(1).Info("reconciled SecretSync",
		"vault", ss.Spec.Vault.Name, "secret", ss.Spec.Vault.Secret, "state", status.State)

	// Garbage-collect the previous target after a spec.target.secretName
	// rename: without this, the Secret written under the old name would stay
	// in the namespace (labelled managed-by=kvsynk8s, carrying the last synced
	// value) until the CR itself was deleted. Runs only when this reconcile
	// ended InSync — i.e. the current target verifiably holds the value — so a
	// failed or conflicted sync can never trigger a deletion. Placed after the
	// status update so a GC failure (returned for backoff retry) never blocks
	// the status from reflecting the sync outcome; the sweep itself is
	// idempotent and re-runs on the retry.
	if status.State == kvsynk8sv1alpha1.SecretSyncStateInSync {
		if err := r.deleteStaleManagedSecrets(ctx, &ss, targetName); err != nil {
			return ctrl.Result{}, err
		}
	}

	if status.State == kvsynk8sv1alpha1.SecretSyncStateFailing && retriedWithBackoff(status.Reason) {
		// Constitution II / FR-008: transient failures (network, throttling,
		// auth token expiry) are retried with exponential backoff via
		// controller-runtime's rate-limited workqueue, not at the same fixed
		// cadence used for a healthy periodic reconcile.
		return ctrl.Result{}, fmt.Errorf("kvsynk8s: %s syncing %s, retrying with backoff", status.Reason, req.NamespacedName)
	}

	return ctrl.Result{RequeueAfter: r.reconcileInterval()}, nil
}

// reconcileDelete handles a SecretSync with a non-zero DeletionTimestamp: it
// deletes the managed Secret — but only if this SecretSync actually owns it
// (see ownedBy) — and removes the finalizer so the API server can finish
// deleting the CR. If the finalizer is already gone, deletion has already
// been handled (or never started) and there is nothing to do.
//
// The ownership check matters because the finalizer alone is not proof of
// ownership: a SecretSync that lost a target-name conflict, or that collided
// with a pre-existing unmanaged Secret, must never delete whatever object
// currently sits at that name — it never created or wrote it (FR-012, "must
// never be silently adopted or overwritten" applies equally to deletion).
func (r *SecretSyncReconciler) reconcileDelete(
	ctx context.Context, ss *kvsynk8sv1alpha1.SecretSync, targetKey types.NamespacedName,
) error {
	if !controllerutil.ContainsFinalizer(ss, finalizerName) {
		return nil
	}

	// Through the filtered cache (ManagedSecretCacheOptions) an unmanaged
	// Secret at the target name comes back NotFound here, which lands in the
	// default branch below: nothing is deleted, exactly as the ownedBy check
	// would have decided when it could still see the object.
	var secret corev1.Secret
	switch err := r.Get(ctx, targetKey, &secret); {
	case err == nil:
		if ownedBy(&secret, ss) {
			if err := r.Delete(ctx, &secret); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("kvsynk8s: delete managed secret %s: %w", targetKey, err)
			}
		}
		// Else: a Secret exists at this name but this SecretSync never owned
		// it (conflict loser, or refused pre-existing unmanaged Secret) —
		// leave it untouched.
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("kvsynk8s: get secret %s: %w", targetKey, err)
	}

	controllerutil.RemoveFinalizer(ss, finalizerName)
	if err := r.Update(ctx, ss); err != nil {
		return fmt.Errorf("kvsynk8s: remove finalizer from %s/%s: %w", ss.Namespace, ss.Name, err)
	}

	return nil
}

// deleteStaleManagedSecrets deletes every Secret in ss's namespace that this
// SecretSync previously managed under a different target name — the orphan a
// spec.target.secretName rename leaves behind. Candidates are found by the
// managed-by label, but a candidate is only ever deleted when it carries a
// controller OwnerReference whose UID matches ss (ownedBy): a Secret owned by
// a different SecretSync, or not owned by any, is never touched, no matter
// what it is named or labelled. Callers gate this on the current target's
// sync having succeeded, so a failed sync never deletes anything.
//
// Logging is identifier-only (namespace/name), never values (constitution I).
func (r *SecretSyncReconciler) deleteStaleManagedSecrets(
	ctx context.Context, ss *kvsynk8sv1alpha1.SecretSync, currentTargetName string,
) error {
	var list corev1.SecretList
	if err := r.List(ctx, &list,
		client.InNamespace(ss.Namespace),
		client.MatchingLabels{sync.LabelManagedBy: sync.LabelManagedByValue},
	); err != nil {
		return fmt.Errorf("kvsynk8s: list managed secrets in namespace %s: %w", ss.Namespace, err)
	}

	log := logf.FromContext(ctx)
	for i := range list.Items {
		secret := &list.Items[i]
		if secret.Name == currentTargetName {
			continue
		}
		if !ownedBy(secret, ss) {
			continue
		}
		if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("kvsynk8s: delete stale managed secret %s/%s: %w", secret.Namespace, secret.Name, err)
		}
		log.Info("deleted stale managed secret left behind by a target rename",
			"namespace", secret.Namespace, "name", secret.Name,
			"currentTarget", currentTargetName)
	}
	return nil
}

// ownedBy reports whether secret carries a controller OwnerReference back to
// ss, i.e. whether ss is the SecretSync that actually created/claimed it —
// as opposed to merely sharing its target name (FR-012). It delegates to
// sync.ControllerOwnedBy, the single ownership predicate the engine and the
// writer also use, so the controller can never disagree with them.
func ownedBy(secret *corev1.Secret, ss *kvsynk8sv1alpha1.SecretSync) bool {
	return sync.ControllerOwnedBy(secret, ss)
}

// setReadyCondition mirrors a freshly computed status into the standard Ready
// condition, so `kubectl wait --for=condition=Ready`, Argo CD and Flux have
// something to key on. InSync is Ready=True, Failing is Ready=False carrying
// the same reason, anything else (Pending) is Ready=Unknown.
//
// Two things here are load-bearing and easy to break:
//
// The existing conditions are copied forward from ss first. Reconcile assigns
// ss.Status = <fresh struct> wholesale in several places, and engine.Sync
// builds a fresh status of its own, so without this copy the slice handed to
// meta.SetStatusCondition would always be empty and every reconcile would
// reset LastTransitionTime — destroying the one piece of information a
// condition adds over the plain state field.
//
// And Reason is never empty. metav1.Condition.Reason is required, with a
// non-empty pattern, so the API server would reject the whole status write on
// the success path if it were left as status.Reason (which is "" when nothing
// failed).
func setReadyCondition(ss *kvsynk8sv1alpha1.SecretSync, status *kvsynk8sv1alpha1.SecretSyncStatus) {
	condition := metav1.Condition{
		Type:               kvsynk8sv1alpha1.ConditionReady,
		ObservedGeneration: status.ObservedGeneration,
	}

	switch status.State {
	case kvsynk8sv1alpha1.SecretSyncStateInSync:
		condition.Status = metav1.ConditionTrue
		condition.Reason = "Synced"
		condition.Message = fmt.Sprintf("synced version %q", status.SyncedVersion)
	case kvsynk8sv1alpha1.SecretSyncStateFailing:
		condition.Status = metav1.ConditionFalse
		condition.Reason = status.Reason
		condition.Message = status.Message
	default:
		condition.Status = metav1.ConditionUnknown
		condition.Reason = "Pending"
		condition.Message = "no sync attempt has completed yet"
	}
	if condition.Reason == "" {
		condition.Reason = "Unknown"
	}

	status.Conditions = ss.Status.Conditions
	meta.SetStatusCondition(&status.Conditions, condition)
}

// isTerminalWriteError reports whether a SecretWriter failure is one no amount
// of retrying can clear: something in the cluster has to change first. Either
// a foreign Secret occupies the target name (FR-012), or the managed Secret
// was marked immutable so its data can no longer be rewritten. Both are
// reported through status and an Event and then requeued at the periodic
// interval; everything else goes back to the workqueue for exponential
// backoff.
//
// Naming the distinction the write-failure branch was already making
// implicitly means a third terminal condition is a one-line change here rather
// than another inline errors.Is buried in Reconcile.
func isTerminalWriteError(err error) bool {
	return errors.Is(err, sync.ErrTargetConflict) || errors.Is(err, sync.ErrTargetImmutable)
}

// retriedWithBackoff reports whether a Failing reason should be handed back to
// the rate-limited workqueue rather than left to the periodic requeue.
//
// TransientError is the obvious one: network, throttling, an unclassified
// upstream error. SecretWriteFailed belongs to the same class and is listed
// for completeness, though in practice its branch returns writeErr directly
// and never reaches this gate. AuthenticationFailed is here for a less
// obvious reason: it usually
// means workload identity is misconfigured, which a human has to fix, but it
// is also what a pod sees in the seconds before its projected token exists.
// Backoff covers both — it converges in seconds when the cause is a startup
// race, and settles at the rate limiter's ceiling when it is not, which beats
// waiting a full reconcile interval to retry a broken identity.
//
// Everything else (SecretNotFound, AccessDenied, SourceDeleted,
// SourceDisabled, TargetConflict, TargetImmutable) needs a change in Azure or
// in the cluster before it can succeed, so retrying it fast buys nothing and
// costs a Key Vault read per attempt.
func retriedWithBackoff(reason string) bool {
	return reason == sync.ReasonTransientError ||
		reason == sync.ReasonSecretWriteFailed ||
		reason == sync.ReasonAuthenticationFailed
}

// resolveTargetName applies the data-model.md default for
// spec.target.secretName: the SecretSync's own name when unset. The CRD
// schema cannot express this default itself (it cannot reference
// metadata.name), so both the engine and the controller apply it the same
// way — kept here as the single controller-side copy of that rule, used
// before the engine even runs (the target Secret must be looked up, and
// conflict-checked, before Sync is called).
func resolveTargetName(ss *kvsynk8sv1alpha1.SecretSync) string {
	if ss.Spec.Target.SecretName != "" {
		return ss.Spec.Target.SecretName
	}
	return ss.Name
}

// resolveDataKey applies the data-model.md default for spec.target.dataKey:
// the vault secret name when unset. Mirrors resolveTargetName — kept here,
// duplicating engine.Sync's own internal defaulting, because the controller
// needs it to route the actual write through SecretWriter.CreateOrUpdate,
// which takes an explicit dataKey rather than the *corev1.Secret engine.Sync
// returns.
func resolveDataKey(ss *kvsynk8sv1alpha1.SecretSync) string {
	if ss.Spec.Target.DataKey != "" {
		return ss.Spec.Target.DataKey
	}
	return ss.Spec.Vault.Secret
}

// findConflictingOwner reports whether some other SecretSync in owner's
// namespace has a stronger claim on the same resolved target Secret name
// (data-model.md "Identity & uniqueness", FR-012: at most one SecretSync may
// own a given namespace+target.secretName; first writer wins).
//
// A competing declaration that is itself already Failing/TargetConflict is
// ignored — a defeated claim does not get to veto anyone else, otherwise two
// losers could deadlock each other forever. Precedence between two live
// claims is resolved deterministically (CreationTimestamp, then Name) rather
// than by reconcile order, so the outcome does not depend on which one
// happens to be processed first.
func (r *SecretSyncReconciler) findConflictingOwner(ctx context.Context, owner *kvsynk8sv1alpha1.SecretSync) (bool, error) {
	targetName := resolveTargetName(owner)

	var list kvsynk8sv1alpha1.SecretSyncList
	if err := r.List(ctx, &list, client.InNamespace(owner.Namespace)); err != nil {
		return false, fmt.Errorf("kvsynk8s: list secretsyncs in namespace %s: %w", owner.Namespace, err)
	}

	for i := range list.Items {
		other := &list.Items[i]
		if other.Name == owner.Name {
			continue
		}
		if resolveTargetName(other) != targetName {
			continue
		}
		if other.Status.State == kvsynk8sv1alpha1.SecretSyncStateFailing && other.Status.Reason == sync.ReasonTargetConflict {
			continue
		}
		if takesPrecedence(other, owner) {
			return true, nil
		}
	}
	return false, nil
}

// takesPrecedence reports whether a has the stronger claim over b on a
// shared target Secret name: earlier CreationTimestamp wins; ties (down to
// envtest's one-second timestamp resolution) break on Name, giving a strict
// deterministic order.
func takesPrecedence(a, b *kvsynk8sv1alpha1.SecretSync) bool {
	at, bt := a.CreationTimestamp.Time, b.CreationTimestamp.Time
	if !at.Equal(bt) {
		return at.Before(bt)
	}
	return a.Name < b.Name
}

// SetupWithManager sets up the controller with the Manager. Owns(&corev1.Secret{})
// means drift on a managed Secret (deleted or edited in-cluster) re-triggers
// a reconcile of its owning SecretSync automatically, without waiting for
// the periodic RequeueAfter (US3 drift repair).
//
// Startup convergence (US3, T023) needs no code here either: For(&SecretSync{})
// backs the controller with a client-go informer, and that informer's initial
// List when the manager starts delivers every already-existing SecretSync as
// a Create event, which the default predicates let through to the workqueue —
// so every declaration gets reconciled once as soon as the cache syncs, with
// no replayed queue events required. Exercised in secretsync_controller_test.go
// (T021) by starting a real manager and asserting convergence without ever
// calling Reconcile by hand.
//
// syncEvents is the channel the queue listener (internal/events, T019)
// sends matched SecretSync objects on; a nil channel is the US1 contract:
// the queue is completely unconfigured, so no WatchesRawSource is added at
// all and reconciliation runs exclusively through the normal For/Owns
// watches plus the periodic RequeueAfter (cmd/main.go only builds a listener
// and a non-nil channel when --queue-url/QUEUE_URL is set).
func (r *SecretSyncReconciler) SetupWithManager(mgr ctrl.Manager, syncEvents <-chan event.GenericEvent) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("kvsynk8s")
	}
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}

	// Publish the cached client the state collector lists through, and
	// register the sync-path metrics. This is the only place that has both the
	// manager's client and a once-per-process guarantee in production; see
	// registerSyncMetrics for why the once matters in the test suite too.
	registerSyncMetrics(mgr.GetClient())

	bldr := ctrl.NewControllerManagedBy(mgr).
		For(&kvsynk8sv1alpha1.SecretSync{}).
		Owns(&corev1.Secret{}).
		// FR-008: more than one worker, so one SecretSync stuck in a slow
		// reconcile (e.g. a Key Vault call working through its retry budget)
		// cannot delay every other declaration behind it in the queue. Each
		// reconcile is additionally bounded by reconcileTimeout (see
		// Reconcile) so a hung call cannot pin a worker forever.
		WithOptions(ctrlcontroller.Options{MaxConcurrentReconciles: maxConcurrentReconciles}).
		Named("secretsync")

	if syncEvents != nil {
		// Near-realtime path (US2): the listener has already matched the
		// notification to these specific SecretSync objects (case-insensitive
		// vault match, data-model.md), so EnqueueRequestForObject just reads
		// their namespace/name back off into a reconcile.Request -- the same
		// Reconcile this controller already runs for every other trigger.
		bldr = bldr.WatchesRawSource(source.Channel(syncEvents, &handler.EnqueueRequestForObject{}))
	}

	return bldr.Complete(r)
}
