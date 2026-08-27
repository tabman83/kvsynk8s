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
	"bytes"
	"context"
	"encoding/base64"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kvsynk8sv1alpha1 "github.com/tabman83/kvsynk8s/api/v1alpha1"
	"github.com/tabman83/kvsynk8s/internal/azure"
)

// capturingReconcileLogger builds a real logr.Logger backed by zap at
// DebugLevel (so V(1) log calls -- the level this controller logs its
// per-reconcile line at -- are captured too), writing to an in-memory buffer
// the specs below can scan afterwards. Same pattern as capturingLogger in
// internal/sync/redaction_test.go: a real structured sink, not a hand-rolled
// double, so what the buffer holds is exactly what a production zap sink
// would have emitted.
func capturingReconcileLogger() (logr.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(buf), zapcore.DebugLevel)
	return zapr.NewLogger(zap.New(core)), buf
}

// leakForms returns every textual shape the sentinel can take in structured
// log output. The plain string is the obvious one; the base64 form is the
// non-obvious one and the reason this helper exists: zapr hands a []byte (and
// a map[string][]byte, such as Secret.Data) to zap.Any, which encodes it as
// base64 in JSON. So `log.Info("...", "data", secret.Data)` -- a real leak of
// the synced value into the operator log -- would NOT put the sentinel in the
// buffer as plain text, and a check for the plain form alone would pass while
// the log held a trivially decodable copy of the value. Mirrors leakForms in
// internal/sync/redaction_test.go (duplicated rather than shared: both are
// test-only helpers in different packages).
func leakForms(sentinel string) []string {
	return []string{sentinel, base64.StdEncoding.EncodeToString([]byte(sentinel))}
}

// gatedReader wraps another azure.SecretReader and blocks every GetLatest
// call until release is closed (or the call's context ends). It lets a spec
// observe what Reconcile has persisted *before* the vault read completes --
// the only window in which the transient Pending state is the CR's current
// status.
type gatedReader struct {
	inner   azure.SecretReader
	release chan struct{}
}

