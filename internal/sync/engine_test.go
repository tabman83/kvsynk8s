// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

// Package sync will hold the sync engine (T012) that turns a SecretSync
// declaration into a Kubernetes Secret. This file is written first per
// constitution IV / tasks.md T009 (TDD): it defines and exercises the
// engine's contract before any implementation exists, so `go build ./...`
// and `go test ./...` are expected to fail with "undefined" errors on the
// identifiers below until T011/T012 land.
//
// ASSUMED ENGINE API (contract for the implementer of engine.go / writer.go):
//
//	type Engine struct {
//	    Reader azure.SecretReader // internal/azure/keyvault.go
//	}
//
//	func (e *Engine) Sync(
//	    ctx context.Context,
//	    owner *kvsynk8sv1alpha1.SecretSync,
//	    existing *corev1.Secret, // nil if the target Secret does not exist yet
//	) (kvsynk8sv1alpha1.SecretSyncStatus, *corev1.Secret, error)
//
// Sync computes desired state; it performs no Kubernetes API I/O itself
// (that is the reconciler's job, T013 — it calls client.Create/Update with
// the returned *corev1.Secret when non-nil). It DOES call Reader.GetLatest,
// since the latest Key Vault version is required to decide in-sync vs. not.
//
// Target defaulting (data-model.md): the output Secret's namespace is always
// owner.Namespace; its name defaults to owner.Name when
// owner.Spec.Target.SecretName is empty; the data key defaults to
// owner.Spec.Vault.Secret when owner.Spec.Target.DataKey is empty.
//
// Returned secret shape when a (re)write is needed ("Managed Kubernetes
// Secret" in data-model.md):
//   - Type: corev1.SecretTypeOpaque
//   - Labels[LabelManagedBy] == LabelManagedByValue
//   - Annotations[AnnotationVault]   == owner.Spec.Vault.Name
//   - Annotations[AnnotationSecret]  == owner.Spec.Vault.Secret
//   - Annotations[AnnotationVersion] == the Key Vault version just fetched
//   - exactly one OwnerReference, Controller: true, pointing at owner
//   - Data[dataKey] == the fetched value (only place a value is ever stored)
//
// Behavior rules under test here:
//
//  1. No existing Secret -> Reader succeeds -> Sync returns a freshly built
//     Secret per the shape above and status {State: InSync, SyncedVersion:
//     <version>, ObservedGeneration: owner.Generation}.
//
//  2. Existing Secret already carries LabelManagedBy and its
//     Annotations[AnnotationVersion] already equals the latest Key Vault
//     version -> idempotent skip (FR-005): Sync returns the SAME *corev1.Secret
//     pointer unchanged (no write needed) and status State: InSync. This must
//     hold even if Reader's value differs from what's already stored — the
//     skip decision is made on the version annotation alone, never by
//     re-comparing values.
//
//  3. Existing Secret does NOT carry LabelManagedBy (pre-existing, unmanaged
//     resource) -> Sync MUST NOT touch it (FR-012): returns the existing
//     Secret unchanged and status {State: Failing, Reason: ReasonTargetConflict}.
//
//  4. Reader.GetLatest fails wrapping azure.ErrSecretNotFound -> Sync returns
//     status {State: Failing, Reason: ReasonSecretNotFound} and does not
//     fabricate a Secret to write (nil, since none existed). err itself is
//     nil: expected, classifiable failure modes are reported via status, not
//     via the error return (a Failing SecretSync is not a Go error). No
//     secret value appears anywhere in the status (constitution I).
//
// Assumed exported identifiers used by these tests (to be added alongside
// Engine in engine.go, or in a small shared consts file in this package):
//
//	const LabelManagedBy      = "app.kubernetes.io/managed-by"
//	const LabelManagedByValue = "kvsynk8s"
//	const AnnotationVault     = "kvsynk8s.io/vault"
//	const AnnotationSecret    = "kvsynk8s.io/secret"
//	const AnnotationVersion   = "kvsynk8s.io/version"
//	const ReasonSecretNotFound = "SecretNotFound"
//	const ReasonTargetConflict = "TargetConflict"
//
// (data-model.md lists further reasons — AccessDenied, SourceDeleted,
// SourceDisabled, TransientError — not exercised by this file; T022 covers
// SourceDeleted/SourceDisabled specifically.)
package sync

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kvsynk8sv1alpha1 "github.com/tabman83/kvsynk8s/api/v1alpha1"
	"github.com/tabman83/kvsynk8s/internal/azure"
)

// sentinelValue looks like a real secret so a leak into status would be an
// obvious test failure, but per constitution I it must never appear in any
// status field, error, or log this package produces.
const sentinelValue = "SENTINEL-do-not-log-me-6f1c"

