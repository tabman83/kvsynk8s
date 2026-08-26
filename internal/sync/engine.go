// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kvsynk8sv1alpha1 "github.com/tabman83/kvsynk8s/api/v1alpha1"
	"github.com/tabman83/kvsynk8s/internal/azure"
)

// Reason values this engine produces on SecretSyncStatus.Reason when
// classifying a SecretReader failure, per the state table in data-model.md.
// ReasonTargetConflict lives in writer.go (it originates from
// ErrTargetConflict, a writer concept); the reader-driven reasons live here,
// next to the code that decides between them.
const (
	// ReasonSecretNotFound: the vault secret does not exist and this
	// SecretSync never had a prior synced version to fall back to.
	ReasonSecretNotFound = "SecretNotFound"
	// ReasonAccessDenied: the operator's identity cannot read the secret.
	ReasonAccessDenied = "AccessDenied"
	// ReasonSourceDeleted: the vault secret used to exist (this SecretSync
	// had already synced a version) and the vault now reports it missing.
	// FR-013: the managed Secret keeps its last synced value untouched.
	ReasonSourceDeleted = "SourceDeleted"
	// ReasonSourceDisabled: the vault secret exists but is administratively
	// disabled. FR-013: the managed Secret keeps its last synced value.
	ReasonSourceDisabled = "SourceDisabled"
	// ReasonTransientError: a retryable failure (network, throttling, or any
	// unclassified upstream error); the controller requeues with backoff.
	ReasonTransientError = "TransientError"
)

// secretSyncKind is the Kind of the owning SecretSync object, used to build
// the controller OwnerReference on every managed Secret.
const secretSyncKind = "SecretSync"

// Engine turns a SecretSync declaration into the Kubernetes Secret it
// describes. It performs no Kubernetes API I/O itself: the reconciler
// (internal/controller, T013) creates/updates the returned *corev1.Secret
// against the cluster, and uses SecretWriter (writer.go) to do so. Sync does
// call SecretReader.GetLatest, since the latest Key Vault version is
// required to decide in-sync vs. not (FR-005, "latest wins" — a specific
// version named by a queue event is never requested).
//
// Building the actual Secret object shape (labels, annotations, data) is
// delegated to populateManagedSecret, the same unexported helper
// SecretWriter.CreateOrUpdate uses, so the engine's idea of a "managed
// Secret" and the writer's real client.Create/Update path can never drift
// apart (constitution I: one shape, one place it is assembled).
type Engine struct {
	// Reader fetches the current value+version of the declared vault
	// secret. Required.
	Reader azure.SecretReader
}

