// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

// T025 (US4, constitution I, FR-010, SC-004): this suite's whole purpose is
// to try to catch a secret-value leak, not to assert the happy path. It
// drives every path through Engine.Sync and SecretWriter.CreateOrUpdate that
// is reachable without a real cluster (success, every classified Key Vault
// reader error, and TargetConflict), plants an obviously-fake
// SENTINEL-... value on each one, and then scans EVERY output the sync
// package can produce for that sentinel: status.Reason, status.Message,
// status.SyncedVersion, a %+v dump of the whole status struct, any error
// string returned, and (via a real logr sink wired into ctx, exactly as
// production code would receive one from the manager) every log line
// emitted during the call. Only Secret.Data -- the one field whose entire
// job is to carry the value -- is allowed to contain it.
//
// The engine and writer packages do no logging of their own as of US1-US3;
// the captured-log assertions here are deliberately kept even though they
// currently see zero log lines, so that if T028 (or any future change) adds
// a log call to engine.go or writer.go that references a value by mistake,
// this suite fails immediately instead of relying on a human noticing.
package sync

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kvsynk8sv1alpha1 "github.com/tabman83/kvsynk8s/api/v1alpha1"
	"github.com/tabman83/kvsynk8s/internal/azure"
)

// capturingLogger builds a real logr.Logger backed by zap, writing to an
// in-memory buffer this test can scan afterwards. It stands in for whatever
// logr sink controller-runtime would install in production (zap, per
// cmd/main.go and the *_test.go suites' own BeforeSuite calls) -- using a
// real structured sink rather than a hand-rolled one so a future log.Info
// call added to engine.go/writer.go is captured exactly as it would be in
// production, not bypassed by a test double that doesn't behave like logr.
func capturingLogger() (logr.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(buf), zapcore.DebugLevel)
	return zapr.NewLogger(zap.New(core)), buf
}

// leakForms returns every textual shape the sentinel can take in structured
// log output. The plain string is the obvious one; the base64 form is the
// non-obvious one and the reason this helper exists: zapr hands a []byte (and
// a map[string][]byte, such as Secret.Data) to zap.Any, which encodes it as
// base64 in JSON. A leak of the raw bytes of a secret value therefore does
// NOT contain the sentinel as plain text, and a naive ContainSubstring check
// would pass while the operator log holds a trivially decodable copy of the
// value.
func leakForms(sentinel string) []string {
	return []string{sentinel, base64.StdEncoding.EncodeToString([]byte(sentinel))}
}

// assertNoLeakInLog fails t if any form of sentinel (see leakForms) appears in
// the captured log buffer.
func assertNoLeakInLog(t *testing.T, label, sentinel string, logBuf *bytes.Buffer) {
	t.Helper()
	logs := logBuf.String()
	for _, form := range leakForms(sentinel) {
		if strings.Contains(logs, form) {
			t.Fatalf("%s: sentinel leaked into captured log output (as %q): %s", label, form, logs)
		}
	}
}

// assertNoLeak fails t if any form of sentinel appears anywhere in haystacks
// or in the captured log buffer. label identifies which scenario is being
// checked, for a useful failure message.
func assertNoLeak(t *testing.T, label, sentinel string, logBuf *bytes.Buffer, haystacks ...string) {
	t.Helper()
	for i, h := range haystacks {
		for _, form := range leakForms(sentinel) {
			if strings.Contains(h, form) {
				t.Fatalf("%s: sentinel leaked into output[%d] (as %q): %q", label, i, form, h)
			}
		}
	}
	assertNoLeakInLog(t, label, sentinel, logBuf)
}

// dumpStatus renders every field of status as text, the same way a human or
// a log call might (fmt %+v), so assertNoLeak has one haystack that would
// catch a leak into any field this test doesn't check by name individually.
func dumpStatus(status kvsynk8sv1alpha1.SecretSyncStatus) string {
	return fmt.Sprintf("%+v", status)
}

