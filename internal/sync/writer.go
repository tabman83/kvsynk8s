// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kvsynk8sv1alpha1 "github.com/tabman83/kvsynk8s/api/v1alpha1"
)

// Label and annotation keys applied to every managed Kubernetes Secret, per
// the "Managed Kubernetes Secret" shape in data-model.md. These live here,
// not in engine.go, because writer.go is the single place that constructs
// the Secret object (constitution principle I) and every other package
// (engine.go, the controller, tests) reads the same constants rather than
// re-declaring the string literals.
const (
	// LabelManagedBy is the label key that marks a Secret as owned by
	// kvsynk8s. Its presence (with LabelManagedByValue) is what the writer
	// checks before ever overwriting a pre-existing Secret (FR-012).
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// LabelManagedByValue is the required value of LabelManagedBy.
	LabelManagedByValue = "kvsynk8s"

	// AnnotationVault records the source Key Vault name (an identifier, not
	// a value; safe per constitution principle I).
	AnnotationVault = "kvsynk8s.io/vault"
	// AnnotationSecret records the source secret name inside the vault.
	AnnotationSecret = "kvsynk8s.io/secret"
	// AnnotationVersion records the Key Vault secret version currently
	// reflected by this Secret's data, enabling no-op detection (FR-005)
	// without ever reading the value back.
	AnnotationVersion = "kvsynk8s.io/version"

	// ReasonTargetConflict is the SecretSync status.reason to use when
	// ErrTargetConflict is returned (data-model.md).
	ReasonTargetConflict = "TargetConflict"

	// ReasonTargetImmutable is the SecretSync status.reason to use when
	// ErrTargetImmutable is returned: the target Secret carries
	// immutable: true, so its data can no longer be rewritten.
	ReasonTargetImmutable = "TargetImmutable"

	// ReasonSecretWriteFailed is the SecretSync status.reason for a
	// Kubernetes write this operator could not complete for any other reason
	// — an admission policy, a ResourceQuota, RBAC drift. It used to share
	// TransientError with vault-side failures, which meant the two most
	// differently-fixed problems in the whole operator were indistinguishable
	// from kubectl, from the Event, and from any metric.
	ReasonSecretWriteFailed = "SecretWriteFailed"
)

// ErrTargetConflict is returned by SecretWriter when the target Secret
// already exists but was not created by kvsynk8s (it lacks LabelManagedBy).
// Per FR-012 ("first writer wins") the writer MUST NOT touch such a Secret;
// the caller maps this to SecretSync status {State: Failing, Reason:
// ReasonTargetConflict}.
var ErrTargetConflict = errors.New("kvsynk8s: target secret exists and is not managed by kvsynk8s")

// ErrTargetImmutable is returned when the managed target Secret has
// immutable: true set and the write would change its data. The Kubernetes API
// server refuses that update permanently: immutability cannot be unset on an
// existing Secret, so no amount of retrying clears it. Treating it as
// retryable — which is what happened before this existed — costs a Key Vault
// read per backoff attempt for a state only a human can fix, and reports the
// condition as transient when it is anything but. The caller maps this to
// {State: Failing, Reason: ReasonTargetImmutable}.
//
// The writer never clears or flips Immutable itself. Silently unfreezing a
// Secret a user deliberately froze is the same class of act FR-012 forbids,
// and a data-shaped one: workloads may be mounting it precisely because it
// cannot change.
var ErrTargetImmutable = errors.New("kvsynk8s: target secret is immutable and its data cannot be updated")