func (g *gatedReader) GetLatest(ctx context.Context, vaultName, secretName string) (string, string, error) {
	select {
	case <-g.release:
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
	return g.inner.GetLatest(ctx, vaultName, secretName)
}

var _ azure.SecretReader = (*gatedReader)(nil)

var _ = Describe("SecretSync Controller: log redaction and Pending state", func() {
	const namespace = "default"

	// The controller materializes the plaintext value in Reconcile itself
	// (`value := string(desired.Data[dataKey])`) right next to live log
	// calls, so it needs its own runtime redaction coverage: the sync
	// package's tests capture engine/writer logging, but not the reconcile
	// loop's. This drives every logging path Reconcile has -- create, update
	// on a new version, the stale-target rename sweep (whose deleted Secret
	// still carries the sentinel in Data), and finalizer-driven deletion --
	// through a real capturing zap sink at DebugLevel, and asserts the
	// sentinel never reaches it.
	It("never emits the secret value through any reconcile log path (SC-004, constitution I)", func() {
		baseCtx := context.Background()

		name := "ss-redact-" + shortUID()
		vaultSecret := "redact-secret-" + shortUID()
		oldTarget := "target-redact-old-" + shortUID()
		newTarget := "target-redact-new-" + shortUID()
		const sentinelV1 = "SENTINEL-controller-redaction-v1-not-real"
		const sentinelV2 = "SENTINEL-controller-redaction-v2-not-real"

		reader := newFakeSecretReader()
		reader.set(fakeVaultName, vaultSecret, sentinelV1, "v1")

		ss := newTestSecretSync(namespace, name, fakeVaultName, vaultSecret, oldTarget, fakeDataKey)
		Expect(k8sClient.Create(baseCtx, ss)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), ss)
		})

		logger, logBuf := capturingReconcileLogger()
		logCtx := logf.IntoContext(baseCtx, logger)

		r := &SecretSyncReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Reader: reader,
		}
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}

		// Create path: first reconcile writes the sentinel into the Secret.
		_, err := r.Reconcile(logCtx, req)
		Expect(err).NotTo(HaveOccurred())

		// Update path: a new vault version with a second sentinel.
		reader.set(fakeVaultName, vaultSecret, sentinelV2, "v2")
		_, err = r.Reconcile(logCtx, req)
		Expect(err).NotTo(HaveOccurred())

		// Rename sweep: deleteStaleManagedSecrets logs while the stale
		// Secret it deletes still carries the sentinel in its Data.
		var latest kvsynk8sv1alpha1.SecretSync
		Expect(k8sClient.Get(baseCtx, req.NamespacedName, &latest)).To(Succeed())
		latest.Spec.Target.SecretName = newTarget
		Expect(k8sClient.Update(baseCtx, &latest)).To(Succeed())
		_, err = r.Reconcile(logCtx, req)
		Expect(err).NotTo(HaveOccurred())

		// Deletion path: the finalizer deletes the managed Secret.
		Expect(k8sClient.Get(baseCtx, req.NamespacedName, &latest)).To(Succeed())
		Expect(k8sClient.Delete(baseCtx, &latest)).To(Succeed())
		_, err = r.Reconcile(logCtx, req)
		Expect(err).NotTo(HaveOccurred())

		logs := logBuf.String()
		// Prove the sink actually captured this controller's own logging --
		// without this, the no-sentinel assertions below could pass vacuously
		// because nothing was wired up.
		Expect(logs).To(ContainSubstring("reconciled SecretSync"))
		Expect(logs).To(ContainSubstring("deleted stale managed secret"))
		for _, sentinel := range []string{sentinelV1, sentinelV2} {
			for _, form := range leakForms(sentinel) {
				Expect(logs).NotTo(ContainSubstring(form),
					"the secret value must not reach the operator log in any encoding")
			}
		}
	})

	// data-model.md: "(created) -> Pending" is a distinct, persisted state.
	// Reconcile writes it before the conflict check and the vault read, and
	// the sync outcome overwrites it moments later -- so observing it needs
	// the vault read held open. A gated reader blocks GetLatest while this
	// spec asserts the API server already reports Pending, then releases it
	// and asserts the reconcile still finishes InSync.
	It("persists status.state == Pending on the first reconcile, before the vault is read", func() {
		baseCtx := context.Background()

		name := "ss-pending-" + shortUID()
		vaultSecret := "pending-secret-" + shortUID()
		targetName := "target-pending-" + shortUID()

		inner := newFakeSecretReader()
		inner.set(fakeVaultName, vaultSecret, "SENTINEL-fake-value-pending-not-real", "v1")
		gate := &gatedReader{inner: inner, release: make(chan struct{})}

		ss := newTestSecretSync(namespace, name, fakeVaultName, vaultSecret, targetName, fakeDataKey)
		Expect(k8sClient.Create(baseCtx, ss)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), ss)
		})

		r := &SecretSyncReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Reader: gate,
		}
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}

		done := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			_, err := r.Reconcile(baseCtx, req)
			done <- err
		}()

		Eventually(func() (kvsynk8sv1alpha1.SecretSyncState, error) {
			var updated kvsynk8sv1alpha1.SecretSync
			if err := k8sClient.Get(baseCtx, req.NamespacedName, &updated); err != nil {
				return "", err
			}
			return updated.Status.State, nil
		}, 5*time.Second, 50*time.Millisecond).Should(Equal(kvsynk8sv1alpha1.SecretSyncStatePending),
			"a freshly-created SecretSync must report Pending while its first sync is still in flight")

		close(gate.release)

		select {
		case err := <-done:
			Expect(err).NotTo(HaveOccurred())
		case <-time.After(10 * time.Second):
			Fail("Reconcile did not finish after the vault read was released")
		}

		var updated kvsynk8sv1alpha1.SecretSync
		Expect(k8sClient.Get(baseCtx, req.NamespacedName, &updated)).To(Succeed())
		Expect(updated.Status.State).To(Equal(kvsynk8sv1alpha1.SecretSyncStateInSync),
			"Pending is transient: the completed first sync must overwrite it")
	})
})
