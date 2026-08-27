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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// These specs exercise the CRD schema itself against the envtest API server:
// spec.target.dataKey values that apimachinery's IsConfigMapKey would reject
// on the Secret write ("." , ".." and any ".."-prefixed key) must already be
// rejected at SecretSync admission by the CEL rule on the field. Otherwise
// the CR is accepted but every Secret write fails Invalid forever.
var _ = Describe("SecretSync dataKey validation", func() {
	const namespace = "default"

	invalidKeys := []string{".", "..", "..foo"}
	for _, key := range invalidKeys {
		It("rejects dataKey "+key+" at admission", func() {
			ctx := context.Background()

			name := "ss-datakey-invalid-" + shortUID()
			ss := newTestSecretSync(namespace, name, fakeVaultName, fakeSecretName, "target-"+shortUID(), key)

			err := k8sClient.Create(ctx, ss)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid, got: %v", err)
			Expect(err.Error()).To(ContainSubstring("dataKey"))
		})
	}

	It("accepts a normal dotted dataKey like tls.crt", func() {
		ctx := context.Background()

		name := "ss-datakey-valid-" + shortUID()
		ss := newTestSecretSync(namespace, name, fakeVaultName, fakeSecretName, "target-"+shortUID(), "tls.crt")

		Expect(k8sClient.Create(ctx, ss)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, ss)
		})
	})
})

// The same principle as the dataKey specs above, applied to the target Secret
// name: spec.target.secretName has to be a real DNS-1123 subdomain, because
// that is what the API server enforces on the managed Secret's metadata.name.
// A name that passes SecretSync admission but fails the Secret write produces
// an Invalid that is not ErrTargetConflict, so Reconcile returns before the
// status update and the CR stays at Pending with an empty reason while the
// workqueue retries forever — the user sees nothing outside operator logs.
var _ = Describe("SecretSync target secretName validation", func() {
	const namespace = "default"

	// The first three are the cases the previous, looser pattern
	// (^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$) wrongly admitted: it treated "." as
	// just another interior character, so empty labels and labels edged with a
	// hyphen slipped through. The last three were already rejected and are
	// kept as regression cover.
	invalidNames := []string{"my..secret", "a.-b", "a-.b", "-abc", "abc-", "My-Secret"}
	for _, target := range invalidNames {
		It("rejects target.secretName "+target+" at admission", func() {
			ctx := context.Background()

			name := "ss-target-invalid-" + shortUID()
			ss := newTestSecretSync(namespace, name, fakeVaultName, fakeSecretName, target, "key")

			err := k8sClient.Create(ctx, ss)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid, got: %v", err)
			Expect(err.Error()).To(ContainSubstring("secretName"))
		})
	}

	validNames := []string{"my-secret", "my.secret", "a.b.c", "a"}
	for _, target := range validNames {
		It("accepts target.secretName "+target, func() {
			expectTargetAccepted(namespace, target)
		})
	}

	// A single 253-character label is legal, and that is the point of the
	// length rule living on the whole subdomain rather than on each label:
	// apimachinery caps the total only.
	It("accepts a 253-character target.secretName", func() {
		expectTargetAccepted(namespace, strings.Repeat("a", 253))
	})
})

// expectTargetAccepted creates a SecretSync with the given target.secretName
// and asserts the API server admits it, cleaning the object up afterwards.
func expectTargetAccepted(namespace, target string) {
	GinkgoHelper()

	ctx := context.Background()

	name := "ss-target-valid-" + shortUID()
	ss := newTestSecretSync(namespace, name, fakeVaultName, fakeSecretName, target, "key")

	Expect(k8sClient.Create(ctx, ss)).To(Succeed())
	DeferCleanup(func() {
		_ = k8sClient.Delete(ctx, ss)
	})
}