// redactionScheme builds the minimal runtime.Scheme the fake client needs for
// the SecretWriter scenarios below.
func redactionScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := kvsynk8sv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add SecretSync types to scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core/v1 types to scheme: %v", err)
	}
	return scheme
}

// TestRedaction_Sync_Success_ValueOnlyInSecretData covers the happy path
// (InSync): the sentinel MUST end up in the returned Secret's Data (that is
// the entire point of this tool) but MUST NOT appear anywhere in status, nor
// in any log line produced while Sync ran.
func TestRedaction_Sync_Success_ValueOnlyInSecretData(t *testing.T) {
	const sentinel = "SENTINEL-redaction-success-6f2b9a"
	owner := newOwner("redact-success", "default", "redact-vault", "redact-secret")
	reader := &fakeSecretReader{value: sentinel, version: "v1"}
	e := &Engine{Reader: reader}

	logger, logBuf := capturingLogger()
	ctx := logf.IntoContext(context.Background(), logger)

	status, secret, err := e.Sync(ctx, owner, nil)
	if err != nil {
		t.Fatalf("Sync() error = %v, want nil", err)
	}

	assertNoLeak(t, "success", sentinel, logBuf, status.Reason, status.Message, status.SyncedVersion, dumpStatus(status))

	if secret == nil || string(secret.Data[owner.Spec.Vault.Secret]) != sentinel {
		t.Fatalf("expected the sentinel to land in Secret.Data (the one allowed place); got %+v", secret)
	}
}

// TestRedaction_Sync_ReaderErrors_NoValueAnywhere drives every classified
// SecretReader error (not found, access denied, disabled, transient, and an
// unclassified error) through Sync with a sentinel value planted on the fake
// reader's error itself (a realistic case: an upstream error message could
// in principle echo request/response content this package has not audited),
// and asserts the sentinel never surfaces in status or logs.
func TestRedaction_Sync_ReaderErrors_NoValueAnywhere(t *testing.T) {
	const sentinel = "SENTINEL-redaction-reader-error-2c918d"

	cases := []struct {
		name       string
		wrap       error
		wantReason string
	}{
		{"not-found", azure.ErrSecretNotFound, ReasonSecretNotFound},
		{"access-denied", azure.ErrAccessDenied, ReasonAccessDenied},
		{"disabled", azure.ErrSecretDisabled, ReasonSourceDisabled},
		{"transient", azure.ErrTransient, ReasonTransientError},
		{"unclassified", nil, ReasonTransientError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner := newOwner("redact-"+tc.name, "default", "redact-vault", "redact-secret")

			// The sentinel is embedded directly in the error string returned
			// by the reader -- simulating an upstream error that happens to
			// echo something sensitive-looking -- to prove classifyReaderError
			// never propagates err's own text into status (engine.go already
			// documents this; this test is what proves it).
			var readerErr error
			if tc.wrap != nil {
				readerErr = fmt.Errorf("upstream said: %s: %w", sentinel, tc.wrap)
			} else {
				readerErr = fmt.Errorf("upstream said: %s", sentinel)
			}
			reader := &fakeSecretReader{err: readerErr}
			e := &Engine{Reader: reader}

			logger, logBuf := capturingLogger()
			ctx := logf.IntoContext(context.Background(), logger)

			status, secret, err := e.Sync(ctx, owner, nil)
			if err != nil {
				t.Fatalf("Sync() error = %v, want nil (expected failures are reported via status)", err)
			}
			if status.State != kvsynk8sv1alpha1.SecretSyncStateFailing {
				t.Errorf("status.State = %q, want Failing", status.State)
			}
			if status.Reason != tc.wantReason {
				t.Errorf("status.Reason = %q, want %q", status.Reason, tc.wantReason)
			}
			if secret != nil {
				t.Errorf("secret = %+v, want nil (nothing existed before, nothing should be fabricated)", secret)
			}

			assertNoLeak(t, tc.name, sentinel, logBuf, status.Reason, status.Message, status.SyncedVersion, dumpStatus(status))
		})
	}
}

