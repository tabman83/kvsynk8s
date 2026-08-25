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
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kvsynk8sv1alpha1 "github.com/tabman83/kvsynk8s/api/v1alpha1"
	"github.com/tabman83/kvsynk8s/internal/azure"
	syncpkg "github.com/tabman83/kvsynk8s/internal/sync"
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

	// fakeSecretName is the vault secret name reused by the US3 reconciliation
	// specs below (RequeueAfter, drift repair, convergence, retry/backoff).
	fakeSecretName = "fake-secret"
)

// fakeSecretReader is a hand-rolled azure.SecretReader for envtest, which has
// no network access to Azure (T010 instructions). Every value it can return
// is an obviously-fake sentinel, per constitution I ("Test fixtures MUST use
// fake values that are clearly fake").
//
// mu guards values: the US3 tests (T021) run a real manager whose controller
// goroutine calls GetLatest concurrently with the test goroutine calling set
// (e.g. to change the reader's answer mid-test, simulating a vault-side
// change with no event injected) — without a lock that's a data race.
type fakeSecretReader struct {
	mu     sync.Mutex
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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[vaultName+"/"+secretName] = fakeSecretEntry{value: value, version: version}
}

// GetLatest implements azure.SecretReader.
func (f *fakeSecretReader) GetLatest(_ context.Context, vaultName, secretName string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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

// flakyThenOKReader fails GetLatest with a transient error the first
// failCount calls, then succeeds every time after. Used by the retry/backoff
// test (T021, constitution IV): this failure-then-recovery pattern is
// observed only through controller-runtime's rate-limited workqueue
// automatically re-invoking Reconcile after an error return, which requires
// a real running controller (see startManagerFor) — a manually-invoked
// Reconcile call, as used elsewhere in this file, cannot exercise it.
type flakyThenOKReader struct {
	mu             sync.Mutex
	remainingFails int
	value, version string
}

func (f *flakyThenOKReader) GetLatest(_ context.Context, _, _ string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.remainingFails > 0 {
		f.remainingFails--
		return "", "", fmt.Errorf("kvsynk8s: fake transient failure: %w", azure.ErrTransient)
	}
	return f.value, f.version, nil
}

var _ azure.SecretReader = (*flakyThenOKReader)(nil)

// startManagerFor spins up a full manager (cache + registered controller, no
// metrics/health/webhook endpoints) around a fresh SecretSyncReconciler using
// reader and interval, and starts it in the background. Returning cancel lets
// the caller (typically via DeferCleanup) stop it once the spec finishes.
//
// This exists because several US3 behaviors only manifest on the real
// controller-runtime request path — the Owns() watch re-triggering a
// reconcile on drift, the periodic RequeueAfter firing on its own schedule,
// and the rate-limited workqueue retrying a failed reconcile — none of which
// a direct r.Reconcile(ctx, req) call (the pattern the rest of this file
// uses for US1's synchronous, single-shot assertions) can exercise.
func startManagerFor(reader azure.SecretReader, interval time.Duration) context.CancelFunc {
	// Each It() in this Context starts its own manager, and controller-runtime
	// validates controller-name uniqueness against a process-global metrics
	// registry — so a second "secretsync" controller in a later spec would
	// otherwise be rejected as a duplicate even though the prior manager has
	// already been stopped. SkipNameValidation opts out of that check; these
	// tests don't scrape metrics, so unique names/metrics per controller
	// don't matter here.
	skipNameValidation := true
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme.Scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		Controller: config.Controller{
			SkipNameValidation: &skipNameValidation,
		},
	})
	Expect(err).NotTo(HaveOccurred())

	r := &SecretSyncReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Reader:            reader,
		ReconcileInterval: interval,
	}
	Expect(r.SetupWithManager(mgr, nil)).To(Succeed())

	mgrCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer GinkgoRecover()
		_ = mgr.Start(mgrCtx)
	}()
	Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())

	return cancel
}

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
			vaultSecret := fakeSecretName
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
			vaultSecret := fakeSecretName
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

	Context("when spec.target.secretName is renamed", func() {
		It("creates the Secret at the new name and deletes the old one, leaving other SecretSyncs' Secrets alone", func() {
			ctx := context.Background()

			name := "ss-rename-" + shortUID()
			vaultSecret := "rename-secret-" + shortUID()
			oldTarget := "target-rename-old-" + shortUID()
			newTarget := "target-rename-new-" + shortUID()
			const fakeValue = "SENTINEL-fake-value-rename-test-not-real"

			reader.set(fakeVaultName, vaultSecret, fakeValue, "v1")

			ss := newTestSecretSync(namespace, name, fakeVaultName, vaultSecret, oldTarget, fakeDataKey)
			Expect(k8sClient.Create(ctx, ss)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ss)
			})

			// A second, unrelated SecretSync in the same namespace, synced to
			// its own target: the rename sweep below must never touch its
			// Secret even though it carries the same managed-by label.
			otherName := "ss-rename-other-" + shortUID()
			otherVaultSecret := "rename-other-secret-" + shortUID()
			otherTarget := "target-rename-other-" + shortUID()
			const otherValue = "SENTINEL-fake-value-rename-other-not-real"
			reader.set(fakeVaultName, otherVaultSecret, otherValue, "v1")

			ssOther := newTestSecretSync(namespace, otherName, fakeVaultName, otherVaultSecret, otherTarget, fakeDataKey)
			Expect(k8sClient.Create(ctx, ssOther)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ssOther)
			})

			r := reconcilerFor(reader)
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}

			// First reconcile of each: both Secrets exist at their targets.
			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: otherName, Namespace: namespace}})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: oldTarget, Namespace: namespace}, &corev1.Secret{})).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: otherTarget, Namespace: namespace}, &corev1.Secret{})).To(Succeed())

			// Rename the target on the first SecretSync.
			var latest kvsynk8sv1alpha1.SecretSync
			Expect(k8sClient.Get(ctx, req.NamespacedName, &latest)).To(Succeed())
			latest.Spec.Target.SecretName = newTarget
			Expect(k8sClient.Update(ctx, &latest)).To(Succeed())

			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			// The new target exists, carries the value, and is owned by ss.
			var renamed corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: newTarget, Namespace: namespace}, &renamed)).To(Succeed())
			Expect(string(renamed.Data[fakeDataKey])).To(Equal(fakeValue))
			Expect(renamed.OwnerReferences).To(ContainElement(HaveField("Name", name)))

			// The old target is gone: the rename must not orphan it.
			err = k8sClient.Get(ctx, types.NamespacedName{Name: oldTarget, Namespace: namespace}, &corev1.Secret{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(),
				"the Secret at the previous target name should be deleted after the rename syncs")

			// The unrelated SecretSync's Secret is untouched.
			var otherSecret corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: otherTarget, Namespace: namespace}, &otherSecret)).To(Succeed())
			Expect(string(otherSecret.Data[fakeDataKey])).To(Equal(otherValue))
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

	// T021 (US3): recovery/drift-repair behaviors that only exist on the real
	// controller-runtime request path, not on a manually-invoked Reconcile
	// call. See startManagerFor's doc comment for why each of these needs a
	// running manager.
	Context("US3: periodic reconciliation, drift repair, and retry with backoff", func() {
		It("returns RequeueAfter equal to the configured reconcile interval on a successful reconcile", func() {
			ctx := context.Background()

			name := "ss-interval-" + shortUID()
			vaultName := fakeVaultName
			vaultSecret := fakeSecretName
			targetName := "target-" + shortUID()
			dataKey := fakeDataKey

			reader.set(vaultName, vaultSecret, "SENTINEL-fake-value-interval-test-not-real", "v1")

			ss := newTestSecretSync(namespace, name, vaultName, vaultSecret, targetName, dataKey)
			Expect(k8sClient.Create(ctx, ss)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ss)
			})

			const configuredInterval = 250 * time.Millisecond
			r := &SecretSyncReconciler{
				Client:            k8sClient,
				Scheme:            k8sClient.Scheme(),
				Reader:            reader,
				ReconcileInterval: configuredInterval,
			}
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}

			result, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(configuredInterval))
		})

		It("re-creates the managed Secret when it is deleted in-cluster, via the Owns() watch", func() {
			ctx := context.Background()

			name := "ss-recreate-" + shortUID()
			vaultName := fakeVaultName
			vaultSecret := fakeSecretName
			targetName := "target-" + shortUID()
			dataKey := fakeDataKey
			targetKey := types.NamespacedName{Name: targetName, Namespace: namespace}
			const fakeValue = "SENTINEL-fake-value-recreate-test-not-real"

			mgrReader := newFakeSecretReader()
			mgrReader.set(vaultName, vaultSecret, fakeValue, "v1")

			ss := newTestSecretSync(namespace, name, vaultName, vaultSecret, targetName, dataKey)
			Expect(k8sClient.Create(ctx, ss)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ss)
			})

			// A long interval: only the Owns() watch, not periodic
			// reconciliation, should be responsible for the re-creation below.
			cancel := startManagerFor(mgrReader, time.Hour)
			DeferCleanup(cancel)

			var secret corev1.Secret
			Eventually(func() error {
				return k8sClient.Get(ctx, targetKey, &secret)
			}).Should(Succeed(), "Secret should be created once the manager's informer picks up the SecretSync (startup convergence)")

			Expect(k8sClient.Delete(ctx, &secret)).To(Succeed())

			Eventually(func() error {
				return k8sClient.Get(ctx, targetKey, &corev1.Secret{})
			}).Should(Succeed(), "managed Secret should be re-created via the Owns() watch without any manual reconcile")
		})

		It("converges to a changed vault value on the next scheduled reconcile with no event injected", func() {
			ctx := context.Background()

			name := "ss-converge-" + shortUID()
			vaultName := fakeVaultName
			vaultSecret := fakeSecretName
			targetName := "target-" + shortUID()
			dataKey := fakeDataKey
			targetKey := types.NamespacedName{Name: targetName, Namespace: namespace}

			mgrReader := newFakeSecretReader()
			mgrReader.set(vaultName, vaultSecret, "SENTINEL-fake-value-v1-not-real", "v1")

			ss := newTestSecretSync(namespace, name, vaultName, vaultSecret, targetName, dataKey)
			Expect(k8sClient.Create(ctx, ss)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ss)
			})

			const shortInterval = 200 * time.Millisecond
			cancel := startManagerFor(mgrReader, shortInterval)
			DeferCleanup(cancel)

			Eventually(func() (string, error) {
				var secret corev1.Secret
				if err := k8sClient.Get(ctx, targetKey, &secret); err != nil {
					return "", err
				}
				return string(secret.Data[dataKey]), nil
			}).Should(Equal("SENTINEL-fake-value-v1-not-real"))

			// The vault "changes" with no queue event injected: only the
			// periodic RequeueAfter can pick this up.
			mgrReader.set(vaultName, vaultSecret, "SENTINEL-fake-value-v2-not-real", "v2")

			Eventually(func() (string, error) {
				var secret corev1.Secret
				if err := k8sClient.Get(ctx, targetKey, &secret); err != nil {
					return "", err
				}
				return string(secret.Data[dataKey]), nil
			}, 5*time.Second, 50*time.Millisecond).Should(Equal("SENTINEL-fake-value-v2-not-real"))
		})

		It("retries a transient reader failure via the rate-limited workqueue until InSync (constitution IV)", func() {
			ctx := context.Background()

			name := "ss-retry-" + shortUID()
			vaultName := fakeVaultName
			vaultSecret := fakeSecretName
			targetName := "target-" + shortUID()
			dataKey := fakeDataKey

			ss := newTestSecretSync(namespace, name, vaultName, vaultSecret, targetName, dataKey)
			Expect(k8sClient.Create(ctx, ss)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ss)
			})

			// Fails the first 3 attempts with a transient error, then
			// succeeds: exercises retry via controller-runtime's default
			// rate-limited workqueue, without any manual re-reconcile.
			flaky := &flakyThenOKReader{
				remainingFails: 3,
				value:          "SENTINEL-fake-value-recovered-not-real",
				version:        "v9",
			}

			// A long interval: convergence here must come from error-driven
			// backoff retries, not the periodic reconcile.
			cancel := startManagerFor(flaky, time.Hour)
			DeferCleanup(cancel)

			req := types.NamespacedName{Name: name, Namespace: namespace}
			Eventually(func() (kvsynk8sv1alpha1.SecretSyncState, error) {
				var updated kvsynk8sv1alpha1.SecretSync
				if err := k8sClient.Get(ctx, req, &updated); err != nil {
					return "", err
				}
				return updated.Status.State, nil
			}, 10*time.Second, 50*time.Millisecond).Should(Equal(kvsynk8sv1alpha1.SecretSyncStateInSync))
		})
	})

	// T026 (US4, FR-008/SC-006): a SecretSync that fails forever must never
	// block, slow down, or otherwise affect a completely unrelated SecretSync
	// that is healthy. This runs a real manager (not a manually-invoked
	// Reconcile) precisely so the two declarations are processed through the
	// same shared workqueue a production operator would use -- proving
	// isolation at the controller level, not just within one engine.Sync call.
	Context("US4: a permanently failing SecretSync never blocks a healthy one", func() {
		It("lets the healthy SecretSync reach InSync on its own schedule while the other stays Failing forever", func() {
			ctx := context.Background()

			okVaultSecret := "ok-secret-" + shortUID()
			failVaultSecret := "fail-secret-" + shortUID()

			mgrReader := newFakeSecretReader()
			// Deliberately never call mgrReader.set for failVaultSecret: every
			// GetLatest call for it returns ErrSecretNotFound, so that
			// SecretSync can never leave the Failing state no matter how many
			// times it is reconciled.
			mgrReader.set(fakeVaultName, okVaultSecret, "SENTINEL-fake-value-isolation-ok-not-real", "v1")

			nameOK := "ss-isolation-ok-" + shortUID()
			nameFail := "ss-isolation-fail-" + shortUID()
			targetOK := "target-isolation-ok-" + shortUID()
			targetFail := "target-isolation-fail-" + shortUID()

			ssFail := newTestSecretSync(namespace, nameFail, fakeVaultName, failVaultSecret, targetFail, fakeDataKey)
			Expect(k8sClient.Create(ctx, ssFail)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ssFail)
			})

			ssOK := newTestSecretSync(namespace, nameOK, fakeVaultName, okVaultSecret, targetOK, fakeDataKey)
			Expect(k8sClient.Create(ctx, ssOK)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ssOK)
			})

			// A short interval so both declarations get reconciled several
			// times over the course of this test -- if the failing one were
			// somehow starving the healthy one (a shared-workqueue bug, or a
			// blocking call), that would show up as the healthy one never
			// reaching InSync within the timeout below.
			const shortInterval = 200 * time.Millisecond
			cancel := startManagerFor(mgrReader, shortInterval)
			DeferCleanup(cancel)

			Eventually(func() (kvsynk8sv1alpha1.SecretSyncState, error) {
				var updated kvsynk8sv1alpha1.SecretSync
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: nameOK, Namespace: namespace}, &updated); err != nil {
					return "", err
				}
				return updated.Status.State, nil
			}, 5*time.Second, 50*time.Millisecond).Should(Equal(kvsynk8sv1alpha1.SecretSyncStateInSync),
				"the healthy SecretSync must reach InSync regardless of the other one failing")

			// The failing one must genuinely still be failing -- not just not
			// yet reconciled -- and stay that way across further reconciles.
			Consistently(func() (kvsynk8sv1alpha1.SecretSyncState, error) {
				var updated kvsynk8sv1alpha1.SecretSync
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: nameFail, Namespace: namespace}, &updated); err != nil {
					return "", err
				}
				return updated.Status.State, nil
			}, time.Second, 100*time.Millisecond).Should(Equal(kvsynk8sv1alpha1.SecretSyncStateFailing))

			var failing kvsynk8sv1alpha1.SecretSync
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nameFail, Namespace: namespace}, &failing)).To(Succeed())
			Expect(failing.Status.Reason).To(Equal(syncpkg.ReasonSecretNotFound))

			// The healthy Secret must actually carry its value -- proving the
			// two declarations were not just both stuck in some shared broken
			// state.
			var okSecret corev1.Secret
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetOK, Namespace: namespace}, &okSecret)).To(Succeed())
			Expect(string(okSecret.Data[fakeDataKey])).To(Equal("SENTINEL-fake-value-isolation-ok-not-real"))

			// And the failing declaration must never have gotten a Secret at
			// all (nothing to fabricate on SecretNotFound with no prior sync).
			err := k8sClient.Get(ctx, types.NamespacedName{Name: targetFail, Namespace: namespace}, &corev1.Secret{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})

	// T027 (US4, FR-009/FR-010): a "Synced" Normal event on success and a
	// "SyncFailed" Warning event on failure, referencing only vault/secret/
	// version identifiers -- never a secret value.
	Context("US4: EventRecorder", func() {
		It("emits a Synced event on success naming vault/secret/version, with no value in it", func() {
			ctx := context.Background()

			name := "ss-event-ok-" + shortUID()
			vaultSecret := "event-ok-secret-" + shortUID()
			targetName := "target-event-ok-" + shortUID()
			const sentinel = "SENTINEL-fake-value-event-ok-not-real"

			reader.set(fakeVaultName, vaultSecret, sentinel, "v7")

			ss := newTestSecretSync(namespace, name, fakeVaultName, vaultSecret, targetName, fakeDataKey)
			Expect(k8sClient.Create(ctx, ss)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ss)
			})

			recorder := events.NewFakeRecorder(10)
			r := reconcilerFor(reader)
			r.Recorder = recorder
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}

			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			var evt string
			Eventually(recorder.Events).Should(Receive(&evt))
			Expect(evt).To(ContainSubstring("Synced"))
			Expect(evt).To(ContainSubstring(vaultSecret))
			Expect(evt).To(ContainSubstring("v7"))
			Expect(evt).NotTo(ContainSubstring(sentinel))
		})

		It("emits a SyncFailed event carrying status.Reason on failure, with no value in it", func() {
			ctx := context.Background()

			name := "ss-event-fail-" + shortUID()
			vaultSecret := "event-fail-secret-" + shortUID() // never reader.set -> SecretNotFound
			targetName := "target-event-fail-" + shortUID()

			ss := newTestSecretSync(namespace, name, fakeVaultName, vaultSecret, targetName, fakeDataKey)
			Expect(k8sClient.Create(ctx, ss)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ss)
			})

			recorder := events.NewFakeRecorder(10)
			r := reconcilerFor(reader)
			r.Recorder = recorder
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}

			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			var evt string
			Eventually(recorder.Events).Should(Receive(&evt))
			Expect(evt).To(ContainSubstring("SyncFailed"))
			Expect(evt).To(ContainSubstring(syncpkg.ReasonSecretNotFound))
			Expect(evt).To(ContainSubstring(vaultSecret))
		})

		It("never wires a Recorder for a reconciler built without SetupWithManager (nil-safe)", func() {
			ctx := context.Background()

			name := "ss-event-norecorder-" + shortUID()
			vaultSecret := "event-norecorder-secret-" + shortUID()
			targetName := "target-event-norecorder-" + shortUID()

			reader.set(fakeVaultName, vaultSecret, "SENTINEL-fake-value-norecorder-not-real", "v1")

			ss := newTestSecretSync(namespace, name, fakeVaultName, vaultSecret, targetName, fakeDataKey)
			Expect(k8sClient.Create(ctx, ss)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ss)
			})

			r := reconcilerFor(reader) // Recorder left nil, exactly like every pre-T027 test in this file
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}

			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred(), "a nil Recorder must never cause Reconcile to fail or panic")
		})
	})
})
