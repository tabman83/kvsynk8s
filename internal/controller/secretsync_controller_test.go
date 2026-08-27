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
	"testing"
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

// deadlineCapturingReader wraps another azure.SecretReader and records the
// deadline (if any) of the context each GetLatest call receives. Used to
// assert that Reconcile runs its vault reads under the per-reconcile timeout
// without actually having to wait that timeout out.
type deadlineCapturingReader struct {
	inner azure.SecretReader

	mu          sync.Mutex
	deadline    time.Time
	hadDeadline bool
}

func (d *deadlineCapturingReader) GetLatest(ctx context.Context, vaultName, secretName string) (string, string, error) {
	d.mu.Lock()
	d.deadline, d.hadDeadline = ctx.Deadline()
	d.mu.Unlock()
	return d.inner.GetLatest(ctx, vaultName, secretName)
}

func (d *deadlineCapturingReader) captured() (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.deadline, d.hadDeadline
}

var _ azure.SecretReader = (*deadlineCapturingReader)(nil)

// deadlineConsumingReader models a black-holed Key Vault endpoint: GetLatest
// neither answers nor fails fast, it hangs until the reconcile's own deadline
// expires and only then reports the timeout. internal/azure classifies a
// network-level failure with no HTTP response as ErrTransient, so that is what
// comes back here; the message carries vault and secret identifiers only, like
// classifyGetSecretError's own (constitution I).
//
// The point of the double is what it leaves behind rather than what it
// returns: every call site after it in Reconcile receives an already-expired
// context.
type deadlineConsumingReader struct{}

func (deadlineConsumingReader) GetLatest(ctx context.Context, vaultName, secretName string) (string, string, error) {
	<-ctx.Done()
	return "", "", fmt.Errorf("vault %q secret %q: %w", vaultName, secretName, azure.ErrTransient)
}