// TestRedaction_Sync_TargetConflict_ReaderNeverCalled proves the strongest
// possible form of redaction for the conflict path: the vault is never even
// read, so a sentinel value can't leak because it never enters this
// package's memory in the first place (FR-012 checked before any vault I/O).
func TestRedaction_Sync_TargetConflict_ReaderNeverCalled(t *testing.T) {
	const sentinel = "SENTINEL-redaction-conflict-8a41f0"
	owner := newOwner("redact-conflict", "default", "redact-vault", "redact-secret")

	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      owner.Name,
			Namespace: owner.Namespace,
			Labels:    map[string]string{"app": "someone-else"},
		},
		Data: map[string][]byte{"unrelated": []byte("unrelated-value")},
	}

	// If Sync ever called the reader despite the conflict, this sentinel
	// would be sitting right there ready to leak -- proving the "reader
	// never called" claim by making a leak trivially detectable if the
	// ordering ever regresses.
	reader := &fakeSecretReader{value: sentinel, version: "v1"}
	e := &Engine{Reader: reader}

	logger, logBuf := capturingLogger()
	ctx := logf.IntoContext(context.Background(), logger)

	status, secret, err := e.Sync(ctx, owner, existing)
	if err != nil {
		t.Fatalf("Sync() error = %v, want nil", err)
	}
	if status.State != kvsynk8sv1alpha1.SecretSyncStateFailing || status.Reason != ReasonTargetConflict {
		t.Fatalf("status = %+v, want Failing/TargetConflict", status)
	}
	if secret != existing {
		t.Fatalf("Sync() must return the same existing pointer, untouched, on TargetConflict")
	}

	assertNoLeak(t, "conflict", sentinel, logBuf, status.Reason, status.Message, status.SyncedVersion, dumpStatus(status))
}

// TestRedaction_Writer_CreateOrUpdate_Success_ValueOnlyInSecretData exercises
// the ONLY code path in the whole codebase allowed to place a secret value
// into a Kubernetes object (constitution I) via a fake client, and asserts
// the sentinel never leaks into a returned error or a log line -- only into
// the persisted Secret's Data field.
func TestRedaction_Writer_CreateOrUpdate_Success_ValueOnlyInSecretData(t *testing.T) {
	const sentinel = "SENTINEL-redaction-writer-success-3d77c1"
	owner := newOwner("redact-writer-ok", "default", "redact-vault", "redact-secret")
	cli := fake.NewClientBuilder().WithScheme(redactionScheme(t)).Build()
	w := &SecretWriter{Client: cli}

	logger, logBuf := capturingLogger()
	ctx := logf.IntoContext(context.Background(), logger)

	if err := w.CreateOrUpdate(ctx, owner, owner.Namespace, owner.Name, "value", sentinel, "v1"); err != nil {
		t.Fatalf("CreateOrUpdate() error = %v, want nil", err)
	}

	assertNoLeakInLog(t, "writer-create", sentinel, logBuf)

	var got corev1.Secret
	if err := cli.Get(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, &got); err != nil {
		t.Fatalf("get written secret: %v", err)
	}
	if string(got.Data["value"]) != sentinel {
		t.Fatalf("expected the sentinel in the written Secret's Data; got %+v", got.Data)
	}
}

