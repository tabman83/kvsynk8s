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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kvsynk8sv1alpha1 "github.com/tabman83/kvsynk8s/api/v1alpha1"
	"github.com/tabman83/kvsynk8s/internal/azure"
)

// Expected contract of the managed Secret (data-model.md "Managed Kubernetes
// Secret"). Declared here, not imported from an implementation package,
// because the writer (internal/sync/writer.go, T011) does not exist yet:
// this test is the specification the implementation must satisfy.
const (
	managedByLabelKey    = "app.kubernetes.io/managed-by"
	managedByLabelValue  = "kvsynk8s"
	vaultAnnotationKey   = "kvsynk8s.io/vault"
	secretAnnotationKey  = "kvsynk8s.io/secret"
	versionAnnotationKey = "kvsynk8s.io/version"

	// secretSyncFinalizer is the finalizer name the reconciler is expected to
	// set on every SecretSync it manages, so that CR deletion can delete the
	// managed Secret before the CR itself is removed (data-model.md state
	// transitions: "(CR deleted) --finalizer--> managed Secret deleted").
	secretSyncFinalizer = "kvsynk8s.io/secretsync-finalizer"

	// reasonTargetConflict is the machine-readable status.reason for FR-012
	// (data-model.md "Identity & uniqueness"): the second SecretSync to claim
	// a given namespace+target.secretName goes Failing, not the first.
	reasonTargetConflict = "TargetConflict"

	// fakeVaultName and fakeDataKey are shared fixture values reused across
	// the specs below (a real vault name and a real Secret data key would
	// work identically; these are fake per constitution I).
	fakeVaultName = "fake-vault"
	fakeDataKey   = "value"
)

// fakeSecretReader is a hand-rolled azure.SecretReader for envtest, which has
// no network access to Azure (T010 instructions). Every value it can return
// is an obviously-fake sentinel, per constitution I ("Test fixtures MUST use
// fake values that are clearly fake").
type fakeSecretReader struct {
	values map[string]fakeSecretEntry
}

type fakeSecretEntry struct {
	value   string
	version string
	err     error
}

func newFakeSecretReader() *fakeSecretReader {
	return &fakeSecretReader{values: make(map[string]fakeSecretEntry)}
}

// set registers the value+version fakeSecretReader.GetLatest returns for
// (vaultName, secretName). vaultName is always fakeVaultName today; it stays
// a real parameter because a future multi-vault test (US2 matching) needs it.
//
//nolint:unparam // see comment above
func (f *fakeSecretReader) set(vaultName, secretName, value, version string) {
	f.values[vaultName+"/"+secretName] = fakeSecretEntry{value: value, version: version}
}

// GetLatest implements azure.SecretReader.
func (f *fakeSecretReader) GetLatest(_ context.Context, vaultName, secretName string) (string, string, error) {
	entry, ok := f.values[vaultName+"/"+secretName]
	if !ok {
		return "", "", fmt.Errorf("vault %q secret %q: %w", vaultName, secretName, azure.ErrSecretNotFound)
	}
	if entry.err != nil {
		return "", "", entry.err
	}
	return entry.value, entry.version, nil
}

var _ azure.SecretReader = (*fakeSecretReader)(nil)

// newTestSecretSync builds a minimal, valid SecretSync for envtest. Vault and
// secret names are fake identifiers only; no value ever appears in a spec
// field (constitution I). namespace is always "default" today; it stays a
// real parameter since SecretSync is namespaced and future tests will vary it.
//
//nolint:unparam // see comment above
func newTestSecretSync(namespace, name, vaultName, vaultSecret, targetSecretName, dataKey string) *kvsynk8sv1alpha1.SecretSync {
	return &kvsynk8sv1alpha1.SecretSync{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: kvsynk8sv1alpha1.SecretSyncSpec{
			Vault: kvsynk8sv1alpha1.VaultSpec{
				Name:   vaultName,
				Secret: vaultSecret,
			},
			Target: kvsynk8sv1alpha1.TargetSpec{
				SecretName: targetSecretName,
				DataKey:    dataKey,
			},
		},
	}
}

// shortUID returns a short, unique suffix for building resource names that
// cannot collide across the specs in this file.
func shortUID() string {
	return uuid.NewString()[:8]
}