// fakeSecretReader is a hand-written stand-in for azure.SecretReader
// (internal/azure/keyvault.go) — no mocking framework, per T009.
type fakeSecretReader struct {
	value   string
	version string
	err     error
}

func (f *fakeSecretReader) GetLatest(_ context.Context, _, _ string) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	return f.value, f.version, nil
}

// newOwner builds a minimal SecretSync CR for use as Sync's owner argument.
// name is always "my-sync" today; it stays a real parameter since callers
// build distinct owners and future tests will vary it.
//
//nolint:unparam // see comment above
func newOwner(name, namespace, vaultName, secretName string) *kvsynk8sv1alpha1.SecretSync {
	return &kvsynk8sv1alpha1.SecretSync{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  namespace,
			UID:        types.UID("test-uid-" + name),
			Generation: 1,
		},
		Spec: kvsynk8sv1alpha1.SecretSyncSpec{
			Vault: kvsynk8sv1alpha1.VaultSpec{
				Name:   vaultName,
				Secret: secretName,
			},
		},
	}
}

func TestSync_CreatesSecretWithLabelsAnnotationsOwnerReference_WhenNoneExists(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")
	const value = "s3cr3t-value-v1"
	reader := &fakeSecretReader{value: value, version: "v1"}
	e := &Engine{Reader: reader}

	status, secret, err := e.Sync(context.Background(), owner, nil)
	if err != nil {
		t.Fatalf("Sync() error = %v, want nil", err)
	}

	if status.State != kvsynk8sv1alpha1.SecretSyncStateInSync {
		t.Errorf("status.State = %q, want %q", status.State, kvsynk8sv1alpha1.SecretSyncStateInSync)
	}
	if status.SyncedVersion != "v1" {
		t.Errorf("status.SyncedVersion = %q, want %q", status.SyncedVersion, "v1")
	}
	if status.ObservedGeneration != owner.Generation {
		t.Errorf("status.ObservedGeneration = %d, want %d", status.ObservedGeneration, owner.Generation)
	}
	if strings.Contains(status.Message, value) || strings.Contains(status.Reason, value) {
		t.Fatalf("secret value leaked into status: message=%q reason=%q", status.Message, status.Reason)
	}

	if secret == nil {
		t.Fatal("secret = nil, want a created Secret")
	}
	if secret.Name != owner.Name {
		t.Errorf("secret.Name = %q, want default of owner.Name %q", secret.Name, owner.Name)
	}
	if secret.Namespace != owner.Namespace {
		t.Errorf("secret.Namespace = %q, want %q", secret.Namespace, owner.Namespace)
	}
	if secret.Type != corev1.SecretTypeOpaque {
		t.Errorf("secret.Type = %q, want %q", secret.Type, corev1.SecretTypeOpaque)
	}
	if got := secret.Labels[LabelManagedBy]; got != LabelManagedByValue {
		t.Errorf("secret.Labels[%q] = %q, want %q", LabelManagedBy, got, LabelManagedByValue)
	}
	if got := secret.Annotations[AnnotationVault]; got != owner.Spec.Vault.Name {
		t.Errorf("secret.Annotations[%q] = %q, want %q", AnnotationVault, got, owner.Spec.Vault.Name)
	}
	if got := secret.Annotations[AnnotationSecret]; got != owner.Spec.Vault.Secret {
		t.Errorf("secret.Annotations[%q] = %q, want %q", AnnotationSecret, got, owner.Spec.Vault.Secret)
	}
	if got := secret.Annotations[AnnotationVersion]; got != "v1" {
		t.Errorf("secret.Annotations[%q] = %q, want %q", AnnotationVersion, got, "v1")
	}

	if len(secret.OwnerReferences) != 1 {
		t.Fatalf("len(secret.OwnerReferences) = %d, want 1", len(secret.OwnerReferences))
	}
	ownerRef := secret.OwnerReferences[0]
	if ownerRef.Kind != "SecretSync" || ownerRef.Name != owner.Name || ownerRef.UID != owner.UID {
		t.Errorf("ownerRef = %+v, want Kind=SecretSync Name=%q UID=%q", ownerRef, owner.Name, owner.UID)
	}
	if ownerRef.Controller == nil || !*ownerRef.Controller {
		t.Error("ownerRef.Controller = nil/false, want true")
	}

	// dataKey defaults to the vault secret name (data-model.md).
	got, ok := secret.Data[owner.Spec.Vault.Secret]
	if !ok {
		t.Fatalf("secret.Data[%q] missing, keys present: %v", owner.Spec.Vault.Secret, secret.Data)
	}
	if string(got) != value {
		t.Errorf("secret.Data[%q] = %q, want %q", owner.Spec.Vault.Secret, got, value)
	}
}