// TestRedaction_Writer_CreateOrUpdate_Update_ValueOnlyInSecretData exercises
// the Update branch of CreateOrUpdate specifically (an already-managed
// Secret gets a new vault value written on top of it -- the path taken by
// every version bump on an already-synced SecretSync, per
// internal/controller/secretsync_controller_test.go's "converges to a
// changed vault value" case). The two tests above only ever call
// CreateOrUpdate once against an empty fake client, so they only ever
// exercise the create() branch and its log.Info call; this test's whole
// purpose is to also drive the *second* log.Info call, at the end of the
// Update branch, under a real capturing logger, so a value leak planted in
// that specific call would be caught.
func TestRedaction_Writer_CreateOrUpdate_Update_ValueOnlyInSecretData(t *testing.T) {
	const initialValue = "non-sentinel-initial-value"
	const sentinel = "SENTINEL-redaction-writer-update-7b3f1e"
	owner := newOwner("redact-writer-update", "default", "redact-vault", "redact-secret")
	cli := fake.NewClientBuilder().WithScheme(redactionScheme(t)).Build()
	w := &SecretWriter{Client: cli}

	// Seed an already-managed Secret via CreateOrUpdate itself (so it is
	// shaped exactly like real prior-sync output), taking the create()
	// branch. This first call is not the one under test.
	seedLogger, _ := capturingLogger()
	seedCtx := logf.IntoContext(context.Background(), seedLogger)
	if err := w.CreateOrUpdate(seedCtx, owner, owner.Namespace, owner.Name, "value", initialValue, "v1"); err != nil {
		t.Fatalf("seed CreateOrUpdate() error = %v, want nil", err)
	}

	// Now call CreateOrUpdate again with a new sentinel and version against
	// that same managed Secret: existing.Labels[LabelManagedBy] already
	// equals LabelManagedByValue, so this drives the Update branch (the
	// Get/populateManagedSecret/Update/log.Info sequence), under a fresh
	// capturing logger scoped to only this call.
	logger, logBuf := capturingLogger()
	ctx := logf.IntoContext(context.Background(), logger)

	if err := w.CreateOrUpdate(ctx, owner, owner.Namespace, owner.Name, "value", sentinel, "v2"); err != nil {
		t.Fatalf("update CreateOrUpdate() error = %v, want nil", err)
	}

	assertNoLeakInLog(t, "writer-update", sentinel, logBuf)

	var got corev1.Secret
	if err := cli.Get(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, &got); err != nil {
		t.Fatalf("get updated secret: %v", err)
	}
	if string(got.Data["value"]) != sentinel {
		t.Fatalf("expected the sentinel in the updated Secret's Data; got %+v", got.Data)
	}
}

// TestRedaction_Writer_CreateOrUpdate_TargetConflict_NoValueInError plants
// the sentinel as the value CreateOrUpdate is asked to write into a Secret
// that already exists unmanaged, and asserts the returned error -- which
// callers may well log verbatim, unlike status.Message -- never contains it.
func TestRedaction_Writer_CreateOrUpdate_TargetConflict_NoValueInError(t *testing.T) {
	const sentinel = "SENTINEL-redaction-writer-conflict-9f0e2a"
	owner := newOwner("redact-writer-conflict", "default", "redact-vault", "redact-secret")

	unmanaged := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: owner.Name, Namespace: owner.Namespace},
		Data:       map[string][]byte{"unrelated": []byte("unrelated-value")},
	}
	cli := fake.NewClientBuilder().WithScheme(redactionScheme(t)).WithObjects(unmanaged).Build()
	w := &SecretWriter{Client: cli}

	logger, logBuf := capturingLogger()
	ctx := logf.IntoContext(context.Background(), logger)

	err := w.CreateOrUpdate(ctx, owner, owner.Namespace, owner.Name, "value", sentinel, "v1")
	if err == nil {
		t.Fatal("CreateOrUpdate() error = nil, want ErrTargetConflict")
	}

	assertNoLeak(t, "writer-conflict", sentinel, logBuf, err.Error())

	var got corev1.Secret
	if getErr := cli.Get(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, &got); getErr != nil {
		t.Fatalf("get unmanaged secret: %v", getErr)
	}
	if _, ok := got.Data["value"]; ok {
		t.Fatalf("unmanaged Secret must not have gained the vault's data key: %+v", got.Data)
	}
}

