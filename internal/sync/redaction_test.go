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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
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

// assertNoLeak fails t if sentinel appears anywhere in haystacks or in the
// captured log buffer. label identifies which scenario is being checked, for
// a useful failure message.
func assertNoLeak(t *testing.T, label, sentinel string, logBuf *bytes.Buffer, haystacks ...string) {
	t.Helper()
	for i, h := range haystacks {
		if strings.Contains(h, sentinel) {
			t.Fatalf("%s: sentinel leaked into output[%d]: %q", label, i, h)
		}
	}
	if strings.Contains(logBuf.String(), sentinel) {
		t.Fatalf("%s: sentinel leaked into captured log output: %s", label, logBuf.String())
	}
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

	if strings.Contains(logBuf.String(), sentinel) {
		t.Fatalf("sentinel leaked into captured log output: %s", logBuf.String())
	}

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

	if strings.Contains(logBuf.String(), sentinel) {
		t.Fatalf("sentinel leaked into captured log output from the Update branch: %s", logBuf.String())
	}

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

// TestWriterSource_NeverLogsValueParameter is a static check (T028): it
// parses writer.go's own source into an AST and inspects every call whose
// selector is Info or Error (the .Info(/.Error(/.V(n).Info() log call
// shapes used in this file) for an argument that references the local
// identifier `value` -- the parameter CreateOrUpdate/create take that holds
// the secret value. A future PR that adds, say,
// `log.Info("wrote secret", "value", value)` to writer.go will fail this
// test the moment it's added, which is the point.
//
// This is AST-based rather than a line-by-line string scan specifically so
// it cannot be defeated by ordinary Go formatting: a call whose arguments
// are wrapped across multiple lines (gofmt does this once a call has enough
// key/value pairs, or an editor auto-wraps a long line) is parsed as the
// same single *ast.CallExpr regardless of how its tokens are laid out on
// screen, so every argument is still inspected.
func TestWriterSource_NeverLogsValueParameter(t *testing.T) {
	src, err := readSourceFile(t, "writer.go")
	if err != nil {
		t.Fatalf("read writer.go: %v", err)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "writer.go", src, 0)
	if err != nil {
		t.Fatalf("parse writer.go: %v", err)
	}

	logCall := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Info" && sel.Sel.Name != "Error") {
			return true
		}
		logCall = true
		for _, arg := range call.Args {
			if referencesValueIdent(arg) {
				pos := fset.Position(call.Pos())
				t.Fatalf("writer.go:%d: %s(...) call has an argument that references the `value` identifier", pos.Line, sel.Sel.Name)
			}
		}
		return true
	})
	if !logCall {
		t.Skip("writer.go currently has no log calls at all; this check has nothing to verify yet but stays in place for when T028 adds one")
	}
}

// referencesValueIdent reports whether expr is, or contains as a
// sub-expression anywhere within it, an *ast.Ident named "value". Walking
// the whole sub-expression (rather than only checking whether expr itself is
// a bare identifier) means it also catches `value` wrapped in a conversion,
// a call, or any other expression built from it. Matching on the parsed
// identifier name -- not a substring of the source text -- means it doesn't
// false-positive on unrelated identifiers that merely contain the same
// letters, like "dataValue" or "valueFoo".
func referencesValueIdent(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok && ident.Name == "value" {
			found = true
		}
		return true
	})
	return found
}

// readSourceFile reads a source file from this package's own directory
// (tests run with the package directory as their working directory, so a
// bare relative name is enough).
func readSourceFile(t *testing.T, name string) (string, error) {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
