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
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// recordSyncOutcome emits the terminal-state Event for one reconcile's
// result, per T027: "Synced" on InSync, "SyncFailed" (carrying status.Reason
// in its message) on Failing. Pending is not terminal (it is only ever set
// once, transiently, before the very first conflict check/vault read) and
// gets no event of its own.
func (r *SecretSyncReconciler) recordSyncOutcome(ss *kvsynk8sv1alpha1.SecretSync, status kvsynk8sv1alpha1.SecretSyncStatus, targetName string) {
	switch status.State {
	case kvsynk8sv1alpha1.SecretSyncStateInSync:
		r.recordEvent(ss, corev1.EventTypeNormal, "Synced", fmt.Sprintf(
			"synced secret %q (version %q) from vault %q into %s/%s",
			ss.Spec.Vault.Secret, status.SyncedVersion, ss.Spec.Vault.Name, ss.Namespace, targetName))
	case kvsynk8sv1alpha1.SecretSyncStateFailing:
		r.recordEvent(ss, corev1.EventTypeWarning, "SyncFailed", fmt.Sprintf("%s: %s", status.Reason, status.Message))
	}
}

// +kubebuilder:rbac:groups=kvsynk8s.io,resources=secretsyncs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kvsynk8s.io,resources=secretsyncs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kvsynk8s.io,resources=secretsyncs/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

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

	targetName := resolveTargetName(&ss)
	targetKey := types.NamespacedName{Namespace: ss.Namespace, Name: targetName}
	dataKey := resolveDataKey(&ss)

	if !ss.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &ss, targetKey)
	}

	// data-model.md: "(created) -> Pending" is a distinct, observable state
	// from the sync-outcome states below. Persist it once, on the very first
	// reconcile of a new CR, before any conflict check or vault read runs.
	if ss.Status.State == "" {
		ss.Status.State = kvsynk8sv1alpha1.SecretSyncStatePending
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
		ss.Status = kvsynk8sv1alpha1.SecretSyncStatus{
			State:  kvsynk8sv1alpha1.SecretSyncStateFailing,
			Reason: sync.ReasonTargetConflict,
			Message: fmt.Sprintf(
				"secret %s/%s is already claimed by another SecretSync", ss.Namespace, targetName,
			),
			ObservedGeneration: ss.Generation,
			LastSyncTime:       ss.Status.LastSyncTime,
			SyncedVersion:      ss.Status.SyncedVersion,
		}
		if err := r.Status().Update(ctx, &ss); err != nil {
			return ctrl.Result{}, fmt.Errorf("kvsynk8s: update status for %s: %w", req.NamespacedName, err)
		}
		r.recordSyncOutcome(&ss, ss.Status, targetName)
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
	// this is the only chance to see the pre-sync annotation for the
	// idempotency comparison below.
	var priorVersion string
	if existing != nil {
		priorVersion = existing.Annotations[sync.AnnotationVersion]
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
		// new or the synced version actually changed. Without this check an
		// Update would fire on every reconcile even when nothing changed,
		// which — combined with Owns(&corev1.Secret{}) below — would retrigger
		// a reconcile for its own no-op write and loop forever.
		if existing == nil || priorVersion != status.SyncedVersion {
			// The actual Kubernetes write goes exclusively through
			// SecretWriter (constitution I: one value-carrying code path),
			// not r.Create/r.Update directly. value/version are read back off
			// `desired` — the Secret engine.Sync already built — rather than
			// issuing a second vault read.
			writer := sync.SecretWriter{Client: r.Client}
			value := string(desired.Data[dataKey])
			writeErr := writer.CreateOrUpdate(ctx, &ss, targetKey.Namespace, targetKey.Name, dataKey, value, status.SyncedVersion)
			if writeErr != nil {
				if !errors.Is(writeErr, sync.ErrTargetConflict) {
					return ctrl.Result{}, fmt.Errorf("kvsynk8s: write secret %s: %w", targetKey, writeErr)
				}
				// An unmanaged Secret occupies the target name. With the
				// filtered cache (ManagedSecretCacheOptions) this is the
				// normal detection path for that case — the cached Get above
				// cannot see unmanaged Secrets — and it also covers the
				// TOCTOU race where another actor created the Secret between
				// that Get and the write just above. Either way the CR's
				// status reflects TargetConflict rather than going stale for
				// this cycle.
				status = kvsynk8sv1alpha1.SecretSyncStatus{
					State:  kvsynk8sv1alpha1.SecretSyncStateFailing,
					Reason: sync.ReasonTargetConflict,
					Message: fmt.Sprintf(
						"secret %s/%s already exists and is not managed by kvsynk8s", ss.Namespace, targetName,
					),
					ObservedGeneration: ss.Generation,
					LastSyncTime:       ss.Status.LastSyncTime,
					SyncedVersion:      ss.Status.SyncedVersion,
				}
			}
		}
	}

	ss.Status = status
	if err := r.Status().Update(ctx, &ss); err != nil {
		return ctrl.Result{}, fmt.Errorf("kvsynk8s: update status for %s: %w", req.NamespacedName, err)
	}
	r.recordSyncOutcome(&ss, status, targetName)

	log.V(1).Info("reconciled SecretSync",
		"vault", ss.Spec.Vault.Name, "secret", ss.Spec.Vault.Secret, "state", status.State)

	if status.State == kvsynk8sv1alpha1.SecretSyncStateFailing && status.Reason == sync.ReasonTransientError {
		// Constitution II / FR-008: transient failures (network, throttling,
		// auth token expiry) are retried with exponential backoff via
		// controller-runtime's rate-limited workqueue, not at the same fixed
		// cadence used for a healthy periodic reconcile.
		return ctrl.Result{}, fmt.Errorf("kvsynk8s: transient error syncing %s, retrying with backoff", req.NamespacedName)
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
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ss, finalizerName) {
		return ctrl.Result{}, nil
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
				return ctrl.Result{}, fmt.Errorf("kvsynk8s: delete managed secret %s: %w", targetKey, err)
			}
		}
		// Else: a Secret exists at this name but this SecretSync never owned
		// it (conflict loser, or refused pre-existing unmanaged Secret) —
		// leave it untouched.
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, fmt.Errorf("kvsynk8s: get secret %s: %w", targetKey, err)
	}

	controllerutil.RemoveFinalizer(ss, finalizerName)
	if err := r.Update(ctx, ss); err != nil {
		return ctrl.Result{}, fmt.Errorf("kvsynk8s: remove finalizer from %s/%s: %w", ss.Namespace, ss.Name, err)
	}

	return ctrl.Result{}, nil
}

// ownedBy reports whether secret carries a controller OwnerReference back to
// ss, i.e. whether ss is the SecretSync that actually created/claimed it —
// as opposed to merely sharing its target name (FR-012).
func ownedBy(secret *corev1.Secret, ss *kvsynk8sv1alpha1.SecretSync) bool {
	for _, ref := range secret.OwnerReferences {
		if ref.UID == ss.UID && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
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