// TestRedaction_Writer_CreateOrUpdate_TargetImmutable_NoValueInError is the
// TargetConflict test above, for the other terminal error path: the target is
// a managed but frozen Secret, so CreateOrUpdate refuses the write and returns
// ErrTargetImmutable. Every error path this package can return has to be in
// this suite or the redaction contract has a hole — the writer holds the
// plaintext in a local when it builds that error, and the caller may well log
// it verbatim, unlike status.Message.
func TestRedaction_Writer_CreateOrUpdate_TargetImmutable_NoValueInError(t *testing.T) {
	const sentinel = "SENTINEL-redaction-writer-immutable-4c86b3"
	owner := newOwner("redact-writer-immutable", "default", "redact-vault", "redact-secret")

	frozen := true
	managed := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      owner.Name,
			Namespace: owner.Namespace,
			Labels:    map[string]string{LabelManagedBy: LabelManagedByValue},
			Annotations: map[string]string{
				AnnotationVault:   owner.Spec.Vault.Name,
				AnnotationSecret:  owner.Spec.Vault.Secret,
				AnnotationVersion: "v1",
			},
		},
		Type:      corev1.SecretTypeOpaque,
		Immutable: &frozen,
		Data:      map[string][]byte{"value": []byte("non-sentinel-frozen-value")},
	}
	cli := fake.NewClientBuilder().WithScheme(redactionScheme(t)).WithObjects(managed).Build()
	w := &SecretWriter{Client: cli, Reader: cli}

	logger, logBuf := capturingLogger()
	ctx := logf.IntoContext(context.Background(), logger)

	err := w.CreateOrUpdate(ctx, owner, owner.Namespace, owner.Name, "value", sentinel, "v2")
	if err == nil {
		t.Fatal("CreateOrUpdate() error = nil, want ErrTargetImmutable")
	}

	assertNoLeak(t, "writer-immutable", sentinel, logBuf, err.Error())

	var got corev1.Secret
	if getErr := cli.Get(ctx, client.ObjectKey{Namespace: owner.Namespace, Name: owner.Name}, &got); getErr != nil {
		t.Fatalf("get immutable secret: %v", getErr)
	}
	if string(got.Data["value"]) != "non-sentinel-frozen-value" {
		t.Fatalf("frozen Secret's data must not have been rewritten: %+v", got.Data)
	}
}