var _ azure.SecretReader = deadlineConsumingReader{}

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
		// The same label-filtered Secret cache production runs with
		// (cmd/main.go), so these specs exercise the real configuration —
		// including unmanaged Secrets being invisible to cached reads.
		Cache: ManagedSecretCacheOptions(),
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

	// The manager's Secret cache is label-filtered to managed Secrets only
	// (ManagedSecretCacheOptions, mirrored from cmd/main.go), so an unmanaged
	// Secret squatting on the target name is invisible to the reconciler's
	// cached Get. FR-012 must hold regardless: the writer's uncached Create
	// hits AlreadyExists, maps it to ErrTargetConflict, and the SecretSync
	// still ends Failing/TargetConflict with the unmanaged Secret untouched.
	// This needs a running manager — a manually-built reconciler on the
	// direct (uncached) k8sClient would still see the unmanaged Secret and
	// take the old engine-side conflict path instead.
	Context("when an unmanaged Secret already occupies the target name", func() {
		It("reports TargetConflict through the filtered cache and leaves the unmanaged Secret untouched", func() {
			ctx := context.Background()

			name := "ss-unmanaged-conflict-" + shortUID()
			vaultSecret := "unmanaged-conflict-secret-" + shortUID()
			targetName := "target-unmanaged-" + shortUID()
			targetKey := types.NamespacedName{Name: targetName, Namespace: namespace}
			const preExistingValue = "SENTINEL-fake-value-pre-existing-unmanaged-not-real"

			// A pre-existing Secret with no managed-by label: the filtered
			// cache never sees it.
			unmanaged := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: targetName, Namespace: namespace},
				StringData: map[string]string{fakeDataKey: preExistingValue},
			}
			Expect(k8sClient.Create(ctx, unmanaged)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), unmanaged)
			})

			mgrReader := newFakeSecretReader()
			mgrReader.set(fakeVaultName, vaultSecret, "SENTINEL-fake-value-unmanaged-conflict-not-real", "v1")

			ss := newTestSecretSync(namespace, name, fakeVaultName, vaultSecret, targetName, fakeDataKey)
			Expect(k8sClient.Create(ctx, ss)).To(Succeed())

			// A long interval: the first reconcile alone must detect the
			// conflict; nothing here depends on periodic retries.
			cancel := startManagerFor(mgrReader, time.Hour)
			DeferCleanup(cancel)

			ssKey := types.NamespacedName{Name: name, Namespace: namespace}
			Eventually(func() (string, error) {
				var updated kvsynk8sv1alpha1.SecretSync
				if err := k8sClient.Get(ctx, ssKey, &updated); err != nil {
					return "", err
				}
				if updated.Status.State != kvsynk8sv1alpha1.SecretSyncStateFailing {
					return string(updated.Status.State), nil
				}
				return updated.Status.Reason, nil
			}, 5*time.Second, 50*time.Millisecond).Should(Equal(reasonTargetConflict))

			// The unmanaged Secret must be byte-for-byte untouched: no data
			// overwrite, no adopted label, no owner reference.
			var after corev1.Secret
			Expect(k8sClient.Get(ctx, targetKey, &after)).To(Succeed())
			Expect(string(after.Data[fakeDataKey])).To(Equal(preExistingValue))
			Expect(after.Labels).NotTo(HaveKey(managedByLabelKey))
			Expect(after.OwnerReferences).To(BeEmpty())

			// Deleting the losing SecretSync while the manager still runs must
			// clear its finalizer without deleting the unmanaged Secret.
			Expect(k8sClient.Delete(ctx, ss)).To(Succeed())
			Eventually(func() bool {
				err := k8sClient.Get(ctx, ssKey, &kvsynk8sv1alpha1.SecretSync{})
				return apierrors.IsNotFound(err)
			}, 5*time.Second, 50*time.Millisecond).Should(BeTrue())
			Expect(k8sClient.Get(ctx, targetKey, &after)).To(Succeed())
			Expect(string(after.Data[fakeDataKey])).To(Equal(preExistingValue))
		})
	})

	// Every reconcile runs under reconcileTimeout so a hung Key Vault call
	// cannot pin a worker forever (FR-008). Asserted by capturing the
	// context's deadline at the vault-read call site rather than by actually
	// hanging for a minute.
	Context("per-reconcile timeout", func() {
		It("passes the vault read a context whose deadline is at most reconcileTimeout away", func() {
			ctx := context.Background()

			name := "ss-deadline-" + shortUID()
			vaultSecret := "deadline-secret-" + shortUID()
			targetName := "target-deadline-" + shortUID()

			reader.set(fakeVaultName, vaultSecret, "SENTINEL-fake-value-deadline-not-real", "v1")
			capture := &deadlineCapturingReader{inner: reader}

			ss := newTestSecretSync(namespace, name, fakeVaultName, vaultSecret, targetName, fakeDataKey)
			Expect(k8sClient.Create(ctx, ss)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ss)
			})

			r := reconcilerFor(capture)
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}

			start := time.Now()
			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			deadline, hadDeadline := capture.captured()
			Expect(hadDeadline).To(BeTrue(), "Reconcile must bound its work with a deadline even when the incoming context has none")
			// One second of slack: Reconcile stamps the deadline a hair after
			// `start` was taken, so an exact <= start+reconcileTimeout bound
			// would flake on scheduling jitter.
			Expect(deadline).To(BeTemporally("<=", start.Add(reconcileTimeout+time.Second)),
				"the deadline must be no further than reconcileTimeout from the start of the reconcile")
		})

		// The other half of the timeout contract: a reconcile that actually
		// runs out of budget must still say so. A hung vault read consumes the
		// whole deadline, comes back as a transient failure, and leaves every
		// later call in Reconcile holding an expired context — including the
		// status write that reports the failure. Persisted on that same
		// context, the update fails instantly, Reconcile returns before the
		// Event is recorded, and since each backoff retry repeats identically
		// the CR would report its last good state, with a stale lastSyncTime
		// and no Events at all, for the entire outage (FR-009).
		It("still persists Failing/TransientError and still emits SyncFailed when the vault read eats the deadline", func() {
			ctx := context.Background()

			suffix := shortUID()
			name := "ss-hung-read-" + suffix
			vaultSecret := "hung-read-secret-" + suffix
			targetName := "target-hung-read-" + suffix
			const sentinel = "SENTINEL-fake-value-hung-read-not-real"

			reader.set(fakeVaultName, vaultSecret, sentinel, "v1")

			ss := newTestSecretSync(namespace, name, fakeVaultName, vaultSecret, targetName, fakeDataKey)
			Expect(k8sClient.Create(ctx, ss)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ss)
			})
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: targetName, Namespace: namespace},
				})
			})

			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}

			// Seed a healthy sync first, so the state the CR must move off is
			// a real InSync rather than the empty initial one — that is what
			// makes the staleness observable.
			_, err := reconcilerFor(reader).Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			var seeded kvsynk8sv1alpha1.SecretSync
			Expect(k8sClient.Get(ctx, req.NamespacedName, &seeded)).To(Succeed())
			Expect(seeded.Status.State).To(Equal(kvsynk8sv1alpha1.SecretSyncStateInSync))

			recorder := events.NewFakeRecorder(10)
			hung := reconcilerFor(deadlineConsumingReader{})
			hung.Recorder = recorder

			// Reconcile derives its deadline with context.WithTimeout, which
			// keeps whichever bound is tighter — so a short-lived incoming
			// context reproduces a minute-long hang in a second, without the
			// test having to wait out reconcileTimeout.
			hungCtx, cancelHung := context.WithTimeout(ctx, time.Second)
			defer cancelHung()
			_, err = hung.Reconcile(hungCtx, req)

			// A transient failure is returned for backoff (constitution II),
			// so an error here is expected — but it must be THAT error, from
			// the end of a reconcile that completed its bookkeeping, not a
			// failed status update short-circuiting everything after it.
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("retrying with backoff"))

			var after kvsynk8sv1alpha1.SecretSync
			Expect(k8sClient.Get(ctx, req.NamespacedName, &after)).To(Succeed())
			Expect(after.Status.State).To(Equal(kvsynk8sv1alpha1.SecretSyncStateFailing))
			Expect(after.Status.Reason).To(Equal(syncpkg.ReasonTransientError))

			var evt string
			Eventually(recorder.Events).Should(Receive(&evt))
			Expect(evt).To(ContainSubstring("SyncFailed"))
			Expect(evt).To(ContainSubstring(syncpkg.ReasonTransientError))
			Expect(evt).NotTo(ContainSubstring(sentinel))
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

		It("rewrites the managed Secret when spec.target.dataKey changes, removing the old key", func() {
			ctx := context.Background()

			name := "ss-datakey-" + shortUID()
			vaultName := fakeVaultName
			vaultSecret := fakeSecretName
			targetName := "target-" + shortUID()
			targetKey := types.NamespacedName{Name: targetName, Namespace: namespace}
			const fakeValue = "SENTINEL-fake-value-datakey-test-not-real"

			reader.set(vaultName, vaultSecret, fakeValue, "v1")

			ss := newTestSecretSync(namespace, name, vaultName, vaultSecret, targetName, "foo")
			Expect(k8sClient.Create(ctx, ss)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ss)
			})

			r := reconcilerFor(reader)
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}

			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			var secret corev1.Secret
			Expect(k8sClient.Get(ctx, targetKey, &secret)).To(Succeed())
			Expect(string(secret.Data["foo"])).To(Equal(fakeValue))

			// The user edits the spec: same vault secret, new data key. The
			// vault version has NOT changed, so only the data comparison can
			// notice that a rewrite is needed.
			var latest kvsynk8sv1alpha1.SecretSync
			Expect(k8sClient.Get(ctx, req.NamespacedName, &latest)).To(Succeed())
			latest.Spec.Target.DataKey = "bar"
			Expect(k8sClient.Update(ctx, &latest)).To(Succeed())

			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, targetKey, &secret)).To(Succeed())
			Expect(string(secret.Data["bar"])).To(Equal(fakeValue), "the new data key must carry the value")
			Expect(secret.Data).NotTo(HaveKey("foo"), "the old data key must be removed")

			var updated kvsynk8sv1alpha1.SecretSync
			Expect(k8sClient.Get(ctx, req.NamespacedName, &updated)).To(Succeed())
			Expect(updated.Status.State).To(Equal(kvsynk8sv1alpha1.SecretSyncStateInSync))
			Expect(updated.Status.ObservedGeneration).To(Equal(updated.Generation))
		})

		It("restores the vault value when the managed Secret's data is edited in-cluster, via the Owns() watch (US3 AS-2)", func() {
			ctx := context.Background()

			name := "ss-datadrift-" + shortUID()
			vaultName := fakeVaultName
			vaultSecret := fakeSecretName
			targetName := "target-" + shortUID()
			dataKey := fakeDataKey
			targetKey := types.NamespacedName{Name: targetName, Namespace: namespace}
			const fakeValue = "SENTINEL-fake-value-datadrift-test-not-real"

			mgrReader := newFakeSecretReader()
			mgrReader.set(vaultName, vaultSecret, fakeValue, "v1")

			ss := newTestSecretSync(namespace, name, vaultName, vaultSecret, targetName, dataKey)
			Expect(k8sClient.Create(ctx, ss)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ss)
			})

			// A long interval: only the Owns() watch, not periodic
			// reconciliation, should be responsible for the repair below.
			cancel := startManagerFor(mgrReader, time.Hour)
			DeferCleanup(cancel)

			var secret corev1.Secret
			Eventually(func() error {
				return k8sClient.Get(ctx, targetKey, &secret)
			}).Should(Succeed())

			// Someone edits the managed Secret's data directly (kubectl edit),
			// leaving labels and annotations -- including the version
			// annotation -- intact. Only the data comparison can see this.
			Expect(k8sClient.Get(ctx, targetKey, &secret)).To(Succeed())
			secret.Data[dataKey] = []byte("SENTINEL-tampered-value-not-real")
			Expect(k8sClient.Update(ctx, &secret)).To(Succeed())

			Eventually(func() (string, error) {
				var got corev1.Secret
				if err := k8sClient.Get(ctx, targetKey, &got); err != nil {
					return "", err
				}
				return string(got.Data[dataKey]), nil
			}, 5*time.Second, 50*time.Millisecond).Should(Equal(fakeValue),
				"the vault value must be restored via the Owns() watch without any manual reconcile")
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

	// FR-012 precedence machinery (findConflictingOwner/takesPrecedence):
	// deterministic ordering between two live claims regardless of reconcile
	// order, the defeated-claim exemption, and loser recovery once the winner
	// is gone. Each spec builds the winner's name lexically smaller than the
	// loser's and creates it first, so takesPrecedence is deterministic even
	// at envtest's one-second CreationTimestamp resolution (ties break on
	// Name).
	Context("FR-012: precedence between competing SecretSyncs", func() {
		It("fails the losing declaration even when it reconciles first, before any Secret exists", func() {
			ctx := context.Background()

			suffix := shortUID()
			targetName := "shared-prec-" + suffix
			targetKey := types.NamespacedName{Name: targetName, Namespace: namespace}
			nameA := "ss-prec-a-" + suffix // winner: created first, lexically smaller
			nameB := "ss-prec-b-" + suffix
			secretA := "prec-secret-a-" + suffix
			secretB := "prec-secret-b-" + suffix
			const valueA = "SENTINEL-fake-value-prec-winner-not-real"
			const valueB = "SENTINEL-fake-value-prec-loser-not-real"
			reader.set(fakeVaultName, secretA, valueA, "v1")
			reader.set(fakeVaultName, secretB, valueB, "v1")

			ssA := newTestSecretSync(namespace, nameA, fakeVaultName, secretA, targetName, fakeDataKey)
			Expect(k8sClient.Create(ctx, ssA)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ssA)
			})
			ssB := newTestSecretSync(namespace, nameB, fakeVaultName, secretB, targetName, fakeDataKey)
			Expect(k8sClient.Create(ctx, ssB)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ssB)
			})

			r := reconcilerFor(reader)
			reqA := reconcile.Request{NamespacedName: types.NamespacedName{Name: nameA, Namespace: namespace}}
			reqB := reconcile.Request{NamespacedName: types.NamespacedName{Name: nameB, Namespace: namespace}}

			// The LOSER reconciles first. Reconcile order must not decide
			// ownership: the CR-level precedence check has to fail B here,
			// before any Secret exists for the Create race to decide instead.
			_, err := r.Reconcile(ctx, reqB)
			Expect(err).NotTo(HaveOccurred())

			var afterB kvsynk8sv1alpha1.SecretSync
			Expect(k8sClient.Get(ctx, reqB.NamespacedName, &afterB)).To(Succeed())
			Expect(afterB.Status.State).To(Equal(kvsynk8sv1alpha1.SecretSyncStateFailing))
			Expect(afterB.Status.Reason).To(Equal(reasonTargetConflict))
			// B must never have written anything: the precedence check runs
			// before any vault read or Secret write.
			err = k8sClient.Get(ctx, targetKey, &corev1.Secret{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(),
				"the losing declaration must not create the target Secret just because it reconciled first")

			// The winner then reconciles and takes the target.
			_, err = r.Reconcile(ctx, reqA)
			Expect(err).NotTo(HaveOccurred())

			var afterA kvsynk8sv1alpha1.SecretSync
			Expect(k8sClient.Get(ctx, reqA.NamespacedName, &afterA)).To(Succeed())
			Expect(afterA.Status.State).To(Equal(kvsynk8sv1alpha1.SecretSyncStateInSync))

			var secret corev1.Secret
			Expect(k8sClient.Get(ctx, targetKey, &secret)).To(Succeed())
			Expect(string(secret.Data[fakeDataKey])).To(Equal(valueA))
			Expect(secret.OwnerReferences).To(ContainElement(HaveField("Name", nameA)))

			// B reconciling again changes nothing.
			_, err = r.Reconcile(ctx, reqB)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, reqB.NamespacedName, &afterB)).To(Succeed())
			Expect(afterB.Status.State).To(Equal(kvsynk8sv1alpha1.SecretSyncStateFailing))
			Expect(afterB.Status.Reason).To(Equal(reasonTargetConflict))
			Expect(k8sClient.Get(ctx, targetKey, &secret)).To(Succeed())
			Expect(string(secret.Data[fakeDataKey])).To(Equal(valueA))
		})

		It("ignores a defeated Failing/TargetConflict claim when deciding precedence", func() {
			ctx := context.Background()

			suffix := shortUID()
			targetName := "shared-defeated-" + suffix
			targetKey := types.NamespacedName{Name: targetName, Namespace: namespace}
			nameA := "ss-defeated-a-" + suffix // would take precedence, but is already defeated
			nameB := "ss-defeated-b-" + suffix
			secretB := "defeated-secret-b-" + suffix
			const valueB = "SENTINEL-fake-value-defeated-live-not-real"
			reader.set(fakeVaultName, secretB, valueB, "v1")

			ssA := newTestSecretSync(namespace, nameA, fakeVaultName, "defeated-secret-a-"+suffix, targetName, fakeDataKey)
			Expect(k8sClient.Create(ctx, ssA)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ssA)
			})
			ssB := newTestSecretSync(namespace, nameB, fakeVaultName, secretB, targetName, fakeDataKey)
			Expect(k8sClient.Create(ctx, ssB)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ssB)
			})

			// A already lost its claim (e.g. to a since-removed unmanaged
			// Secret): a defeated claim must not veto anyone else, or two
			// losers could deadlock each other forever.
			var defeated kvsynk8sv1alpha1.SecretSync
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nameA, Namespace: namespace}, &defeated)).To(Succeed())
			defeated.Status = kvsynk8sv1alpha1.SecretSyncStatus{
				State:              kvsynk8sv1alpha1.SecretSyncStateFailing,
				Reason:             reasonTargetConflict,
				Message:            "defeated claim, set up by the test",
				ObservedGeneration: defeated.Generation,
			}
			Expect(k8sClient.Status().Update(ctx, &defeated)).To(Succeed())

			r := reconcilerFor(reader)
			reqB := reconcile.Request{NamespacedName: types.NamespacedName{Name: nameB, Namespace: namespace}}
			_, err := r.Reconcile(ctx, reqB)
			Expect(err).NotTo(HaveOccurred())

			// Without the defeated-claim exemption, A (older, lexically
			// smaller) would take precedence and B would go Failing here.
			var afterB kvsynk8sv1alpha1.SecretSync
			Expect(k8sClient.Get(ctx, reqB.NamespacedName, &afterB)).To(Succeed())
			Expect(afterB.Status.State).To(Equal(kvsynk8sv1alpha1.SecretSyncStateInSync))

			var secret corev1.Secret
			Expect(k8sClient.Get(ctx, targetKey, &secret)).To(Succeed())
			Expect(string(secret.Data[fakeDataKey])).To(Equal(valueB))
			Expect(secret.OwnerReferences).To(ContainElement(HaveField("Name", nameB)))
		})

		It("lets the loser recover to InSync after the winning SecretSync is deleted", func() {
			ctx := context.Background()

			suffix := shortUID()
			targetName := "shared-recover-" + suffix
			targetKey := types.NamespacedName{Name: targetName, Namespace: namespace}
			nameA := "ss-recover-a-" + suffix // winner
			nameB := "ss-recover-b-" + suffix // loser, recovers
			secretA := "recover-secret-a-" + suffix
			secretB := "recover-secret-b-" + suffix
			const valueA = "SENTINEL-fake-value-recover-winner-not-real"
			const valueB = "SENTINEL-fake-value-recover-loser-not-real"
			reader.set(fakeVaultName, secretA, valueA, "v1")
			reader.set(fakeVaultName, secretB, valueB, "v1")

			ssA := newTestSecretSync(namespace, nameA, fakeVaultName, secretA, targetName, fakeDataKey)
			Expect(k8sClient.Create(ctx, ssA)).To(Succeed())
			ssB := newTestSecretSync(namespace, nameB, fakeVaultName, secretB, targetName, fakeDataKey)
			Expect(k8sClient.Create(ctx, ssB)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ssB)
			})

			r := reconcilerFor(reader)
			reqA := reconcile.Request{NamespacedName: types.NamespacedName{Name: nameA, Namespace: namespace}}
			reqB := reconcile.Request{NamespacedName: types.NamespacedName{Name: nameB, Namespace: namespace}}

			// Winner takes the target; loser goes Failing/TargetConflict.
			_, err := r.Reconcile(ctx, reqA)
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, reqB)
			Expect(err).NotTo(HaveOccurred())

			var afterB kvsynk8sv1alpha1.SecretSync
			Expect(k8sClient.Get(ctx, reqB.NamespacedName, &afterB)).To(Succeed())
			Expect(afterB.Status.State).To(Equal(kvsynk8sv1alpha1.SecretSyncStateFailing))
			Expect(afterB.Status.Reason).To(Equal(reasonTargetConflict))

			// The winner is deleted: its finalizer removes the managed Secret
			// and then the CR itself goes away.
			var winner kvsynk8sv1alpha1.SecretSync
			Expect(k8sClient.Get(ctx, reqA.NamespacedName, &winner)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &winner)).To(Succeed())
			_, err = r.Reconcile(ctx, reqA)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool {
				err := k8sClient.Get(ctx, reqA.NamespacedName, &kvsynk8sv1alpha1.SecretSync{})
				return apierrors.IsNotFound(err)
			}).Should(BeTrue(), "the winning SecretSync should be fully removed")
			err = k8sClient.Get(ctx, targetKey, &corev1.Secret{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the winner's managed Secret should be gone")

			// The loser's next reconcile finds no live competitor and no
			// occupied target: it must claim the name and reach InSync.
			_, err = r.Reconcile(ctx, reqB)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, reqB.NamespacedName, &afterB)).To(Succeed())
			Expect(afterB.Status.State).To(Equal(kvsynk8sv1alpha1.SecretSyncStateInSync))
			Expect(afterB.Status.SyncedVersion).To(Equal("v1"))

			var secret corev1.Secret
			Expect(k8sClient.Get(ctx, targetKey, &secret)).To(Succeed())
			Expect(string(secret.Data[fakeDataKey])).To(Equal(valueB))
			Expect(secret.OwnerReferences).To(ContainElement(HaveField("Name", nameB)))
		})
	})

	// FR-012, the other half of the ownership rule: a Secret carrying the
	// managed-by label but NO controller ownerReference. It is not a conflict
	// — nothing claims it — so the SecretSync adopts it. What matters is that
	// adoption really reaches the API server even when the value and the
	// version already match: that case makes every content-based idempotency
	// check false, and only the ownership check in the write gate still forces
	// the write.
	Context("FR-012: a labeled Secret with no controller owner", func() {
		It("adopts it on the first reconcile even when version and data already match", func() {
			ctx := context.Background()

			suffix := shortUID()
			name := "ss-adopt-" + suffix
			targetName := "target-adopt-" + suffix
			targetKey := types.NamespacedName{Name: targetName, Namespace: namespace}
			vaultSecret := "adopt-secret-" + suffix
			const value = "SENTINEL-fake-value-adopt-not-real"
			reader.set(fakeVaultName, vaultSecret, value, "v1")

			// Exactly what `kubectl delete secretsync --cascade=orphan`, a
			// restore from backup, or a hand-applied manifest leaves behind:
			// the managed-by label and the right content, but no
			// ownerReferences at all.
			orphan := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      targetName,
					Namespace: namespace,
					Labels:    map[string]string{managedByLabelKey: managedByLabelValue},
					Annotations: map[string]string{
						vaultAnnotationKey:   fakeVaultName,
						secretAnnotationKey:  vaultSecret,
						versionAnnotationKey: "v1",
					},
				},
				Type: corev1.SecretTypeOpaque,
				Data: map[string][]byte{fakeDataKey: []byte(value)},
			}
			Expect(k8sClient.Create(ctx, orphan)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), orphan)
			})

			ss := newTestSecretSync(namespace, name, fakeVaultName, vaultSecret, targetName, fakeDataKey)
			Expect(k8sClient.Create(ctx, ss)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(context.Background(), ss)
			})

			r := reconcilerFor(reader)
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}
			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			var after kvsynk8sv1alpha1.SecretSync
			Expect(k8sClient.Get(ctx, req.NamespacedName, &after)).To(Succeed())
			Expect(after.Status.State).To(Equal(kvsynk8sv1alpha1.SecretSyncStateInSync))

			// The point of the spec: the ownerReference must be PERSISTED, not
			// merely stamped on the in-memory copy the engine returned. Without
			// it, Owns(&corev1.Secret{}) cannot map edits of this Secret back to
			// any SecretSync, and deleting the CR would leave it behind.
			var secret corev1.Secret
			Expect(k8sClient.Get(ctx, targetKey, &secret)).To(Succeed())
			Expect(secret.OwnerReferences).To(HaveLen(1))
			Expect(secret.OwnerReferences[0].UID).To(Equal(after.UID))
			Expect(secret.OwnerReferences[0].Controller).NotTo(BeNil())
			Expect(*secret.OwnerReferences[0].Controller).To(BeTrue())
			Expect(string(secret.Data[fakeDataKey])).To(Equal(value))

			// And the adopting write happens exactly once: with ownership now
			// on record, the next reconcile is write-free (a resourceVersion
			// bump here would mean the gate writes on every pass and, with
			// Owns(), retriggers itself forever).
			settled := secret.ResourceVersion
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, targetKey, &secret)).To(Succeed())
			Expect(secret.ResourceVersion).To(Equal(settled))
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