// Sync computes the desired SecretSyncStatus and Kubernetes Secret for
// owner, given the current state of its target Secret (existing is nil if
// it does not exist yet). Target name/dataKey defaults from data-model.md
// are resolved here because the CRD schema cannot reference metadata.name.
//
// err is non-nil only for a programmer error (e.g. Reader is nil); every
// expected failure mode — vault secret not found/disabled/access-denied,
// target owned by something else, transient upstream error — is reported
// via the returned status instead, per data-model.md's state machine. On
// any failure the returned secret is exactly the input existing (nil stays
// nil): nothing is fabricated, and a Secret that was previously synced is
// never modified or deleted just because the latest sync attempt failed
// (FR-013, keep-last-known-good).
func (e *Engine) Sync(
	ctx context.Context,
	owner *kvsynk8sv1alpha1.SecretSync,
	existing *corev1.Secret,
) (kvsynk8sv1alpha1.SecretSyncStatus, *corev1.Secret, error) {
	if e.Reader == nil {
		return kvsynk8sv1alpha1.SecretSyncStatus{}, existing, errors.New("kvsynk8s: sync engine has no SecretReader configured")
	}

	dataKey := owner.Spec.Target.DataKey
	if dataKey == "" {
		dataKey = owner.Spec.Vault.Secret
	}
	targetName := owner.Spec.Target.SecretName
	if targetName == "" {
		targetName = owner.Name
	}

	status := kvsynk8sv1alpha1.SecretSyncStatus{
		ObservedGeneration: owner.Generation,
		// Carry the previous last-known-good sync time forward; it only
		// advances below, on an actual successful sync/verify, never on a
		// failure.
		LastSyncTime: owner.Status.LastSyncTime,
	}

	// FR-012, first-writer-wins: never touch a pre-existing Secret this
	// SecretSync did not create, no matter what the vault currently holds.
	// Checked before ever calling the vault so a conflicting declaration
	// never even attempts a read. Two distinct conflict shapes:
	//   - no managed-by label: an unmanaged Secret created by someone else;
	//   - labeled, but controller-owned by a DIFFERENT SecretSync. The label
	//     only proves "some kvsynk8s SecretSync wrote this"; WHICH one is
	//     recorded solely in the controller ownerReference UID — the same
	//     rule the controller's ownedBy and the writer's AlreadyOwnedError
	//     mapping already apply. Without this second check, a conflict loser
	//     whose data/version happened to match the winner's would take the
	//     idempotent InSync skip below over a Secret it does not own, and the
	//     reconciler's write gate would then never reach the writer's own
	//     ownership check.
	labeled := existing != nil && existing.Labels[LabelManagedBy] == LabelManagedByValue
	if existing != nil && !labeled {
		status.State = kvsynk8sv1alpha1.SecretSyncStateFailing
		status.Reason = ReasonTargetConflict
		status.Message = fmt.Sprintf(
			"secret %s/%s already exists and is not managed by kvsynk8s",
			owner.Namespace, targetName,
		)
		return status, existing, nil
	}
	if labeled {
		if ref := metav1.GetControllerOf(existing); ref != nil && ref.UID != owner.UID {
			status.State = kvsynk8sv1alpha1.SecretSyncStateFailing
			status.Reason = ReasonTargetConflict
			status.Message = fmt.Sprintf(
				"secret %s/%s is already owned by another SecretSync",
				owner.Namespace, targetName,
			)
			return status, existing, nil
		}
	}

	// managed: this Secret is verifiably a prior write of THIS SecretSync —
	// labeled AND controller-owned by owner. A labeled Secret with no
	// controller owner at all is not a conflict (the writer adopts it on the
	// write path below, exactly as it always has), but it is not `managed`
	// either: the idempotent skip must never accept a Secret this SecretSync
	// cannot prove it owns.
	managed := labeled && ControllerOwnedBy(existing, owner)

	value, version, err := e.Reader.GetLatest(ctx, owner.Spec.Vault.Name, owner.Spec.Vault.Secret)
	if err != nil {
		status.State = kvsynk8sv1alpha1.SecretSyncStateFailing
		status.Reason, status.Message = classifyReaderError(err, owner, managed)
		return status, existing, nil
	}

	// Idempotency (FR-005): skip the write only when the target already
	// reflects this exact version AND its data is exactly the desired shape.
	// The version annotation alone is not enough: a spec edit (e.g. a changed
	// target.dataKey) or an in-cluster edit of the managed Secret's data
	// leaves the annotation intact while the data no longer matches, and both
	// must be repaired on the next reconcile (US3 drift repair, FR-007). The
	// value was already fetched above, so this comparison costs no extra
	// vault call; the comparison itself lives in writer.go
	// (managedSecretDataMatches), next to populateManagedSecret, so the
	// definition of the desired data shape stays in one place.
	if managed && existing.Annotations[AnnotationVersion] == version &&
		managedSecretDataMatches(existing, dataKey, value) {
		status.State = kvsynk8sv1alpha1.SecretSyncStateInSync
		status.SyncedVersion = version
		status.LastSyncTime = metav1.Now()
		return status, existing, nil
	}

	secret := existing
	if secret == nil {
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      targetName,
				Namespace: owner.Namespace,
			},
		}
	}
	populateManagedSecret(secret, owner, dataKey, value, version)
	secret.OwnerReferences = []metav1.OwnerReference{controllerOwnerReference(owner)}

	status.State = kvsynk8sv1alpha1.SecretSyncStateInSync
	status.SyncedVersion = version
	status.LastSyncTime = metav1.Now()
	return status, secret, nil
}

