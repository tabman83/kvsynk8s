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