// TestTakesPrecedence pins the deterministic ordering rule between two live
// claims on the same target name: earlier CreationTimestamp wins; ties (down
// to envtest's one-second timestamp resolution) break on Name. A plain unit
// test, no cluster needed — the envtest specs above exercise the same rule
// end-to-end through Reconcile.
func TestTakesPrecedence(t *testing.T) {
	at := func(name string, created time.Time) *kvsynk8sv1alpha1.SecretSync {
		return &kvsynk8sv1alpha1.SecretSync{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				Namespace:         "default",
				CreationTimestamp: metav1.NewTime(created),
			},
		}
	}
	t0 := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Second)

	tests := []struct {
		name string
		a, b *kvsynk8sv1alpha1.SecretSync
		want bool
	}{
		{"earlier timestamp wins", at("zzz", t0), at("aaa", t1), true},
		{"later timestamp loses", at("aaa", t1), at("zzz", t0), false},
		{"equal timestamps: smaller name wins", at("aaa", t0), at("bbb", t0), true},
		{"equal timestamps: larger name loses", at("bbb", t0), at("aaa", t0), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := takesPrecedence(tt.a, tt.b); got != tt.want {
				t.Fatalf("takesPrecedence(%s@%s, %s@%s) = %v, want %v",
					tt.a.Name, tt.a.CreationTimestamp, tt.b.Name, tt.b.CreationTimestamp, got, tt.want)
			}
		})
	}
}

