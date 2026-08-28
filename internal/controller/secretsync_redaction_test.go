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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kvsynk8sv1alpha1 "github.com/tabman83/kvsynk8s/api/v1alpha1"
	"github.com/tabman83/kvsynk8s/internal/azure"
	syncpkg "github.com/tabman83/kvsynk8s/internal/sync"
)

// capturingReconcileLogger builds a real logr.Logger backed by zap at
// DebugLevel (so V(1) log calls -- the level this controller logs its
// per-reconcile line at -- are captured too), writing to an in-memory buffer
// the specs below can scan afterwards. Same pattern as capturingLogger in
// internal/sync/redaction_test.go: a real structured sink, not a hand-rolled
// double, so what the buffer holds is exactly what a production zap sink
// would have emitted.
func capturingReconcileLogger() (logr.Logger, *bytes.Buffer) {
	return capturingReconcileLoggerAt(zapcore.DebugLevel)
}

// capturingReconcileLoggerAt is the same sink at a caller-chosen level.
// InfoLevel is what production zap actually runs at, and a spec asserting that
// an operator can SEE something must capture at that level: at DebugLevel
// everything is captured, so such an assertion would pass even for a line
// nobody in production would ever get.
func capturingReconcileLoggerAt(level zapcore.Level) (logr.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(buf), level)
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

		// The Ready condition has to be readable in this window too: a tool
		// waiting on it (kubectl wait, Argo CD, Flux) must see Unknown, not the
		// absence of a condition, before the first attempt finishes. The read
		// is safe here because the reconcile is still parked on the gate.
		var pending kvsynk8sv1alpha1.SecretSync
		Expect(k8sClient.Get(baseCtx, req.NamespacedName, &pending)).To(Succeed())
		notYet := meta.FindStatusCondition(pending.Status.Conditions, kvsynk8sv1alpha1.ConditionReady)
		Expect(notYet).NotTo(BeNil())
		Expect(notYet.Status).To(Equal(metav1.ConditionUnknown))
		Expect(notYet.Reason).To(Equal("Pending"))

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

	// The runtime counterpart to the static AST check in
	// internal/sync/redaction_test.go, for a surface that check cannot cover:
	// metrics.go contains no log call at all, so it is deliberately absent
	// from TestValueCarryingSources_NeverLogValueIdentifiers. What matters
	// here is the label VALUES a real sync actually produces. A /metrics
	// endpoint is scraped by anything on the cluster network and retained for
	// months, so it must carry neither the value nor the identifiers that say
	// which vault secret this is -- the closed, compile-time label vocabulary
	// in metrics.go is what guarantees that, and this spec is what notices if
	// someone ever adds a per-object label to it.
	It("never puts a value or an object identifier into a metric label (constitution I)", func() {
		ctx := context.Background()

		suffix := shortUID()
		okName := "ss-metric-redact-" + suffix
		okSecret := "metric-redact-secret-" + suffix
		okTarget := "target-metric-redact-" + suffix
		failName := "ss-metric-redact-fail-" + suffix
		failSecret := "metric-redact-fail-secret-" + suffix
		const sentinel = "SENTINEL-controller-metric-redaction-not-real"

		reader := newFakeSecretReader()
		reader.set(fakeVaultName, okSecret, sentinel, "v1")

		okSS := newTestSecretSync(namespace, okName, fakeVaultName, okSecret, okTarget, fakeDataKey)
		Expect(k8sClient.Create(ctx, okSS)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), okSS)
		})
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: okTarget, Namespace: namespace},
			})
		})

		// Never registered with the reader, so this one fails with
		// SecretNotFound and drives the failure labels too.
		failSS := newTestSecretSync(
			namespace, failName, fakeVaultName, failSecret, "target-metric-redact-fail-"+suffix, fakeDataKey)
		Expect(k8sClient.Create(ctx, failSS)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), failSS)
		})

		// SetupWithManager is what does this in production. Called directly so
		// the spec does not depend on some other container having started a
		// manager first: Ginkgo randomises the order of top-level containers.
		registerSyncMetrics(k8sClient)

		r := &SecretSyncReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Reader: reader}
		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: okName, Namespace: namespace}})
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: failName, Namespace: namespace}})
		Expect(err).NotTo(HaveOccurred())

		families, err := metrics.Registry.Gather()
		Expect(err).NotTo(HaveOccurred())

		// The value in both of its encodings, plus every identifier that would
		// say WHICH secret a series is about.
		forbidden := append(leakForms(sentinel),
			fakeVaultName, okSecret, failSecret, namespace, okName, failName, okTarget)

		seen := map[string]bool{}
		for _, mf := range families {
			if !strings.HasPrefix(mf.GetName(), "kvsynk8s_") {
				continue
			}
			seen[mf.GetName()] = true
			for _, m := range mf.GetMetric() {
				for _, label := range m.GetLabel() {
					for _, leak := range forbidden {
						Expect(label.GetName()).NotTo(ContainSubstring(leak),
							"metric %q carries a forbidden label name", mf.GetName())
						Expect(label.GetValue()).NotTo(ContainSubstring(leak),
							"metric %q label %q carries a forbidden value", mf.GetName(), label.GetName())
					}
				}
			}
		}
		// Non-vacuity: the walk above proves nothing if the reconciles left no
		// kvsynk8s series behind to walk.
		Expect(seen).To(HaveKey("kvsynk8s_sync_total"))
		Expect(seen).To(HaveKey("kvsynk8s_secretsync_state"))
	})

	// A classified failure returns err == nil, so controller-runtime logs
	// nothing for it and the V(1) trace at the end of Reconcile is invisible at
	// production verbosity. Before recordSyncOutcome logged, that left an
	// operator tailing logs with no sign at all that a SecretSync was broken --
	// everything lived in status and in an Event. Captured at InfoLevel on
	// purpose: at DebugLevel this spec would pass even if the lines went back
	// to being V(1).
	It("logs the failure and the recovery at production verbosity, the recovery exactly once", func() {
		baseCtx := context.Background()

		suffix := shortUID()
		name := "ss-failing-log-" + suffix
		vaultSecret := "failing-log-secret-" + suffix
		targetName := "target-failing-log-" + suffix
		const sentinel = "SENTINEL-controller-recovery-log-not-real"

		// Empty to begin with: the first reconcile cannot read the vault.
		reader := newFakeSecretReader()

		ss := newTestSecretSync(namespace, name, fakeVaultName, vaultSecret, targetName, fakeDataKey)
		Expect(k8sClient.Create(baseCtx, ss)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), ss)
		})
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: targetName, Namespace: namespace},
			})
		})

		logger, logBuf := capturingReconcileLoggerAt(zapcore.InfoLevel)
		logCtx := logf.IntoContext(baseCtx, logger)

		r := &SecretSyncReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Reader: reader}
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}

		_, err := r.Reconcile(logCtx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(logBuf.String()).To(ContainSubstring("SecretSync is failing"))
		Expect(logBuf.String()).To(ContainSubstring(syncpkg.ReasonSecretNotFound),
			"the log line has to say WHY, or it sends the operator to kubectl anyway")
		// Proof the capture really is at production level: the per-reconcile
		// V(1) trace must not be in this buffer.
		Expect(logBuf.String()).NotTo(ContainSubstring("reconciled SecretSync"))

		// The vault answers again.
		reader.set(fakeVaultName, vaultSecret, sentinel, "v1")
		_, err = r.Reconcile(logCtx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Count(logBuf.String(), "SecretSync recovered")).To(Equal(1))

		// The recovery is a transition, not a state: a healthy SecretSync must
		// not re-announce it on every periodic reconcile for the rest of its
		// life.
		_, err = r.Reconcile(logCtx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Count(logBuf.String(), "SecretSync recovered")).To(Equal(1),
			"only the transition out of Failing is a recovery")

		for _, form := range leakForms(sentinel) {
			Expect(logBuf.String()).NotTo(ContainSubstring(form),
				"the secret value must not reach the operator log in any encoding")
		}
	})
})
