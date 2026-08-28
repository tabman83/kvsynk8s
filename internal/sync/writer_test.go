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
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kvsynk8sv1alpha1 "github.com/tabman83/kvsynk8s/api/v1alpha1"
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

// immutableSecret builds a managed Secret that looks like a prior successful
// sync for owner and then freezes it, the way a user who wants a value pinned
// (and the kubelet's watch dropped) would.
func immutableSecret(owner *kvsynk8sv1alpha1.SecretSync, version, value string) *corev1.Secret {
	secret := existingSyncedSecret(owner, version, value)
	frozen := true
	secret.Immutable = &frozen
	return secret
}

// TestCreateOrUpdate_ImmutableSecretValueChanged_TargetImmutable pins the
// up-front refusal: the API server rejects any change to a frozen Secret's
// data permanently — immutability cannot be unset on an existing Secret — so
// retrying costs a Key Vault read per backoff attempt for a state only a human
// can fix. The writer must answer ErrTargetImmutable, which the caller maps to
// a terminal Failing/TargetImmutable, and NOT ErrTargetConflict: the Secret is
// verifiably this owner's, so calling it someone else's would send whoever
// reads the status looking for a name collision that does not exist.
//
// The stored Secret must come back byte-identical: the check runs before
// populateManagedSecret, so nothing — not even the version annotation, which
// an immutable Secret would legally accept — is written on a refused sync.
func TestCreateOrUpdate_ImmutableSecretValueChanged_TargetImmutable(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")

	frozen := immutableSecret(owner, "v1", "old-value")
	cli := fake.NewClientBuilder().WithScheme(redactionScheme(t)).WithObjects(frozen).Build()
	w := &SecretWriter{Client: cli, Reader: cli}

	err := w.CreateOrUpdate(context.Background(), owner, owner.Namespace, owner.Name, "my-app-password", "new-value", "v2")
	if err == nil {
		t.Fatal("CreateOrUpdate() error = nil, want ErrTargetImmutable")
	}
	if !errors.Is(err, ErrTargetImmutable) {
		t.Fatalf("CreateOrUpdate() error = %v, want wrapped ErrTargetImmutable", err)
	}
	if errors.Is(err, ErrTargetConflict) {
		t.Fatalf("CreateOrUpdate() error = %v, must not also be a TargetConflict: the Secret is this owner's own", err)
	}

	var got corev1.Secret
	if getErr := cli.Get(context.Background(), client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, &got); getErr != nil {
		t.Fatalf("get immutable secret: %v", getErr)
	}
	if string(got.Data["my-app-password"]) != "old-value" {
		t.Fatalf("immutable Secret's data was modified: %v", got.Data)
	}
	if got.Annotations[AnnotationVersion] != "v1" {
		t.Fatalf("version annotation = %q, want it untouched at %q: a refused write must write nothing at all", got.Annotations[AnnotationVersion], "v1")
	}
}

// TestCreateOrUpdate_ImmutableSecretSameValueNewVersion_Succeeds is the case
// that stops anyone "simplifying" immutableBlocksWrite to a bare
// `Immutable != nil && *Immutable`. Kubernetes freezes .data only; object
// metadata stays editable. A rotation that produces the same bytes under a new
// Key Vault version is therefore a legal metadata-only update, and refusing it
// would park a perfectly healthy SecretSync in Failing/TargetImmutable forever
// with a version annotation that never catches up.
func TestCreateOrUpdate_ImmutableSecretSameValueNewVersion_Succeeds(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")

	frozen := immutableSecret(owner, "v1", "same-value")
	cli := fake.NewClientBuilder().WithScheme(redactionScheme(t)).WithObjects(frozen).Build()
	w := &SecretWriter{Client: cli, Reader: cli}

	err := w.CreateOrUpdate(context.Background(), owner, owner.Namespace, owner.Name, "my-app-password", "same-value", "v2")
	if err != nil {
		t.Fatalf("CreateOrUpdate() error = %v, want nil (an immutable Secret still accepts metadata-only updates)", err)
	}

	var got corev1.Secret
	if getErr := cli.Get(context.Background(), client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, &got); getErr != nil {
		t.Fatalf("get immutable secret: %v", getErr)
	}
	if got.Annotations[AnnotationVersion] != "v2" {
		t.Fatalf("version annotation = %q, want it advanced to %q", got.Annotations[AnnotationVersion], "v2")
	}
	if string(got.Data["my-app-password"]) != "same-value" {
		t.Fatalf("data = %q, want it unchanged at %q", got.Data["my-app-password"], "same-value")
	}
	if got.Immutable == nil || !*got.Immutable {
		t.Fatalf("immutable = %v, want it left set: the writer never unfreezes a Secret a user froze", got.Immutable)
	}
}