// SecretWriter is the ONLY code path in the whole codebase allowed to write
// a secret value into a Kubernetes object (constitution principle I). Every
// other package that needs a value synced into the cluster must go through
// CreateOrUpdate; nothing here ever logs, wraps in an error string, or
// otherwise surfaces the value parameter — it is placed exclusively into the
// returned/persisted Secret's Data field.
//
// Structured logging convention (T028, constitution I / FR-010): every log
// call in this file uses only identifier keys — "namespace", "name",
// "vault", "secret", "version" — read off owner/namespace/name/version.
// internal/sync/redaction_test.go's static check
// (TestValueCarryingSources_NeverLogValueIdentifiers) parses this file's AST
// and inspects every .Info(/.Error(/.WithValues( call's arguments for a
// reference to a value-carrying identifier (`value`, `existing`, `secret`).
// That catches a violation whether the call is on one line or wrapped across
// several — but only for those call shapes and identifiers; the runtime
// log-capture tests in the same file cover what actually gets emitted.
type SecretWriter struct {
	// Client is used to read the current state of the target Secret and to
	// create or update it. Required.
	Client client.Client

	// Reader, when set, is an UNCACHED reader (mgr.GetAPIReader()) used only
	// to re-check the target after Create fails with AlreadyExists. The
	// cached Client can race the informer: a queue-triggered reconcile right
	// after this operator's own Create may still see NotFound from the cache
	// while the API server already has the Secret, and without an uncached
	// re-read that race would be misclassified as ErrTargetConflict. Falls
	// back to Client when nil, so hand-built writers (tests) keep working.
	Reader client.Reader
}

// reader returns the uncached Reader when wired, or Client otherwise.
func (w *SecretWriter) reader() client.Reader {
	if w.Reader != nil {
		return w.Reader
	}
	return w.Client
}

// CreateOrUpdate ensures that an Opaque Secret named `name` in `namespace`
// carries `value` under `dataKey`, owned by `owner`. It sets:
//   - metadata.labels[LabelManagedBy] = LabelManagedByValue
//   - metadata.annotations[AnnotationVault/AnnotationSecret/AnnotationVersion]
//   - metadata.ownerReferences: a single controller reference to owner
//     (Controller: true, BlockOwnerDeletion: true)
//   - data[dataKey] = value (corev1.Secret.Data is []byte; the API server
//     base64-encodes it on the wire, so value is passed through as-is here
//     without any additional encoding)
//
// If a Secret already exists at namespace/name without LabelManagedBy set to
// LabelManagedByValue, CreateOrUpdate does not touch it at all and returns an
// error wrapping ErrTargetConflict (FR-012, first-writer-wins).
func (w *SecretWriter) CreateOrUpdate(
	ctx context.Context,
	owner *kvsynk8sv1alpha1.SecretSync,
	namespace, name, dataKey, value, version string,
) error {
	if w.Client == nil {
		return errors.New("kvsynk8s: secret writer has no client configured")
	}

	key := client.ObjectKey{Namespace: namespace, Name: name}
	existing := &corev1.Secret{}
	err := w.Client.Get(ctx, key, existing)

	switch {
	case apierrors.IsNotFound(err):
		return w.create(ctx, owner, namespace, name, dataKey, value, version)
	case err != nil:
		return fmt.Errorf("kvsynk8s: get secret %s/%s: %w", namespace, name, err)
	}

	if !writable(existing, owner) {
		return fmt.Errorf("kvsynk8s: secret %s/%s: %w", namespace, name, ErrTargetConflict)
	}

	return w.update(ctx, owner, existing, dataKey, value, version)
}

// writable reports whether the writer may write over the Secret currently at
// the target name, i.e. whether it is one kvsynk8s wrote rather than someone
// else's object (FR-012, first writer wins). Two independent proofs count, and
// either alone is enough:
//
//   - the managed-by label: some kvsynk8s SecretSync wrote it. Which one is
//     not settled here — a label with a controller ownerReference pointing at
//     a DIFFERENT SecretSync is still a conflict, caught by update's
//     AlreadyOwnedError mapping; a label with no controller owner at all is
//     the orphan left by `--cascade=orphan` or a backup restore, which the
//     writer adopts.
//   - a controller ownerReference whose UID is this owner's: this owner
//     provably created it, whatever its labels say now. Requiring the label
//     too would wedge a Secret whose label was stripped in-cluster into a
//     permanent TargetConflict about the CR's own Secret: the label-filtered
//     cache (controller.ManagedSecretCacheOptions) hides it, so the reconciler
//     sees NotFound, Create hits AlreadyExists, and every later reconcile
//     repeats that identically — rotations would stop reaching that Secret
//     forever, while every other kind of in-cluster drift on it is repaired
//     automatically (FR-007). populateManagedSecret re-stamps the label on the
//     write this unblocks, so the repair is a single reconcile.
func writable(secret *corev1.Secret, owner *kvsynk8sv1alpha1.SecretSync) bool {
	return secret.Labels[LabelManagedBy] == LabelManagedByValue || ControllerOwnedBy(secret, owner)
}