// TestStatusWriteContext pins both halves of what statusWriteContext exists to
// guarantee, because either half alone is a bug.
//
// Detached, so the terminal status write outlives an exhausted reconcile
// deadline — that is the whole point of the fix, and the envtest spec above
// ("still persists Failing/TransientError ...") already fails without it.
//
// Bounded, which nothing else in the suite can observe: context.WithoutCancel
// drops the manager's shutdown cancellation along with the deadline, so a
// detached-but-unbounded status write has nothing left to interrupt it. Facing
// a wedged or throttling API server it would block forever, permanently
// costing one of the maxConcurrentReconciles workers and stalling graceful
// shutdown — strictly worse than the expired context this fix replaces.
// statusPersistTimeout is what keeps FR-008's worker protection true, so it is
// asserted here rather than left to the comment on the constant.
//
// A plain unit test, no cluster needed: both properties live in the derived
// context itself.
func TestStatusWriteContext(t *testing.T) {
	// The load-bearing case: the reconcile deadline is already gone by the
	// time the failure it caused gets written.
	t.Run("survives a parent cancelled before the write starts", func(t *testing.T) {
		parent, cancelParent := context.WithCancel(context.Background())
		cancelParent()

		ctx, cancel := statusWriteContext(parent)
		defer cancel()

		if err := ctx.Err(); err != nil {
			t.Fatalf("status write context must not inherit its parent's cancellation, got %v", err)
		}
	})

	// The same must hold for a cancellation that lands mid-write — a manager
	// shutdown, or the reconcile deadline expiring a moment later.
	t.Run("survives a parent cancelled after the write starts", func(t *testing.T) {
		parent, cancelParent := context.WithCancel(context.Background())

		ctx, cancel := statusWriteContext(parent)
		defer cancel()

		cancelParent()
		if err := ctx.Err(); err != nil {
			t.Fatalf("status write context must not follow its parent's cancellation, got %v", err)
		}
	})

	t.Run("carries a deadline of its own, no further than statusPersistTimeout", func(t *testing.T) {
		// An hour-long parent deadline: if the derived context kept it instead
		// of replacing it, the bound below catches that too.
		parent, cancelParent := context.WithTimeout(context.Background(), time.Hour)
		defer cancelParent()

		start := time.Now()
		ctx, cancel := statusWriteContext(parent)
		defer cancel()

		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("status write context must carry a deadline: detaching from the reconcile deadline also detaches from manager shutdown, so an unbounded write can pin a worker forever")
		}
		// One second of slack, like the vault-read deadline spec above: the
		// deadline is stamped a hair after `start` was taken.
		if latest := start.Add(statusPersistTimeout + time.Second); deadline.After(latest) {
			t.Fatalf("status write deadline %s is further than statusPersistTimeout (%s) from the start of the write (latest allowed %s)",
				deadline, statusPersistTimeout, latest)
		}
	})
}