// conflictingUpdateClient builds a fake client whose first `conflicts` Update
// calls return a 409 Conflict while every later one passes through to the real
// fake store. It reproduces the stale-resourceVersion race: the Secret handed
// to Update came from the cached Get, and something (usually this operator's
// own immediately preceding write) has already moved it on.
func conflictingUpdateClient(t *testing.T, conflicts int, objs ...client.Object) client.WithWatch {
	t.Helper()
	seen := 0
	return fake.NewClientBuilder().
		WithScheme(redactionScheme(t)).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				seen++
				if seen <= conflicts {
					return apierrors.NewConflict(
						schema.GroupResource{Resource: "secrets"},
						obj.GetName(),
						errors.New("the object has been modified; please apply your changes to the latest version"),
					)
				}
				return c.Update(ctx, obj, opts...)
			},
		}).
		Build()
}

// TestCreateOrUpdate_ConflictOnFirstUpdate_RetriesAndConverges covers the
// duplicate at-least-once queue message: two reconciles for the same rotation
// overlap, the second builds its write from a resourceVersion the first has
// already superseded, and Update comes back 409. The cluster is already
// correct or about to be, so this must not surface as a failure at all —
// before the recovery existed it flapped the CR to Failing with a spurious
// SyncFailed event that the next reconcile silently healed.
func TestCreateOrUpdate_ConflictOnFirstUpdate_RetriesAndConverges(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")

	own := existingSyncedSecret(owner, "v1", "old-value")
	cli := conflictingUpdateClient(t, 1, own)
	w := &SecretWriter{Client: cli, Reader: cli}

	err := w.CreateOrUpdate(context.Background(), owner, owner.Namespace, owner.Name, "my-app-password", "new-value", "v2")
	if err != nil {
		t.Fatalf("CreateOrUpdate() error = %v, want nil (a single 409 is recovered, not reported)", err)
	}

	var got corev1.Secret
	if getErr := cli.Get(context.Background(), client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, &got); getErr != nil {
		t.Fatalf("get secret after conflict recovery: %v", getErr)
	}
	if string(got.Data["my-app-password"]) != "new-value" {
		t.Fatalf("secret data = %q, want the new value written by the retry", got.Data["my-app-password"])
	}
	if got.Annotations[AnnotationVersion] != "v2" {
		t.Fatalf("version annotation = %q, want %q", got.Annotations[AnnotationVersion], "v2")
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].UID != owner.UID {
		t.Fatalf("ownerReferences = %+v, want exactly one controller reference to the owner", got.OwnerReferences)
	}
}