func TestSync_SkipsWriteWhenVersionAnnotationMatches_Idempotent(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")

	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      owner.Name,
			Namespace: owner.Namespace,
			Labels: map[string]string{
				LabelManagedBy: LabelManagedByValue,
			},
			Annotations: map[string]string{
				AnnotationVault:   owner.Spec.Vault.Name,
				AnnotationSecret:  owner.Spec.Vault.Secret,
				AnnotationVersion: "v3",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			owner.Spec.Vault.Secret: []byte("already-stored-value"),
		},
	}

	// Reader reports the SAME version but a DIFFERENT value: the skip
	// decision must be made on the version annotation alone, never by
	// comparing values (which would require reading them unnecessarily).
	reader := &fakeSecretReader{value: "a-different-value-should-not-be-written", version: "v3"}
	e := &Engine{Reader: reader}

	status, secret, err := e.Sync(context.Background(), owner, existing)
	if err != nil {
		t.Fatalf("Sync() error = %v, want nil", err)
	}

	if status.State != kvsynk8sv1alpha1.SecretSyncStateInSync {
		t.Errorf("status.State = %q, want %q", status.State, kvsynk8sv1alpha1.SecretSyncStateInSync)
	}
	if status.SyncedVersion != "v3" {
		t.Errorf("status.SyncedVersion = %q, want %q", status.SyncedVersion, "v3")
	}

	if secret != existing {
		t.Fatalf("Sync() returned a different *Secret on an idempotent skip; want the same existing pointer, unwritten")
	}
	if string(secret.Data[owner.Spec.Vault.Secret]) != "already-stored-value" {
		t.Fatalf("existing Secret data was modified on an idempotent skip: %q", secret.Data[owner.Spec.Vault.Secret])
	}
}

func TestSync_RefusesUnmanagedExistingSecret_TargetConflict(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")

	// Pre-existing Secret with the target name, created by something other
	// than kvsynk8s: no managed-by label.
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      owner.Name,
			Namespace: owner.Namespace,
			Labels:    map[string]string{"app": "someone-else"},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"unrelated-key": []byte("unrelated-value")},
	}

	reader := &fakeSecretReader{value: sentinelValue, version: "v1"}
	e := &Engine{Reader: reader}

	status, secret, err := e.Sync(context.Background(), owner, existing)
	if err != nil {
		t.Fatalf("Sync() error = %v, want nil", err)
	}

	if status.State != kvsynk8sv1alpha1.SecretSyncStateFailing {
		t.Errorf("status.State = %q, want %q", status.State, kvsynk8sv1alpha1.SecretSyncStateFailing)
	}
	if status.Reason != ReasonTargetConflict {
		t.Errorf("status.Reason = %q, want %q", status.Reason, ReasonTargetConflict)
	}
	if strings.Contains(status.Message, sentinelValue) {
		t.Fatalf("secret value leaked into status.Message: %q", status.Message)
	}

	if secret != existing {
		t.Fatalf("Sync() must leave an unmanaged existing Secret completely untouched (same pointer)")
	}
	if string(secret.Data["unrelated-key"]) != "unrelated-value" {
		t.Fatalf("unmanaged Secret data was modified: %q", secret.Data["unrelated-key"])
	}
	if _, ok := secret.Data[owner.Spec.Vault.Secret]; ok {
		t.Fatalf("unmanaged Secret must not gain the vault's data key, found %q", owner.Spec.Vault.Secret)
	}
}

func TestSync_ReaderNotFound_FailingSecretNotFound_NoValueAnywhere(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "does-not-exist")

	notFoundErr := fmt.Errorf("vault %q secret %q: %w", owner.Spec.Vault.Name, owner.Spec.Vault.Secret, azure.ErrSecretNotFound)
	reader := &fakeSecretReader{err: notFoundErr}
	e := &Engine{Reader: reader}

	status, secret, err := e.Sync(context.Background(), owner, nil)
	if err != nil {
		t.Fatalf("Sync() error = %v, want nil (expected failures are reported via status)", err)
	}

	if status.State != kvsynk8sv1alpha1.SecretSyncStateFailing {
		t.Errorf("status.State = %q, want %q", status.State, kvsynk8sv1alpha1.SecretSyncStateFailing)
	}
	if status.Reason != ReasonSecretNotFound {
		t.Errorf("status.Reason = %q, want %q", status.Reason, ReasonSecretNotFound)
	}

	// No secret existed before, and none should be fabricated on failure.
	if secret != nil {
		t.Fatalf("secret = %+v, want nil (nothing to write on SecretNotFound with no prior Secret)", secret)
	}

	// Constitution I: assert no value substring anywhere reachable from the
	// result, not just the obvious Message field.
	haystacks := []string{status.Message, status.Reason, status.SyncedVersion, fmt.Sprintf("%+v", status)}
	for _, h := range haystacks {
		if strings.Contains(h, sentinelValue) {
			t.Fatalf("sentinel value leaked into status output: %q", h)
		}
	}
}
