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

// This test lives here rather than in internal/events, even though it is
// entirely about events.Listener, because it needs a real manager with
// leader election enabled -- and that needs envtest's real apiserver
// (coordination.k8s.io Leases are not something a fake client models
// meaningfully). internal/controller already owns the envtest suite and
// already builds managers in its tests (startManagerFor in
// secretsync_controller_test.go). Giving internal/events an envtest
// dependency for one spec would break the property CLAUDE.md documents --
// that internal/events runs under plain `go test` with no KUBEBUILDER_ASSETS
// -- so the manager-construction machinery stays here and imports
// internal/events instead (specs/003-single-replica-invariant/research.md R4).

import (
	"context"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/event"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/tabman83/kvsynk8s/internal/azure"
	kvevents "github.com/tabman83/kvsynk8s/internal/events"
)

// recordingQueueSource is a minimal azure.QueueSource stand-in: it only
// counts Receive calls, which is all this spec needs to assert on. It never
// returns a message, so it never exercises handleMessage.
type recordingQueueSource struct {
	mu       sync.Mutex
	receives int
}

func (q *recordingQueueSource) Receive(_ context.Context, _ int32) ([]azure.QueueMessage, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.receives++
	return nil, nil
}

func (q *recordingQueueSource) Delete(_ context.Context, _, _ string) error { return nil }

func (q *recordingQueueSource) receiveCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.receives
}

var _ azure.QueueSource = (*recordingQueueSource)(nil)

// This spec proves the contract in
// specs/003-single-replica-invariant/contracts/runnable-leadership.md: a
// Listener must not run on a manager instance that does not hold leadership,
// because nothing on such an instance drains the events channel it writes
// to. It fails against events.Listener.NeedLeaderElection() returning false
// (the listener starts immediately, in the non-leader-election runnable
// group, and polls its queue) and passes once it returns true (the listener
// is placed in the leader-election group and never starts, because the
// Lease below is held by someone else for the whole test).
var _ = Describe("Listener leadership", func() {
	It("does not poll the queue on an instance that never becomes leader", func() {
		const namespace = "default" // matches the fixed envtest namespace this suite's other specs use
		leaseName := "kvsynk8s-listener-leadership-test"
		leaseNamespace := namespace

		By("pre-creating a Lease held by a different identity, so this manager never becomes leader")
		durationSeconds := int32(3600)
		now := metav1.NewMicroTime(time.Now())
		lease := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: leaseName, Namespace: leaseNamespace},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       ptr("someone-else"),
				LeaseDurationSeconds: &durationSeconds,
				AcquireTime:          &now,
				RenewTime:            &now,
			},
		}
		Expect(k8sClient.Create(ctx, lease)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Delete(ctx, lease)).To(Succeed())
		})

		By("starting a manager with leader election enabled and a Listener registered")
		skipNameValidation := true
		renewDeadline := 2 * time.Second
		retryPeriod := 500 * time.Millisecond
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                  scheme.Scheme,
			Metrics:                 metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress:  "0",
			LeaderElection:          true,
			LeaderElectionID:        leaseName,
			LeaderElectionNamespace: leaseNamespace,
			RenewDeadline:           &renewDeadline,
			RetryPeriod:             &retryPeriod,
			Controller: config.Controller{
				SkipNameValidation: &skipNameValidation,
			},
		})
		Expect(err).NotTo(HaveOccurred())

		queue := &recordingQueueSource{}
		listener := kvevents.NewListener(queue, mgr.GetClient(), make(chan event.GenericEvent, 1))
		Expect(mgr.Add(listener)).To(Succeed())

		mgrCtx, cancelMgr := context.WithCancel(ctx)
		DeferCleanup(cancelMgr)
		go func() {
			defer GinkgoRecover()
			_ = mgr.Start(mgrCtx)
		}()
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())

		By("giving the manager time to try (and fail) to acquire leadership")
		Consistently(queue.receiveCount, 3*time.Second, 250*time.Millisecond).Should(Equal(0),
			"the listener must not poll its queue on an instance that never holds leadership")
	})
})

func ptr[T any](v T) *T { return &v }
