// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
				Kind:               "SecretSync",
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
