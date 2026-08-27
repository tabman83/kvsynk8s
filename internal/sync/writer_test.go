// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// TestCreateOrUpdate_LabeledSecretOwnedByOtherController_TargetConflict
// covers the ownership wedge: a Secret that DOES carry the managed-by label
// (so the label check passes) but whose controller ownerReference points at a
// DIFFERENT SecretSync. controllerutil.SetControllerReference refuses that
// with AlreadyOwnedError; wrapped generically it would make the reconciler
// retry forever as an unclassified hard error. CreateOrUpdate must instead
// map it to ErrTargetConflict so the CR lands in the normal
// TargetConflict/Failing state (FR-012), and must leave the conflicting
// Secret untouched.
func TestCreateOrUpdate_LabeledSecretOwnedByOtherController_TargetConflict(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")
	otherOwner := newOwner("other-sync", "default", "my-vault", "other-secret")

	isController := true
	blockDeletion := true
	conflicting := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      owner.Name,
			Namespace: owner.Namespace,
			Labels: map[string]string{
				LabelManagedBy: LabelManagedByValue,
			},
			Annotations: map[string]string{
				AnnotationVault:   otherOwner.Spec.Vault.Name,
				AnnotationSecret:  otherOwner.Spec.Vault.Secret,
				AnnotationVersion: "v1",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         "kvsynk8s.io/v1alpha1",
				Kind:               secretSyncKind,
				Name:               otherOwner.Name,
				UID:                types.UID("some-other-uid"),
				Controller:         &isController,
				BlockOwnerDeletion: &blockDeletion,
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"other-secret": []byte("owned-by-someone-else")},
	}

	cli := fake.NewClientBuilder().WithScheme(redactionScheme(t)).WithObjects(conflicting).Build()
	w := &SecretWriter{Client: cli}

	err := w.CreateOrUpdate(context.Background(), owner, owner.Namespace, owner.Name, "my-app-password", "new-value", "v2")
	if err == nil {
		t.Fatal("CreateOrUpdate() error = nil, want ErrTargetConflict")
	}
	if !errors.Is(err, ErrTargetConflict) {
		t.Fatalf("CreateOrUpdate() error = %v, want wrapped ErrTargetConflict", err)
	}

	// The conflicting Secret must not have been updated in the cluster.
	var got corev1.Secret
	if getErr := cli.Get(context.Background(), client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, &got); getErr != nil {
		t.Fatalf("get conflicting secret: %v", getErr)
	}
	if string(got.Data["other-secret"]) != "owned-by-someone-else" {
		t.Fatalf("conflicting Secret's data was modified: %v", got.Data)
	}
	if got.Annotations[AnnotationVersion] != "v1" {
		t.Fatalf("conflicting Secret's version annotation was modified: %q", got.Annotations[AnnotationVersion])
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].Name != otherOwner.Name {
		t.Fatalf("conflicting Secret's ownerReferences were modified: %+v", got.OwnerReferences)
	}
}

// staleCacheClient builds a fake client whose FIRST Get for any Secret
// returns NotFound while every later Get (and all other verbs) passes
// through to the real fake store. This simulates the informer-cache race
// CreateOrUpdate can hit: the cached pre-write Get reports NotFound for a
// Secret the API server already has, Create then fails with AlreadyExists,
// and the recovery re-read (the second Get) sees the truth.
func staleCacheClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	firstGet := true
	return fake.NewClientBuilder().
		WithScheme(redactionScheme(t)).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if firstGet {
					firstGet = false
					return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, key.Name)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
}