// update rewrites an existing managed Secret in place and answers the two
// ways that write can fail because the copy it was built from is behind the
// API server.
//
// The Secret handed to applyUpdate came from the CACHED Get in CreateOrUpdate.
// A 409 Conflict means its resourceVersion is stale; an Invalid can mean the
// cached copy had not yet observed someone setting immutable: true. Both are
// answered by re-reading through the uncached reader() — the same move
// recoverAlreadyExists makes for the NotFound flavour of this race. Wrapping
// in applyUpdate does not hide either one: apierrors.IsConflict/IsInvalid
// unwrap through fmt.Errorf's %w.
func (w *SecretWriter) update(
	ctx context.Context,
	owner *kvsynk8sv1alpha1.SecretSync,
	existing *corev1.Secret,
	dataKey, value, version string,
) error {
	err := w.applyUpdate(ctx, owner, existing, dataKey, value, version)
	switch {
	case err == nil:
		return nil
	case apierrors.IsConflict(err):
		return w.recoverConflict(ctx, owner, existing.Namespace, existing.Name, dataKey, value, version)
	case apierrors.IsInvalid(err):
		return w.classifyInvalid(ctx, existing.Namespace, existing.Name, dataKey, value, err)
	}
	return err
}

// applyUpdate rewrites an existing managed Secret in place (labels,
// annotations, owner reference, data) and persists it. Shared by
// CreateOrUpdate's normal found-and-labeled path and by the AlreadyExists
// recovery in create, so the two paths cannot drift.
func (w *SecretWriter) applyUpdate(
	ctx context.Context,
	owner *kvsynk8sv1alpha1.SecretSync,
	existing *corev1.Secret,
	dataKey, value, version string,
) error {
	namespace, name := existing.Namespace, existing.Name

	// An immutable Secret still accepts metadata-only updates — Kubernetes
	// freezes .data, not the object — so a rotation that only advances the
	// version annotation is legal and must go through. Only a write that
	// would actually change the data is refused, which is exactly what
	// immutableBlocksWrite asks. Checked here, before populateManagedSecret
	// overwrites existing.Data, because afterwards the pre-write data is gone
	// and the comparison would always say "nothing changed".
	if immutableBlocksWrite(existing, dataKey, value) {
		return fmt.Errorf("kvsynk8s: secret %s/%s: %w", namespace, name, ErrTargetImmutable)
	}

	populateManagedSecret(existing, owner, dataKey, value, version)
	if err := controllerutil.SetControllerReference(owner, existing, w.Client.Scheme()); err != nil {
		var alreadyOwned *controllerutil.AlreadyOwnedError
		if errors.As(err, &alreadyOwned) {
			// The Secret carries the managed-by label but its controller
			// ownerReference points at a DIFFERENT owner (typically another
			// SecretSync). Classify it as a target conflict (FR-012) rather
			// than returning an unclassified error: the latter would make the
			// reconciler retry forever with backoff, while ErrTargetConflict
			// lands the CR in the normal TargetConflict/Failing state. The
			// early return also means the conflicting Secret is never updated.
			return fmt.Errorf("kvsynk8s: secret %s/%s: %w", namespace, name, ErrTargetConflict)
		}
		return fmt.Errorf("kvsynk8s: set owner reference on secret %s/%s: %w", namespace, name, err)
	}
	if err := w.Client.Update(ctx, existing); err != nil {
		return fmt.Errorf("kvsynk8s: update secret %s/%s: %w", namespace, name, err)
	}
	logf.FromContext(ctx).V(1).Info("updated managed secret", "namespace", namespace, "name", name, "vault", owner.Spec.Vault.Name, "secret", owner.Spec.Vault.Secret, "version", version)
	return nil
}