var _ = Describe("SecretSync Controller", func() {
	const namespace = "default"

	var reader *fakeSecretReader

	// reconcilerFor wires a reconciler for this test with the given fake
	// SecretReader injected, since envtest cannot reach real Key Vault. The
	// Reader field does not exist on SecretSyncReconciler yet (only Client
	// and Scheme are scaffolded) — adding it, and acting on it, is part of
	// implementing T011-T013.
	reconcilerFor := func(r azure.SecretReader) *SecretSyncReconciler {
		return &SecretSyncReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Reader: r,
		}
	}

	BeforeEach(func() {
		reader = newFakeSecretReader()
	})

	Context("when reconciling a new SecretSync", func() {
		It("creates the target Secret and reports InSync with a matching observedGeneration", func() {
			ctx := context.Background()

			name := "ss-create-" + shortUID()
			vaultName := fakeVaultName
			vaultSecret := "fake-secret"
			targetName := "target-" + shortUID()
			dataKey := fakeDataKey
			const fakeValue = "SENTINEL-fake-value-create-test-not-real"

			reader.set(vaultName, vaultSecret, fakeValue, "v1")

			ss := newTestSecretSync(namespace, name, vaultName, vaultSecret, targetName, dataKey)
			Expect(k8sClient.Create(ctx, ss)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ss)
			})

			r := reconcilerFor(reader)
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}

			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			var secret corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetName, Namespace: namespace}, &secret)).To(Succeed())
			Expect(secret.Labels[managedByLabelKey]).To(Equal(managedByLabelValue))
			Expect(secret.Annotations[vaultAnnotationKey]).To(Equal(vaultName))
			Expect(secret.Annotations[secretAnnotationKey]).To(Equal(vaultSecret))
			Expect(secret.Annotations[versionAnnotationKey]).To(Equal("v1"))
			Expect(string(secret.Data[dataKey])).To(Equal(fakeValue))
			Expect(secret.OwnerReferences).To(ContainElement(HaveField("Name", name)))

			var updated kvsynk8sv1alpha1.SecretSync
			Expect(k8sClient.Get(ctx, req.NamespacedName, &updated)).To(Succeed())
			Expect(updated.Status.State).To(Equal(kvsynk8sv1alpha1.SecretSyncStateInSync))
			Expect(updated.Status.ObservedGeneration).To(Equal(updated.Generation))
			Expect(updated.Status.SyncedVersion).To(Equal("v1"))
		})
	})

	Context("when a SecretSync is deleted", func() {
		It("runs the finalizer and deletes the managed Secret", func() {
			ctx := context.Background()

			name := "ss-delete-" + shortUID()
			vaultName := fakeVaultName
			vaultSecret := "fake-secret"
			targetName := "target-" + shortUID()
			dataKey := fakeDataKey

			reader.set(vaultName, vaultSecret, "SENTINEL-fake-value-delete-test-not-real", "v1")

			ss := newTestSecretSync(namespace, name, vaultName, vaultSecret, targetName, dataKey)
			Expect(k8sClient.Create(ctx, ss)).To(Succeed())

			r := reconcilerFor(reader)
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}

			// First reconcile: creates the Secret and (per data-model.md)
			// must have attached the finalizer so deletion can be intercepted.
			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			var afterCreate kvsynk8sv1alpha1.SecretSync
			Expect(k8sClient.Get(ctx, req.NamespacedName, &afterCreate)).To(Succeed())
			Expect(afterCreate.Finalizers).To(ContainElement(secretSyncFinalizer))

			var secret corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetName, Namespace: namespace}, &secret)).To(Succeed())

			// Delete the CR: with a finalizer present this only sets
			// deletionTimestamp; the object is not removed from the API yet.
			Expect(k8sClient.Delete(ctx, &afterCreate)).To(Succeed())

			// Second reconcile: the controller observes the deletionTimestamp,
			// deletes the managed Secret, and removes its own finalizer so
			// the API server can finish deleting the CR.
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool {
				err := k8sClient.Get(ctx, req.NamespacedName, &kvsynk8sv1alpha1.SecretSync{})
				return apierrors.IsNotFound(err)
			}).Should(BeTrue(), "SecretSync should be fully removed once the finalizer is cleared")

			err = k8sClient.Get(ctx, types.NamespacedName{Name: targetName, Namespace: namespace}, &corev1.Secret{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "managed Secret should have been deleted by the finalizer")
		})
	})

	Context("when two SecretSyncs target the same Secret name in the same namespace", func() {
		It("keeps the first InSync and fails the later one with TargetConflict", func() {
			ctx := context.Background()

			targetName := "shared-target-" + shortUID()
			vaultName := fakeVaultName
			dataKey := fakeDataKey

			const valueA = "SENTINEL-fake-value-conflict-a-not-real"
			const valueB = "SENTINEL-fake-value-conflict-b-not-real"
			reader.set(vaultName, "secret-a", valueA, "v1")
			reader.set(vaultName, "secret-b", valueB, "v1")

			nameA := "ss-conflict-a-" + shortUID()
			nameB := "ss-conflict-b-" + shortUID()

			ssA := newTestSecretSync(namespace, nameA, vaultName, "secret-a", targetName, dataKey)
			Expect(k8sClient.Create(ctx, ssA)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ssA)
			})

			ssB := newTestSecretSync(namespace, nameB, vaultName, "secret-b", targetName, dataKey)
			Expect(k8sClient.Create(ctx, ssB)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ssB)
			})

			r := reconcilerFor(reader)

			// The first declaration to reconcile wins the target and reaches
			// InSync (FR-012: "first writer wins").
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: nameA, Namespace: namespace}})
			Expect(err).NotTo(HaveOccurred())

			var afterA kvsynk8sv1alpha1.SecretSync
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nameA, Namespace: namespace}, &afterA)).To(Succeed())
			Expect(afterA.Status.State).To(Equal(kvsynk8sv1alpha1.SecretSyncStateInSync))

			// The second declaration reconciles afterwards and finds the
			// target Secret already managed by a different SecretSync.
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: nameB, Namespace: namespace}})
			Expect(err).NotTo(HaveOccurred())

			var afterB kvsynk8sv1alpha1.SecretSync
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nameB, Namespace: namespace}, &afterB)).To(Succeed())
			Expect(afterB.Status.State).To(Equal(kvsynk8sv1alpha1.SecretSyncStateFailing))
			Expect(afterB.Status.Reason).To(Equal(reasonTargetConflict))
			// The conflict message must identify the target, never a value.
			Expect(afterB.Status.Message).NotTo(ContainSubstring(valueA))
			Expect(afterB.Status.Message).NotTo(ContainSubstring(valueB))

			// The Secret must still reflect A: B's conflicting reconcile must
			// never have overwritten it.
			var secret corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetName, Namespace: namespace}, &secret)).To(Succeed())
			Expect(string(secret.Data[dataKey])).To(Equal(valueA))
		})
	})
})