// TestCreateOrUpdate_AlreadyExistsOnOwnSecret_FallsThroughToUpdate covers the
// informer race on the operator's OWN Secret: the cached Get misses it,
// Create returns AlreadyExists, and the uncached re-read shows a Secret that
// carries both the managed-by label and a controller ownerReference to this
// very owner. That must NOT be classified as ErrTargetConflict (which used to
// flap the CR to a false Failing/TargetConflict with a spurious SyncFailed
// event); CreateOrUpdate must fall through to the update path and converge.
func TestCreateOrUpdate_AlreadyExistsOnOwnSecret_FallsThroughToUpdate(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")

	own := existingSyncedSecret(owner, "v1", "old-value")
	cli := staleCacheClient(t, own)
	w := &SecretWriter{Client: cli, Reader: cli}

	err := w.CreateOrUpdate(context.Background(), owner, owner.Namespace, owner.Name, "my-app-password", "new-value", "v2")
	if err != nil {
		t.Fatalf("CreateOrUpdate() error = %v, want nil (own just-created Secret is not a conflict)", err)
	}

	var got corev1.Secret
	if getErr := cli.Get(context.Background(), client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, &got); getErr != nil {
		t.Fatalf("get secret after recovery: %v", getErr)
	}
	if string(got.Data["my-app-password"]) != "new-value" {
		t.Fatalf("secret data = %q, want the new value written through the update path", got.Data["my-app-password"])
	}
	if got.Annotations[AnnotationVersion] != "v2" {
		t.Fatalf("version annotation = %q, want %q", got.Annotations[AnnotationVersion], "v2")
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].UID != owner.UID {
		t.Fatalf("ownerReferences = %+v, want exactly one controller reference to the owner", got.OwnerReferences)
	}
}

// TestCreateOrUpdate_AlreadyExistsOnForeignSecret_TargetConflict pins the
// other half of the recovery: when the Secret that caused AlreadyExists is
// NOT verifiably this owner's — unmanaged, or labeled but controller-owned by
// a different SecretSync — the classification stays ErrTargetConflict and the
// occupant is left untouched (FR-012).
func TestCreateOrUpdate_AlreadyExistsOnForeignSecret_TargetConflict(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")
	otherOwner := newOwner("other-sync", "default", "my-vault", "my-app-password")

	unmanaged := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      owner.Name,
			Namespace: owner.Namespace,
			Labels:    map[string]string{"created-by": "some-other-tool"},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"unrelated-key": []byte("unrelated-value")},
	}

	otherOwned := existingSyncedSecret(otherOwner, "v1", "owned-by-someone-else")

	tests := []struct {
		name     string
		occupant *corev1.Secret
	}{
		{"unmanaged occupant", unmanaged},
		{"labeled occupant owned by another SecretSync", otherOwned},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := tt.occupant.DeepCopy()
			cli := staleCacheClient(t, tt.occupant)
			w := &SecretWriter{Client: cli, Reader: cli}

			err := w.CreateOrUpdate(context.Background(), owner, owner.Namespace, tt.occupant.Name, "my-app-password", "new-value", "v2")
			if !errors.Is(err, ErrTargetConflict) {
				t.Fatalf("CreateOrUpdate() error = %v, want wrapped ErrTargetConflict", err)
			}

			var got corev1.Secret
			if getErr := cli.Get(context.Background(), client.ObjectKey{Namespace: owner.Namespace, Name: tt.occupant.Name}, &got); getErr != nil {
				t.Fatalf("get occupant secret: %v", getErr)
			}
			for key, want := range before.Data {
				if string(got.Data[key]) != string(want) {
					t.Fatalf("occupant data[%q] = %q, want untouched %q", key, got.Data[key], want)
				}
			}
			if len(got.OwnerReferences) != len(before.OwnerReferences) {
				t.Fatalf("occupant ownerReferences were modified: %+v", got.OwnerReferences)
			}
		})
	}
}