// controllerOwnerReference builds the single controller OwnerReference every
// managed Secret carries, pointing at owner. Built by hand rather than via
// controllerutil.SetControllerReference because Sync does no API I/O and has
// no *runtime.Scheme available; the SecretSync GVK is fixed and known at
// compile time. The reconciler's real write path (writer.go) re-derives and
// re-applies the same reference through the cluster's scheme, so this is
// belt-and-braces, not the only place it is set.
func controllerOwnerReference(owner *kvsynk8sv1alpha1.SecretSync) metav1.OwnerReference {
	isController := true
	blockDeletion := true
	return metav1.OwnerReference{
		APIVersion:         kvsynk8sv1alpha1.GroupVersion.String(),
		Kind:               secretSyncKind,
		Name:               owner.Name,
		UID:                owner.UID,
		Controller:         &isController,
		BlockOwnerDeletion: &blockDeletion,
	}
}

// ControllerOwnedBy reports whether secret's controller OwnerReference points
// at owner by UID — i.e. whether owner is the SecretSync that actually
// created/claimed secret, as opposed to merely sharing its target name or its
// managed-by label (FR-012). This is the single ownership predicate for the
// whole codebase: the engine's conflict/idempotency checks above, the writer's
// AlreadyExists recovery, and the controller's deletion/GC paths (ownedBy) all
// delegate to the same UID comparison so they can never disagree.
func ControllerOwnedBy(secret *corev1.Secret, owner *kvsynk8sv1alpha1.SecretSync) bool {
	ref := metav1.GetControllerOf(secret)
	return ref != nil && ref.UID == owner.UID
}

// classifyReaderError maps a SecretReader.GetLatest error to a
// status.reason/message pair per data-model.md's state table. managed
// reports whether the target Secret already carried a synced version
// annotation from a prior successful sync — this is what distinguishes
// "never synced" (SecretNotFound) from "used to be there, now gone"
// (SourceDeleted) for the same underlying not-found response (FR-013).
//
// The returned message references only the vault name and secret name
// (identifiers) plus fixed, static text — never anything derived from err's
// own string, which could in principle echo upstream response content this
// package has not audited (constitution I: no value, ever, in status).
func classifyReaderError(err error, owner *kvsynk8sv1alpha1.SecretSync, managed bool) (reason, message string) {
	vault, secretName := owner.Spec.Vault.Name, owner.Spec.Vault.Secret

	switch {
	case errors.Is(err, azure.ErrSecretNotFound):
		if managed {
			return ReasonSourceDeleted, fmt.Sprintf(
				"secret %q in vault %q no longer exists; keeping last synced value", secretName, vault)
		}
		return ReasonSecretNotFound, fmt.Sprintf("secret %q not found in vault %q", secretName, vault)
	case errors.Is(err, azure.ErrSecretDisabled):
		if managed {
			return ReasonSourceDisabled, fmt.Sprintf(
				"secret %q in vault %q is disabled; keeping last synced value", secretName, vault)
		}
		return ReasonSourceDisabled, fmt.Sprintf("secret %q in vault %q is disabled", secretName, vault)
	case errors.Is(err, azure.ErrAccessDenied):
		return ReasonAccessDenied, fmt.Sprintf("access denied reading secret %q from vault %q", secretName, vault)
	case errors.Is(err, azure.ErrTransient):
		return ReasonTransientError, fmt.Sprintf("transient error reading secret %q from vault %q", secretName, vault)
	default:
		return ReasonTransientError, fmt.Sprintf("unclassified error reading secret %q from vault %q", secretName, vault)
	}
}