// create builds and persists a brand new managed Secret. Only reached when
// the earlier Get in CreateOrUpdate reported nothing at this namespace/name —
// which, with the label-filtered cache, may still be stale or incomplete: an
// AlreadyExists from the API server is resolved by recoverAlreadyExists.
func (w *SecretWriter) create(
	ctx context.Context,
	owner *kvsynk8sv1alpha1.SecretSync,
	namespace, name, dataKey, value, version string,
) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	populateManagedSecret(secret, owner, dataKey, value, version)

	if err := controllerutil.SetControllerReference(owner, secret, w.Client.Scheme()); err != nil {
		return fmt.Errorf("kvsynk8s: set owner reference on secret %s/%s: %w", namespace, name, err)
	}
	if err := w.Client.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return w.recoverAlreadyExists(ctx, owner, namespace, name, dataKey, value, version)
		}
		return fmt.Errorf("kvsynk8s: create secret %s/%s: %w", namespace, name, err)
	}
	logf.FromContext(ctx).V(1).Info("created managed secret", "namespace", namespace, "name", name, "vault", owner.Spec.Vault.Name, "secret", owner.Spec.Vault.Secret, "version", version)
	return nil
}

// recoverAlreadyExists decides what a Create that failed with AlreadyExists
// actually means. Two very different situations produce it:
//
//   - a genuinely foreign Secret at the target name — unmanaged, or managed
//     by a different SecretSync — that the label-filtered cache could not
//     see. First-writer-wins (FR-012) classifies that as ErrTargetConflict,
//     here for the unmanaged one and in update (AlreadyOwnedError) for the one
//     another SecretSync controls.
//   - this operator's OWN managed Secret, created moments ago, that the
//     cached Get in CreateOrUpdate has not observed yet (a queue-triggered
//     reconcile racing the informer). That is not a conflict: falling
//     through to the update path converges immediately instead of flapping
//     the CR to a false Failing/TargetConflict with a spurious SyncFailed
//     event until a later reconcile self-heals it.
//
// The re-read goes through reader() — the uncached APIReader when wired — so
// it cannot repeat the very cache staleness being recovered from. What counts
// as ours is decided by writable, the same predicate CreateOrUpdate's
// found-the-Secret path uses, so the two paths cannot disagree about the same
// occupant; anything writable rejects stays a conflict.
func (w *SecretWriter) recoverAlreadyExists(
	ctx context.Context,
	owner *kvsynk8sv1alpha1.SecretSync,
	namespace, name, dataKey, value, version string,
) error {
	current := &corev1.Secret{}
	if getErr := w.reader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, current); getErr != nil {
		// Whatever occupies the name cannot be identified: return the error
		// for backoff retry rather than misclassifying. A NotFound here means
		// the occupant is already gone again, and the retry's Create will
		// simply succeed.
		return fmt.Errorf("kvsynk8s: re-read secret %s/%s after AlreadyExists: %w", namespace, name, getErr)
	}
	if writable(current, owner) {
		return w.update(ctx, owner, current, dataKey, value, version)
	}
	return fmt.Errorf("kvsynk8s: create secret %s/%s: %w", namespace, name, ErrTargetConflict)
}

// freshCopy re-reads the target straight through reader() — the uncached
// APIReader when wired — so the caller is looking at what the API server
// actually holds, not at the cached copy whose staleness caused the failure
// being recovered from.
func (w *SecretWriter) freshCopy(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	current := &corev1.Secret{}
	if err := w.reader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, current); err != nil {
		return nil, err
	}
	return current, nil
}

// recoverConflict answers a 409 on the Update. The Secret this write was built
// from came from the cached Get in CreateOrUpdate, and a duplicate
// at-least-once queue message arriving inside the informer-lag window after
// this operator's OWN write hands the reconciler a stale resourceVersion. That
// is not a failure worth reporting: the value in the cluster is already
// correct, or about to be. Re-read uncached, re-check ownership with the same
// writable predicate every other path uses, and apply the write once more —
// the same shape recoverAlreadyExists uses for the NotFound flavour of this
// race, and for the same stated reason: not flapping the CR to a false Failing
// with a spurious SyncFailed event that a later reconcile silently heals.
//
// Deliberately ONE retry, through applyUpdate rather than update, so this
// cannot recurse. A second conflict means something else is actively rewriting
// the Secret, which is a real condition the workqueue should back off on.
func (w *SecretWriter) recoverConflict(
	ctx context.Context,
	owner *kvsynk8sv1alpha1.SecretSync,
	namespace, name, dataKey, value, version string,
) error {
	current, err := w.freshCopy(ctx, namespace, name)
	if err != nil {
		return fmt.Errorf("kvsynk8s: re-read secret %s/%s after Conflict: %w", namespace, name, err)
	}
	if !writable(current, owner) {
		return fmt.Errorf("kvsynk8s: secret %s/%s: %w", namespace, name, ErrTargetConflict)
	}
	return w.applyUpdate(ctx, owner, current, dataKey, value, version)
}