// TestValueCarryingSources_NeverLogValueIdentifiers is a static check (T028):
// it parses each value-carrying source file into an AST and inspects every
// call whose selector is Info, Error, or WithValues (the
// .Info(/.Error(/.V(n).Info()/.WithValues() log call shapes used in these
// files) for an argument that references one of that file's value-carrying
// local identifiers -- the ones that hold, or contain, the plaintext secret
// value. A future PR that adds, say, `log.Info("wrote secret", "value",
// value)` to writer.go, or `log.WithValues("data", desired.Data)` to the
// controller, will fail this test the moment it's added, which is the point.
//
// Covered files: writer.go (the only code path allowed to place a value into
// a Kubernetes object) and internal/controller/secretsync_controller.go
// (which materializes the plaintext value -- `value :=
// string(desired.Data[dataKey])` -- right next to live log calls).
//
// A value-carrying identifier may still be logged through a field that
// provably cannot carry a value (`secret.Name`, `secret.Namespace`): those
// are listed per identifier as safeFields, and only those exact field
// selections are allowed. Anything else off the identifier -- the bare
// identifier, `secret.Data`, an index expression -- fails.
//
// Honest scope: this is AST-based rather than a line-by-line string scan, so
// a call whose arguments are wrapped across multiple lines (gofmt does this
// once a call has enough key/value pairs) is still parsed as one
// *ast.CallExpr and every argument is inspected. It is NOT a proof of
// non-leakage: it only inspects Info/Error/WithValues selector calls for the
// listed identifiers, so a value copied into a differently-named variable
// first, or smuggled through a helper such as fmt.Sprintf into an error that
// is logged later, would not be caught here. The runtime log-capture tests in
// this file and in the controller package are the complementary check for
// those shapes -- and they scan for both the plain and the base64 form of the
// sentinel, because zapr encodes a []byte or a map[string][]byte as base64.
func TestValueCarryingSources_NeverLogValueIdentifiers(t *testing.T) {
	logSelectors := map[string]bool{"Info": true, "Error": true, "WithValues": true}

	// noSafeFields means every use of that identifier inside a log call
	// argument is a violation.
	noSafeFields := map[string]bool(nil)

	cases := []struct {
		path string
		// valueIdents maps each identifier in that file that holds or
		// contains a plaintext secret value to the field selections off it
		// that are allowed to reach a log call. Everything else fails.
		valueIdents map[string]map[string]bool
	}{
		{
			path: "writer.go",
			valueIdents: map[string]map[string]bool{
				"value": noSafeFields, "existing": noSafeFields, "secret": noSafeFields,
			},
		},
		{
			path: filepath.Join("..", "controller", "secretsync_controller.go"),
			// `value` and `desired` hold the plaintext directly; `existing`,
			// `fetched`, and `priorData` hold the target Secret's data. The
			// loop variable `secret` in deleteStaleManagedSecrets is a whole
			// managed Secret, so it carries the value in .Data -- it is
			// listed too, with only the identity fields it legitimately logs
			// today allowed through.
			valueIdents: map[string]map[string]bool{
				"value": noSafeFields, "desired": noSafeFields, "existing": noSafeFields,
				"fetched": noSafeFields, "priorData": noSafeFields,
				"secret": {"Name": true, "Namespace": true},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			src, err := readSourceFile(t, tc.path)
			if err != nil {
				t.Fatalf("read %s: %v", tc.path, err)
			}

			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, tc.path, src, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", tc.path, err)
			}

			logCall := false
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !logSelectors[sel.Sel.Name] {
					return true
				}
				logCall = true
				for _, arg := range call.Args {
					if ident, found := referencesIdent(arg, tc.valueIdents); found {
						pos := fset.Position(call.Pos())
						t.Fatalf("%s:%d: %s(...) call has an argument that references the value-carrying identifier %q",
							tc.path, pos.Line, sel.Sel.Name, ident)
					}
				}
				return true
			})
			if !logCall {
				t.Fatalf("%s: no Info/Error/WithValues calls found at all -- if logging moved elsewhere, this check must follow it", tc.path)
			}
		})
	}
}

// referencesIdent reports whether expr is, or contains as a sub-expression
// anywhere within it, a reference to one of the value-carrying identifiers in
// idents, returning the first such reference. Walking the whole
// sub-expression (rather than only checking whether expr itself is a bare
// identifier) means it also catches an identifier wrapped in a conversion, a
// call, or any other expression built from it. Matching on the parsed
// identifier name -- not a substring of the source text -- means it doesn't
// false-positive on unrelated identifiers that merely contain the same
// letters, like "dataValue" or "valueFoo".
//
// A selection listed in that identifier's safe-field set (`secret.Name`) is
// skipped, and so is anything below it; every other shape -- the bare
// identifier, `secret.Data`, `secret.Data[k]` -- is reported.
func referencesIdent(expr ast.Expr, idents map[string]map[string]bool) (string, bool) {
	var found string
	ast.Inspect(expr, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if base, ok := sel.X.(*ast.Ident); ok {
				if safeFields, listed := idents[base.Name]; listed {
					if safeFields[sel.Sel.Name] {
						return false
					}
					found = base.Name + "." + sel.Sel.Name
					return false
				}
			}
			return true
		}
		if ident, ok := n.(*ast.Ident); ok {
			if _, listed := idents[ident.Name]; listed {
				found = ident.Name
			}
			return false
		}
		return true
	})
	return found, found != ""
}

// readSourceFile reads a source file by a path relative to this package's
// own directory (tests run with the package directory as their working
// directory, so a bare relative name -- or a ../sibling-package path -- is
// enough).
func readSourceFile(t *testing.T, name string) (string, error) {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