// TestCreateOrUpdate_ConflictOnEveryUpdate_SurfacesConflict pins that the
// recovery is exactly ONE retry and not a hidden loop: the retry goes through
// applyUpdate rather than update, so a second 409 cannot recurse. A Secret
// something else is actively rewriting is a real condition the workqueue must
// back off on, so the conflict has to reach the caller — still recognisable as
// a 409 through the fmt.Errorf %w wrapping, and classified as neither of the
// two terminal states.
func TestCreateOrUpdate_ConflictOnEveryUpdate_SurfacesConflict(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")

	own := existingSyncedSecret(owner, "v1", "old-value")
	cli := conflictingUpdateClient(t, 100, own)
	w := &SecretWriter{Client: cli, Reader: cli}

	err := w.CreateOrUpdate(context.Background(), owner, owner.Namespace, owner.Name, "my-app-password", "new-value", "v2")
	if err == nil {
		t.Fatal("CreateOrUpdate() error = nil, want the second conflict surfaced")
	}
	if !apierrors.IsConflict(err) {
		t.Fatalf("CreateOrUpdate() error = %v, want it still recognisable as a 409 through the %%w wrapping", err)
	}
	if errors.Is(err, ErrTargetImmutable) || errors.Is(err, ErrTargetConflict) {
		t.Fatalf("CreateOrUpdate() error = %v, want a retryable conflict, not a terminal classification", err)
	}
}

// TestCreateOrUpdate_ConflictThenForeignOccupant_TargetConflict pins that the
// conflict retry re-checks ownership instead of trusting the copy it started
// from. Between the cached Get and the 409, the owner's Secret can be deleted
// and the name taken by something else; writing the retry blind would hand a
// vault value to a stranger's Secret, which is exactly what first-writer-wins
// (FR-012) forbids. The uncached re-read is the only view that can see that.
func TestCreateOrUpdate_ConflictThenForeignOccupant_TargetConflict(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")

	// The cached client still shows the owner's own Secret; the uncached
	// reader shows who actually holds the name now.
	cli := conflictingUpdateClient(t, 100, existingSyncedSecret(owner, "v1", "old-value"))
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      owner.Name,
			Namespace: owner.Namespace,
			Labels:    map[string]string{"created-by": "some-other-tool"},
		},
		Data: map[string][]byte{"unrelated-key": []byte("unrelated-value")},
	}
	truth := fake.NewClientBuilder().WithScheme(redactionScheme(t)).WithObjects(foreign).Build()
	w := &SecretWriter{Client: cli, Reader: truth}

	err := w.CreateOrUpdate(context.Background(), owner, owner.Namespace, owner.Name, "my-app-password", "new-value", "v2")
	if !errors.Is(err, ErrTargetConflict) {
		t.Fatalf("CreateOrUpdate() error = %v, want wrapped ErrTargetConflict", err)
	}

	var got corev1.Secret
	if getErr := truth.Get(context.Background(), client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, &got); getErr != nil {
		t.Fatalf("get foreign occupant: %v", getErr)
	}
	if _, ok := got.Data["my-app-password"]; ok {
		t.Fatalf("the retry wrote the vault value into a foreign Secret: %+v", got.Data)
	}
}

// staleImmutableCacheClient builds the client half of the stale-cache race for
// the Invalid path: its Get answers with the target as the cache saw it before
// someone set immutable: true (so applyUpdate's pre-check waves the write
// through), and its Update is rejected by the API server with Invalid. The
// uncached Reader wired next to it holds the truth.
func staleImmutableCacheClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(redactionScheme(t)).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if secret, ok := obj.(*corev1.Secret); ok {
					secret.Immutable = nil
				}
				return nil
			},
			Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
				return apierrors.NewInvalid(
					schema.GroupKind{Kind: "Secret"},
					obj.GetName(),
					field.ErrorList{field.Forbidden(field.NewPath("data"), "field is immutable when `immutable` is set")},
				)
			},
		}).
		Build()
}

