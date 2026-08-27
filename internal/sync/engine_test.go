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
//  2. Existing Secret already carries LabelManagedBy, its
//     Annotations[AnnotationVersion] already equals the latest Key Vault
//     version, AND its data is exactly the desired shape (the single resolved
//     dataKey holding the fetched value) -> idempotent skip (FR-005): Sync
//     returns the SAME *corev1.Secret pointer unchanged (no write needed) and
//     status State: InSync. A matching version annotation alone is NOT
//     enough: a changed spec.target.dataKey, or in-cluster drift of the
//     managed Secret's data, must be repaired on the next reconcile even
//     though the annotation still matches (US3 drift repair, FR-007).
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
	if ownerRef.Kind != secretSyncKind || ownerRef.Name != owner.Name || ownerRef.UID != owner.UID {
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

func TestSync_SkipsWriteWhenVersionAndDataMatch_Idempotent(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")

	const storedValue = "already-stored-value"
	existing := existingSyncedSecret(owner, "v3", storedValue)

	// Reader reports the SAME version and the SAME value the Secret already
	// carries: nothing to repair, so the skip path must return the existing
	// pointer completely untouched.
	reader := &fakeSecretReader{value: storedValue, version: "v3"}
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
	if string(secret.Data[owner.Spec.Vault.Secret]) != storedValue {
		t.Fatalf("existing Secret data was modified on an idempotent skip: %q", secret.Data[owner.Spec.Vault.Secret])
	}
}

// TestSync_RewritesWhenDataKeyChanges_RemovesStaleKey covers a spec edit that
// the version annotation cannot see: the user changes spec.target.dataKey
// (here foo -> bar) while the vault secret itself is unchanged. Sync must
// rewrite the Secret so that the new key carries the value and the old key is
// gone, and report InSync with the new generation observed (FR-007; the old
// version-only idempotency gate skipped this forever).
func TestSync_RewritesWhenDataKeyChanges_RemovesStaleKey(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")
	owner.Spec.Target.DataKey = "bar"
	owner.Generation = 2

	const value = "same-vault-value"
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
			// The controller ownerReference a prior successful sync always
			// leaves behind. Without it `managed` is false and the
			// idempotency gate short-circuits before comparing the data,
			// which would make this test pass no matter what
			// managedSecretDataMatches returns — the exact comparison it
			// exists to protect.
			OwnerReferences: []metav1.OwnerReference{controllerOwnerReference(owner)},
		},
		Type: corev1.SecretTypeOpaque,
		// Written under the previous spec's dataKey "foo".
		Data: map[string][]byte{"foo": []byte(value)},
	}

	// Same version, same value: only the desired data key changed.
	reader := &fakeSecretReader{value: value, version: "v3"}
	e := &Engine{Reader: reader}

	status, secret, err := e.Sync(context.Background(), owner, existing)
	if err != nil {
		t.Fatalf("Sync() error = %v, want nil", err)
	}

	if status.State != kvsynk8sv1alpha1.SecretSyncStateInSync {
		t.Errorf("status.State = %q, want %q", status.State, kvsynk8sv1alpha1.SecretSyncStateInSync)
	}
	if status.ObservedGeneration != 2 {
		t.Errorf("status.ObservedGeneration = %d, want 2", status.ObservedGeneration)
	}

	if secret == nil {
		t.Fatal("secret = nil, want a rewritten Secret")
	}
	if string(secret.Data["bar"]) != value {
		t.Errorf("secret.Data[%q] = %q, want %q", "bar", secret.Data["bar"], value)
	}
	if _, ok := secret.Data["foo"]; ok {
		t.Errorf("stale data key %q must be removed on rewrite, still present: %v", "foo", secret.Data)
	}
	if len(secret.Data) != 1 {
		t.Errorf("len(secret.Data) = %d, want exactly 1 key", len(secret.Data))
	}
}

