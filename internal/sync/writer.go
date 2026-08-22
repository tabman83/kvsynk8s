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
)

// ErrTargetConflict is returned by SecretWriter when the target Secret
// already exists but was not created by kvsynk8s (it lacks LabelManagedBy).
// Per FR-012 ("first writer wins") the writer MUST NOT touch such a Secret;
// the caller maps this to SecretSync status {State: Failing, Reason:
// ReasonTargetConflict}.
var ErrTargetConflict = errors.New("kvsynk8s: target secret exists and is not managed by kvsynk8s")

// SecretWriter is the ONLY code path in the whole codebase allowed to write
// a secret value into a Kubernetes object (constitution principle I). Every
// other package that needs a value synced into the cluster must go through
// CreateOrUpdate; nothing here ever logs, wraps in an error string, or
// otherwise surfaces the value parameter — it is placed exclusively into the
// returned/persisted Secret's Data field.
type SecretWriter struct {
	// Client is used to read the current state of the target Secret and to
	// create or update it. Required.
	Client client.Client
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

	if existing.Labels[LabelManagedBy] != LabelManagedByValue {
		return fmt.Errorf("kvsynk8s: secret %s/%s: %w", namespace, name, ErrTargetConflict)
	}

	populateManagedSecret(existing, owner, dataKey, value, version)
	if err := controllerutil.SetControllerReference(owner, existing, w.Client.Scheme()); err != nil {
		return fmt.Errorf("kvsynk8s: set owner reference on secret %s/%s: %w", namespace, name, err)
	}
	if err := w.Client.Update(ctx, existing); err != nil {
		return fmt.Errorf("kvsynk8s: update secret %s/%s: %w", namespace, name, err)
	}
	return nil
}

// create builds and persists a brand new managed Secret. Only reached when
// the earlier Get in CreateOrUpdate confirmed nothing exists at this
// namespace/name yet, so there is no unmanaged resource to conflict with.
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
			// Another actor created a Secret at this name between the Get in
			// CreateOrUpdate and this Create (TOCTOU): treat it the same as
			// any other pre-existing unmanaged Secret rather than a generic
			// error, so callers can classify it as TargetConflict (FR-012)
			// instead of the status update being skipped entirely.
			return fmt.Errorf("kvsynk8s: create secret %s/%s: %w", namespace, name, ErrTargetConflict)
		}
		return fmt.Errorf("kvsynk8s: create secret %s/%s: %w", namespace, name, err)
	}
	return nil
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

	if secret.Data == nil {
		secret.Data = make(map[string][]byte, 1)
	}
	secret.Data[dataKey] = []byte(value)
}