// TestCreateOrUpdate_InvalidFromStaleCache_TargetImmutable covers the race the
// up-front check alone cannot catch: the cached copy the write was built from
// had not observed someone setting immutable: true, so the pre-check passed and
// the API server did the refusing. classifyInvalid answers it by re-reading
// through the UNCACHED reader and asking immutableBlocksWrite again — never by
// parsing the StatusError's details, which is why the Invalid error here is
// hand-built and its text is deliberately arbitrary. Matching on a field path
// or a cause string would couple this operator to wording owned by
// k8s.io/kubernetes, which is not even a dependency of this module.
func TestCreateOrUpdate_InvalidFromStaleCache_TargetImmutable(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")

	// The cached client answers Gets with Immutable stripped; the uncached
	// reader holds the Secret as the API server actually has it.
	cli := staleImmutableCacheClient(t, immutableSecret(owner, "v1", "old-value"))
	truth := fake.NewClientBuilder().
		WithScheme(redactionScheme(t)).
		WithObjects(immutableSecret(owner, "v1", "old-value")).
		Build()
	w := &SecretWriter{Client: cli, Reader: truth}

	err := w.CreateOrUpdate(context.Background(), owner, owner.Namespace, owner.Name, "my-app-password", "new-value", "v2")
	if !errors.Is(err, ErrTargetImmutable) {
		t.Fatalf("CreateOrUpdate() error = %v, want wrapped ErrTargetImmutable", err)
	}
}

// TestCreateOrUpdate_InvalidForOtherReason_ErrorUnchanged pins the other half
// of classifyInvalid: an Invalid that has nothing to do with immutability — an
// admission webhook, a validating policy — over a perfectly mutable Secret
// comes back exactly as it arrived. Nothing about a re-read may upgrade an
// unrelated, possibly transient rejection into the terminal TargetImmutable
// state, which no amount of retrying would ever leave.
func TestCreateOrUpdate_InvalidForOtherReason_ErrorUnchanged(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")

	mutable := existingSyncedSecret(owner, "v1", "old-value")
	cli := fake.NewClientBuilder().
		WithScheme(redactionScheme(t)).
		WithObjects(mutable).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
				return apierrors.NewInvalid(
					schema.GroupKind{Kind: "Secret"},
					obj.GetName(),
					field.ErrorList{field.Forbidden(field.NewPath("metadata", "labels"), "denied by admission webhook policy.example.com")},
				)
			},
		}).
		Build()
	w := &SecretWriter{Client: cli, Reader: cli}

	err := w.CreateOrUpdate(context.Background(), owner, owner.Namespace, owner.Name, "my-app-password", "new-value", "v2")
	if err == nil {
		t.Fatal("CreateOrUpdate() error = nil, want the webhook's Invalid surfaced")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("CreateOrUpdate() error = %v, want the original Invalid returned untouched", err)
	}
	if errors.Is(err, ErrTargetImmutable) {
		t.Fatalf("CreateOrUpdate() error = %v, must not be upgraded to the terminal TargetImmutable state", err)
	}
}

// TestCreateOrUpdate_InvalidWithFailedReRead_ErrorUnchanged pins the last
// branch of classifyInvalid: when the re-read that would settle the question
// fails, the writer has no evidence, so it returns the original Invalid rather
// than guessing. Guessing the other way would park the CR in a terminal
// TargetImmutable — a state nothing retries out of — on the strength of an API
// call that did not answer.
func TestCreateOrUpdate_InvalidWithFailedReRead_ErrorUnchanged(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")

	// The Secret really IS immutable, so a re-read that worked would classify
	// this as TargetImmutable. The reader below never lets that happen, which
	// is what makes this test about the failed re-read and nothing else.
	cli := staleImmutableCacheClient(t, immutableSecret(owner, "v1", "old-value"))
	unavailable := fake.NewClientBuilder().
		WithScheme(redactionScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return apierrors.NewInternalError(errors.New("apiserver unavailable"))
			},
		}).
		Build()
	w := &SecretWriter{Client: cli, Reader: unavailable}

	err := w.CreateOrUpdate(context.Background(), owner, owner.Namespace, owner.Name, "my-app-password", "new-value", "v2")
	if err == nil {
		t.Fatal("CreateOrUpdate() error = nil, want the original Invalid surfaced")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("CreateOrUpdate() error = %v, want the original Invalid returned untouched", err)
	}
	if errors.Is(err, ErrTargetImmutable) {
		t.Fatalf("CreateOrUpdate() error = %v, must not reach a terminal state on an unanswered re-read", err)
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