// TestCreateOrUpdate_OwnSecretWithLabelStripped_RepairedNotConflicted covers
// the inverse of the two conflict cases above: a Secret this very owner
// created — its controller ownerReference still points at it by UID — whose
// managed-by label was removed in-cluster (a hand edit, or a GitOps/backup
// tool rewriting metadata).
//
// Requiring the label as well as the ownerReference used to wedge that Secret
// permanently: the label-filtered cache hides it, so the reconciler sees
// NotFound, Create comes back AlreadyExists, and the writer answered
// ErrTargetConflict about the CR's own Secret — on every reconcile, forever,
// so rotations stopped reaching it until someone restored the label by hand.
// Ownership by UID is proof enough to write; the write restores the label.
//
// Both routes to the occupant are exercised: the direct Get (an unfiltered
// client sees it) and the AlreadyExists recovery (what the filtered cache
// actually produces in production).
func TestCreateOrUpdate_OwnSecretWithLabelStripped_RepairedNotConflicted(t *testing.T) {
	tests := []struct {
		name   string
		client func(t *testing.T, objs ...client.Object) client.WithWatch
	}{
		{
			name: "found by the pre-write Get",
			client: func(t *testing.T, objs ...client.Object) client.WithWatch {
				t.Helper()
				return fake.NewClientBuilder().WithScheme(redactionScheme(t)).WithObjects(objs...).Build()
			},
		},
		{
			name:   "invisible to the cache, surfaced by AlreadyExists",
			client: staleCacheClient,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner := newOwner("my-sync", "default", "my-vault", "my-app-password")

			stripped := existingSyncedSecret(owner, "v1", "old-value")
			delete(stripped.Labels, LabelManagedBy)

			cli := tt.client(t, stripped)
			w := &SecretWriter{Client: cli, Reader: cli}

			err := w.CreateOrUpdate(context.Background(), owner, owner.Namespace, owner.Name, "my-app-password", "new-value", "v2")
			if err != nil {
				t.Fatalf("CreateOrUpdate() error = %v, want nil (the owner's own Secret must be repaired, not conflicted)", err)
			}

			var got corev1.Secret
			if getErr := cli.Get(context.Background(), client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, &got); getErr != nil {
				t.Fatalf("get repaired secret: %v", getErr)
			}
			if got.Labels[LabelManagedBy] != LabelManagedByValue {
				t.Fatalf("labels = %v, want the managed-by label restored by the repairing write", got.Labels)
			}
			if string(got.Data["my-app-password"]) != "new-value" {
				t.Fatalf("data was not updated: %v", got.Data)
			}
			if got.Annotations[AnnotationVersion] != "v2" {
				t.Fatalf("version annotation = %q, want %q", got.Annotations[AnnotationVersion], "v2")
			}
			if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].UID != owner.UID {
				t.Fatalf("ownerReferences = %+v, want exactly one controller reference to the owner", got.OwnerReferences)
			}
		})
	}
}

func TestManagedSecretDataMatches(t *testing.T) {
	tests := []struct {
		name    string
		data    map[string][]byte
		dataKey string
		value   string
		want    bool
	}{
		{"exact match", map[string][]byte{"k": []byte("v")}, "k", "v", true},
		{"nil data", nil, "k", "v", false},
		{"wrong value", map[string][]byte{"k": []byte("drifted")}, "k", "v", false},
		{"wrong key", map[string][]byte{"old": []byte("v")}, "k", "v", false},
		{"extra key", map[string][]byte{"k": []byte("v"), "extra": []byte("x")}, "k", "v", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := &corev1.Secret{Data: tt.data}
			if got := managedSecretDataMatches(secret, tt.dataKey, tt.value); got != tt.want {
				t.Fatalf("managedSecretDataMatches() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPopulateManagedSecret_ReplacesDataWholesale locks in that a write
// removes any keys that are not the resolved dataKey: this is what makes a
// spec.target.dataKey change drop the old key, and what strips extra keys
// added by an in-cluster edit (US3 drift repair, FR-007).
func TestPopulateManagedSecret_ReplacesDataWholesale(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")
	secret := &corev1.Secret{
		Data: map[string][]byte{
			"stale-key": []byte("stale-value"),
			"extra-key": []byte("extra-value"),
		},
	}

	populateManagedSecret(secret, owner, "fresh-key", "fresh-value", "v9")

	if len(secret.Data) != 1 {
		t.Fatalf("len(secret.Data) = %d, want exactly 1 key; got %v", len(secret.Data), secret.Data)
	}
	if string(secret.Data["fresh-key"]) != "fresh-value" {
		t.Fatalf("secret.Data[%q] = %q, want %q", "fresh-key", secret.Data["fresh-key"], "fresh-value")
	}
}