// TestSync_RewritesWhenManagedSecretDataDrifted covers US3 AS-2 at the engine
// level: someone edits the managed Secret's data in-cluster (kubectl edit),
// leaving labels and annotations intact. The version annotation still matches
// the vault, so the old version-only idempotency gate declared it InSync and
// the drift was never repaired; Sync must now detect the mismatch and rewrite
// the vault value.
func TestSync_RewritesWhenManagedSecretDataDrifted(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")
	// A stale prior status.syncedVersion must not survive a successful sync:
	// the carried-forward value is overwritten with the version just synced.
	owner.Status.SyncedVersion = "v2"

	const vaultValue = "the-real-vault-value"
	existing := existingSyncedSecret(owner, "v3", "tampered-in-cluster-value")

	reader := &fakeSecretReader{value: vaultValue, version: "v3"}
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

	if secret == nil {
		t.Fatal("secret = nil, want a rewritten Secret")
	}
	if got := string(secret.Data[owner.Spec.Vault.Secret]); got != vaultValue {
		t.Errorf("secret.Data[%q] = %q, want the vault value %q restored", owner.Spec.Vault.Secret, got, vaultValue)
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

// TestSync_ManagedSecretOwnedByAnotherSecretSync_TargetConflict covers the
// ownership half of FR-012 at the engine level: a Secret that carries the
// managed-by label but whose controller ownerReference points at a DIFFERENT
// SecretSync. The reader is set up to return exactly the version and value
// the Secret already holds — the configuration that used to sail through the
// idempotent InSync skip on the label alone, letting a conflict loser report
// InSync over a Secret it does not own. Sync must instead classify it as
// Failing/TargetConflict (matching the writer's AlreadyOwnedError mapping)
// and leave the Secret untouched.
func TestSync_ManagedSecretOwnedByAnotherSecretSync_TargetConflict(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")
	otherOwner := newOwner("other-sync", "default", "my-vault", "my-app-password")

	const storedValue = "value-owned-by-the-other-secretsync"
	existing := existingSyncedSecret(owner, "v3", storedValue)
	// Same name, same label, same annotations — but the controller owner is
	// the OTHER SecretSync.
	existing.OwnerReferences = []metav1.OwnerReference{controllerOwnerReference(otherOwner)}

	// Matching version AND matching data: without the ownership check this is
	// precisely the idempotent-skip configuration.
	reader := &fakeSecretReader{value: storedValue, version: "v3"}
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
	if status.SyncedVersion != "" {
		t.Errorf("status.SyncedVersion = %q, want empty: a conflict loser never synced anything", status.SyncedVersion)
	}
	if strings.Contains(status.Message, storedValue) {
		t.Fatalf("secret value leaked into status.Message: %q", status.Message)
	}

	if secret != existing {
		t.Fatalf("Sync() must leave another SecretSync's Secret completely untouched (same pointer)")
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].UID != otherOwner.UID {
		t.Fatalf("the other SecretSync's ownerReference was modified: %+v", secret.OwnerReferences)
	}
	if string(secret.Data[owner.Spec.Vault.Secret]) != storedValue {
		t.Fatalf("the other SecretSync's Secret data was modified: %q", secret.Data[owner.Spec.Vault.Secret])
	}
}

// TestSync_LabeledOwnerlessSecret_TakesWritePathNotSkip locks in the deliberate
// middle ground: a Secret with the managed-by label but NO controller
// ownerReference at all is not a conflict (the writer adopts it on the write
// path, as it always has), but it must never take the no-write InSync skip
// either — the engine cannot prove this SecretSync owns it, so it must be
// re-written, which (re)stamps this owner's controller reference.
func TestSync_LabeledOwnerlessSecret_TakesWritePathNotSkip(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")

	const storedValue = "labeled-but-ownerless-value"
	existing := existingSyncedSecret(owner, "v3", storedValue)
	existing.OwnerReferences = nil

	// Matching version and data: only the missing ownerReference distinguishes
	// this from a legitimate idempotent skip.
	reader := &fakeSecretReader{value: storedValue, version: "v3"}
	e := &Engine{Reader: reader}

	status, secret, err := e.Sync(context.Background(), owner, existing)
	if err != nil {
		t.Fatalf("Sync() error = %v, want nil", err)
	}

	if status.State != kvsynk8sv1alpha1.SecretSyncStateInSync {
		t.Errorf("status.State = %q, want %q", status.State, kvsynk8sv1alpha1.SecretSyncStateInSync)
	}
	if secret == nil {
		t.Fatal("secret = nil, want a rewritten Secret")
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].UID != owner.UID {
		t.Fatalf("the write path must stamp this owner's controller reference, got %+v", secret.OwnerReferences)
	}
}

func TestControllerOwnedBy(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")
	otherOwner := newOwner("other-sync", "default", "my-vault", "my-app-password")
	isController := true
	notController := false

	tests := []struct {
		name string
		refs []metav1.OwnerReference
		want bool
	}{
		{"no owner references", nil, false},
		{"controller reference to this owner", []metav1.OwnerReference{controllerOwnerReference(owner)}, true},
		{"controller reference to another owner", []metav1.OwnerReference{controllerOwnerReference(otherOwner)}, false},
		{"non-controller reference to this owner", []metav1.OwnerReference{{
			APIVersion: kvsynk8sv1alpha1.GroupVersion.String(), Kind: secretSyncKind,
			Name: owner.Name, UID: owner.UID, Controller: &notController,
		}}, false},
		{"controller is other, this owner only non-controller", []metav1.OwnerReference{
			{
				APIVersion: kvsynk8sv1alpha1.GroupVersion.String(), Kind: secretSyncKind,
				Name: owner.Name, UID: owner.UID, Controller: &notController,
			},
			{
				APIVersion: kvsynk8sv1alpha1.GroupVersion.String(), Kind: secretSyncKind,
				Name: otherOwner.Name, UID: otherOwner.UID, Controller: &isController,
			},
		}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{OwnerReferences: tt.refs}}
			if got := ControllerOwnedBy(secret, owner); got != tt.want {
				t.Fatalf("ControllerOwnedBy() = %v, want %v", got, tt.want)
			}
		})
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
	// This CR never synced: there is no version to carry forward, so the
	// carried-forward SyncedVersion must simply stay empty.
	if status.SyncedVersion != "" {
		t.Errorf("status.SyncedVersion = %q, want empty (never synced)", status.SyncedVersion)
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

// T022 (US3, FR-013): once a SecretSync has already synced a version
// successfully, the vault reporting that same secret as deleted or disabled
// on a later reconcile must NOT be treated like TestSync_ReaderNotFound above
// (which covers "never synced" — nothing exists to preserve). Here `existing`
// already carries a synced version annotation, so Sync must go Failing with
// SourceDeleted/SourceDisabled while leaving the previously-written Secret
// completely untouched: same pointer, same Data, same annotations. This is
// "keep last known good" — the whole point of FR-013 is that a vault-side
// deletion/disable must never itself blank out or delete the Secret that
// workloads are already reading from.

// existingSyncedSecret builds a managed Secret that looks like the result of
// a prior successful Sync for owner at the given version/value — including
// the controller OwnerReference the writer always sets, which is what the
// engine's ownership check (and the `managed` classification) requires.
func existingSyncedSecret(owner *kvsynk8sv1alpha1.SecretSync, version, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      owner.Name,
			Namespace: owner.Namespace,
			Labels: map[string]string{
				LabelManagedBy: LabelManagedByValue,
			},
			Annotations: map[string]string{
				AnnotationVault:   owner.Spec.Vault.Name,
				AnnotationSecret:  owner.Spec.Vault.Secret,
				AnnotationVersion: version,
			},
			OwnerReferences: []metav1.OwnerReference{controllerOwnerReference(owner)},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			owner.Spec.Vault.Secret: []byte(value),
		},
	}
}

func TestSync_ReaderReportsDeleted_FailingSourceDeleted_ExistingSecretUntouched(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")
	owner.Status.SyncedVersion = "v2"
	const lastKnownGood = "SENTINEL-last-known-good-value-not-real"
	existing := existingSyncedSecret(owner, "v2", lastKnownGood)

	deletedErr := fmt.Errorf("vault %q secret %q: %w", owner.Spec.Vault.Name, owner.Spec.Vault.Secret, azure.ErrSecretNotFound)
	reader := &fakeSecretReader{err: deletedErr}
	e := &Engine{Reader: reader}

	status, secret, err := e.Sync(context.Background(), owner, existing)
	if err != nil {
		t.Fatalf("Sync() error = %v, want nil (expected failures are reported via status)", err)
	}
	if status.State != kvsynk8sv1alpha1.SecretSyncStateFailing {
		t.Errorf("status.State = %q, want %q", status.State, kvsynk8sv1alpha1.SecretSyncStateFailing)
	}
	if status.Reason != ReasonSourceDeleted {
		t.Errorf("status.Reason = %q, want %q", status.Reason, ReasonSourceDeleted)
	}
	// FR-013: the managed Secret keeps its last value, so the version the
	// status reports must keep saying which one that is, not go blank.
	if status.SyncedVersion != "v2" {
		t.Errorf("status.SyncedVersion = %q, want %q carried forward on SourceDeleted", status.SyncedVersion, "v2")
	}

	if secret != existing {
		t.Fatalf("Sync() must return the SAME existing Secret pointer on SourceDeleted (keep-last-known-good, FR-013)")
	}
	if string(secret.Data[owner.Spec.Vault.Secret]) != lastKnownGood {
		t.Fatalf("existing Secret data was mutated on SourceDeleted: got %q, want unchanged %q",
			secret.Data[owner.Spec.Vault.Secret], lastKnownGood)
	}
	if secret.Annotations[AnnotationVersion] != "v2" {
		t.Fatalf("existing Secret's version annotation was mutated on SourceDeleted: got %q, want unchanged %q",
			secret.Annotations[AnnotationVersion], "v2")
	}

	haystacks := []string{status.Message, status.Reason, fmt.Sprintf("%+v", status)}
	for _, h := range haystacks {
		if strings.Contains(h, lastKnownGood) {
			t.Fatalf("secret value leaked into status output: %q", h)
		}
	}
}

// A transient reader failure is the flap case: the vault secret is fine, the
// read just failed this cycle. The prior synced version must survive in the
// status instead of blinking blank until the next successful reconcile.
func TestSync_ReaderTransientError_SyncedVersionCarriedForward(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")
	owner.Status.SyncedVersion = "v7"
	const lastKnownGood = "SENTINEL-last-known-good-value-transient-not-real"
	existing := existingSyncedSecret(owner, "v7", lastKnownGood)

	transientErr := fmt.Errorf("vault %q secret %q: %w", owner.Spec.Vault.Name, owner.Spec.Vault.Secret, azure.ErrTransient)
	reader := &fakeSecretReader{err: transientErr}
	e := &Engine{Reader: reader}

	status, secret, err := e.Sync(context.Background(), owner, existing)
	if err != nil {
		t.Fatalf("Sync() error = %v, want nil (expected failures are reported via status)", err)
	}
	if status.State != kvsynk8sv1alpha1.SecretSyncStateFailing {
		t.Errorf("status.State = %q, want %q", status.State, kvsynk8sv1alpha1.SecretSyncStateFailing)
	}
	if status.Reason != ReasonTransientError {
		t.Errorf("status.Reason = %q, want %q", status.Reason, ReasonTransientError)
	}
	if status.SyncedVersion != "v7" {
		t.Errorf("status.SyncedVersion = %q, want %q carried forward on TransientError", status.SyncedVersion, "v7")
	}
	if secret != existing {
		t.Fatalf("Sync() must return the SAME existing Secret pointer on TransientError")
	}
}

func TestSync_ReaderReportsDisabled_FailingSourceDisabled_ExistingSecretUntouched(t *testing.T) {
	owner := newOwner("my-sync", "default", "my-vault", "my-app-password")
	owner.Status.SyncedVersion = "v5"
	const lastKnownGood = "SENTINEL-last-known-good-value-disabled-not-real"
	existing := existingSyncedSecret(owner, "v5", lastKnownGood)

	disabledErr := fmt.Errorf("vault %q secret %q: %w", owner.Spec.Vault.Name, owner.Spec.Vault.Secret, azure.ErrSecretDisabled)
	reader := &fakeSecretReader{err: disabledErr}
	e := &Engine{Reader: reader}

	status, secret, err := e.Sync(context.Background(), owner, existing)
	if err != nil {
		t.Fatalf("Sync() error = %v, want nil (expected failures are reported via status)", err)
	}
	if status.State != kvsynk8sv1alpha1.SecretSyncStateFailing {
		t.Errorf("status.State = %q, want %q", status.State, kvsynk8sv1alpha1.SecretSyncStateFailing)
	}
	if status.Reason != ReasonSourceDisabled {
		t.Errorf("status.Reason = %q, want %q", status.Reason, ReasonSourceDisabled)
	}
	// FR-013, same as SourceDeleted: the Secret still holds v5's value, the
	// status must keep reporting v5.
	if status.SyncedVersion != "v5" {
		t.Errorf("status.SyncedVersion = %q, want %q carried forward on SourceDisabled", status.SyncedVersion, "v5")
	}

	if secret != existing {
		t.Fatalf("Sync() must return the SAME existing Secret pointer on SourceDisabled (keep-last-known-good, FR-013)")
	}
	if string(secret.Data[owner.Spec.Vault.Secret]) != lastKnownGood {
		t.Fatalf("existing Secret data was mutated on SourceDisabled: got %q, want unchanged %q",
			secret.Data[owner.Spec.Vault.Secret], lastKnownGood)
	}
	if secret.Annotations[AnnotationVersion] != "v5" {
		t.Fatalf("existing Secret's version annotation was mutated on SourceDisabled: got %q, want unchanged %q",
			secret.Annotations[AnnotationVersion], "v5")
	}

	haystacks := []string{status.Message, status.Reason, fmt.Sprintf("%+v", status)}
	for _, h := range haystacks {
		if strings.Contains(h, lastKnownGood) {
			t.Fatalf("secret value leaked into status output: %q", h)
		}
	}
}