// classifyInvalid decides whether an Invalid rejection of the Update was the
// API server refusing to change an immutable Secret's data. It never writes
// and never retries.
//
// It answers that by re-reading the object and asking the same deterministic
// question applyUpdate's pre-check asks — NOT by parsing invalidErr's
// StatusError details. Matching on a field path and cause type would couple
// this operator to wording owned by k8s.io/kubernetes, which is not even a
// dependency of this module, and would start misclassifying silently if that
// wording ever moved. The object itself is the ground truth and costs one Get
// on a path that has already failed.
//
// Anything else Invalid can mean — an admission webhook, a validating policy —
// falls through with invalidErr unchanged, so the caller keeps classifying it
// as a write failure exactly as before. A failed re-read does the same: never
// upgrade a guess into a terminal state.
func (w *SecretWriter) classifyInvalid(
	ctx context.Context,
	namespace, name, dataKey, value string,
	invalidErr error,
) error {
	current, err := w.freshCopy(ctx, namespace, name)
	if err != nil {
		return invalidErr
	}
	if immutableBlocksWrite(current, dataKey, value) {
		return fmt.Errorf("kvsynk8s: secret %s/%s: %w", namespace, name, ErrTargetImmutable)
	}
	return invalidErr
}

// populateManagedSecret sets the type, managed-by label, vault/secret/version
// annotations, and data[dataKey] on secret in place. It is the single
// function, in the single file allowed to do so, that places a secret value
// into a Kubernetes object field.
func populateManagedSecret(secret *corev1.Secret, owner *kvsynk8sv1alpha1.SecretSync, dataKey, value, version string) {
	secret.Type = corev1.SecretTypeOpaque

	if secret.Labels == nil {
		secret.Labels = make(map[string]string, 1)
	}
	secret.Labels[LabelManagedBy] = LabelManagedByValue

	if secret.Annotations == nil {
		secret.Annotations = make(map[string]string, 3)
	}
	secret.Annotations[AnnotationVault] = owner.Spec.Vault.Name
	secret.Annotations[AnnotationSecret] = owner.Spec.Vault.Secret
	secret.Annotations[AnnotationVersion] = version

	// The managed Secret carries exactly one data key: the resolved dataKey.
	// Data is replaced wholesale (rather than setting one key into whatever
	// map is already there) so a stale key left behind by an earlier
	// spec.target.dataKey, or an extra key added by an in-cluster edit, is
	// removed by the same write that stores the current value (US3 drift
	// repair, FR-007).
	secret.Data = map[string][]byte{dataKey: []byte(value)}
}

// managedSecretDataMatches reports whether secret's data is already exactly
// the shape populateManagedSecret would produce for (dataKey, value): one
// key, the right key, the right value. The engine uses it to decide whether
// an existing managed Secret still needs a write even when its version
// annotation is current (a changed spec.target.dataKey, or in-cluster drift
// on the data). It lives here, next to populateManagedSecret, so the
// definition of the desired data shape stays in the one file allowed to
// handle secret values (constitution I); nothing here logs or returns the
// value.
// immutableBlocksWrite reports whether the API server will refuse the write
// this writer is about to make because secret carries immutable: true.
// Immutability freezes .data only — object metadata stays editable — so a
// write is blocked precisely when the desired data differs from what is
// already there, which is what managedSecretDataMatches decides. Reusing it
// means this check and the engine's idempotency check can never disagree
// about whether a write changes anything.
func immutableBlocksWrite(secret *corev1.Secret, dataKey, value string) bool {
	return secret.Immutable != nil && *secret.Immutable &&
		!managedSecretDataMatches(secret, dataKey, value)
}

func managedSecretDataMatches(secret *corev1.Secret, dataKey, value string) bool {
	if len(secret.Data) != 1 {
		return false
	}
	got, ok := secret.Data[dataKey]
	return ok && string(got) == value
}
