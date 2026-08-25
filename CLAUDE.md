# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

`kvsynk8s` — realtime sync from Azure Key Vault to Kubernetes secrets (repo: `tabman83/kvsynk8s`, default branch `master`).

## Current state

A working kubebuilder operator. All of Phases 1-8 in
`specs/001-akv-eventgrid-sync/tasks.md` are implemented except:

- T032 (partial) — the E2E suite in `test/e2e/secretsync_test.go` that
  exercises the actual sync loop (create, SC-001 queue propagation, drift
  repair, deletion, TargetConflict, log redaction) is written and was proven
  correct by hand against Azurite/Lowkey Vault, but its `Context` is gated
  behind `KVSYNK8S_E2E_EMULATORS=1` (unset by default, and not set in CI)
  because of an unresolved DNS-reachability flake — see research.md R10.
- T038 (the real-AKS validation run, which needs a live subscription and is
  done by hand, not by an agent).

## Architecture

Standard kubebuilder layout, one Go module, one controller.

- **`cmd/main.go`** — manager setup: flags/env (queue URL, reconcile
  interval, Azure client ID), wires the Key Vault reader, optionally the
  queue listener (only if a queue URL is configured), and the reconciler.
- **`api/v1alpha1/secretsync_types.go`** — the `SecretSync` CRD (spec: vault
  name/secret, optional target secret name/data key; status: state/reason/
  message/syncedVersion/lastSyncTime/observedGeneration).
- **`internal/controller/secretsync_controller.go`** — the controller-runtime
  reconcile loop: finalizer-driven cleanup on deletion, target-conflict
  detection (first writer wins), periodic `RequeueAfter` (default 4h) as the
  drift/missed-event safety net, `Owns(&corev1.Secret{})` so in-cluster
  edits/deletes of the managed Secret re-trigger a reconcile, and an optional
  `WatchesRawSource(source.Channel(...))` fed by the queue listener.
- **`internal/sync/engine.go`** — the sync engine: resolves target
  name/dataKey defaults, calls the `SecretReader`, decides the resulting
  status (`Pending`/`InSync`/`Failing` + reason). No Kubernetes API I/O of its
  own; the reconciler applies whatever it returns.
- **`internal/sync/writer.go`** — the *only* code path in the whole codebase
  that ever places a secret value into a Kubernetes object
  (`SecretWriter.CreateOrUpdate`). Every other package that needs a value
  synced goes through this file. Nothing here logs the value; a static AST
  check in `internal/sync/redaction_test.go` enforces that.
- **`internal/azure/keyvault.go`** and **`internal/azure/queue.go`** — thin
  wrappers around `azsecrets` and `azqueue`, each behind a small interface
  (`SecretReader`, `QueueSource`) so the rest of the codebase never imports
  the Azure SDK directly.
- **`charts/kvsynk8s/`** — the Helm chart, the second first-class install
  method next to `install.yaml`. Hand-maintained reviewed source, *except*
  two machine-managed regions that `make helm-sync` regenerates: the whole of
  `templates/crds/secretsync-crd.yaml` (wrapped from
  `config/crd/bases/`, gated by `crds.install`/`crds.keep`) and the rules
  between the `# BEGIN/END generated rules` markers in
  `templates/rbac/manager-clusterrole.yaml` (spliced from
  `config/rbac/role.yaml`). Never edit those two regions by hand. The default
  render is kept equivalent to `kustomize build config/default` minus the
  Namespace by `hack/compare-helm-kustomize.sh`.
- **`internal/events/`** — the Event Grid → Storage Queue path: `parser.go`
  decodes a queue message into a `(vault, secret, version)` tuple or a clean
  discard (wrong event type, wrong object type, malformed body);
  `listener.go` is a `manager.Runnable` that polls the queue with an adaptive
  delay (2s busy / 30s idle), matches events against `SecretSync` objects via
  the manager's cached client, and injects matches into the controller
  through a `source.Channel`.

**Auth.** Azure: `azidentity.DefaultAzureCredential`, i.e. Microsoft Entra
Workload ID in-cluster (falls back through the standard credential chain
locally) — no static credentials anywhere. Kubernetes: the in-cluster
ServiceAccount / kubeconfig controller-runtime already handles.

**Change propagation, in order of how a value actually reaches the cluster:**
a secret is rotated in Key Vault → Key Vault emits a
`Microsoft.KeyVault.SecretNewVersionCreated` event → Event Grid delivers it
(Base64-encoded) to an Azure Storage Queue → the listener pulls it, matches it
against `SecretSync` objects, and enqueues a reconcile request → the
reconciler re-reads the **latest** value from Key Vault (never trusting the
event's own version — latest always wins) and writes it through
`SecretWriter`. If the queue is unconfigured, misconfigured, or the event is
simply lost, the periodic reconcile (default every hour, `RequeueAfter` on the
controller) reaches the same end state on its own; the queue path only buys
speed, never correctness.

## Build, lint, test

```bash
make build              # go build -o bin/manager cmd/main.go (also regenerates manifests/deepcopy)
make test               # unit tests + envtest (real API server), all packages except test/e2e
make lint                # golangci-lint run
make test-integration    # azqueue against Azurite + azsecrets against Lowkey Vault, via testcontainers-go. Needs Docker.
make test-e2e            # scaffold checks against a kind cluster (spins the cluster up and tears it down);
                         # the SecretSync sync-loop Context is skipped unless KVSYNK8S_E2E_EMULATORS=1 (see T032 above)
make helm-sync           # regenerate the two machine-managed regions of the chart (runs `manifests` first)
make helm-verify         # helm lint (defaults + ci/nondefault-values.yaml) + the kustomize equivalence check
```

The chart checks need `helm` >= 3.8 and `python3` with PyYAML (used by
`hack/compare-helm-kustomize.sh`, `hack/check-render.sh` and
`hack/check-values.sh`). CI runs the whole chart suite twice, over a matrix of
the latest 3.x and the latest 4.x (the `helm` matrix in
`.github/workflows/helm.yml`, mirrored by the `helm` job in `release.yml`), so
the chart cannot break on either line unnoticed. The release itself *packages*
the chart with the 3.x floor (`HELM_VERSION` in `release.yml`) so the artifact
is readable by every supported Helm. Bump both matrices together.

Run a single test:

```bash
# one package, unit/envtest layer (needs KUBEBUILDER_ASSETS for envtest-backed packages,
# which `make test` sets up automatically via setup-envtest; for a package that has no
# envtest dependency — e.g. internal/sync, internal/events — plain `go test` is enough)
go test ./internal/sync/... -run TestSync_ReaderNotFound_FailingSecretNotFound_NoValueAnywhere -v

# integration layer (build tag `integration`, needs Docker)
go test -tags integration ./internal/azure/... -run TestStorageQueueSource_BatchReceiveAndDelete -v
```

CI runs `make test`, `make lint`, and `make test-integration` on every PR
(`.github/workflows/test.yml`, `lint.yml`, `test-integration.yml`); `helm.yml`
lints and renders the chart, fails on `make helm-sync` drift, and runs the
equivalence check; `test-e2e` runs there too (`test-e2e.yml`) and additionally
on release runs as part of `release.yml`, which then builds and pushes the
multi-arch image to GHCR, publishes the chart to
`oci://ghcr.io/tabman83/charts/kvsynk8s`, and attaches both the rendered
install manifest and `kvsynk8s-X.Y.Z.tgz` to a GitHub Release.

A release starts either from a `v[0-9]*` tag push or from
`gh workflow run Release -f version=X.Y.Z` (the version has no leading `v`; the
run creates the tag itself at the end). A version with a semver prerelease
suffix still publishes everything but does **not** move the `:latest` image tag
and marks the GitHub Release as a prerelease. The whole workflow is serialised
by `concurrency: release` so two releases cannot race on `:latest`.

Once a branch is pushed and its PR is open, watch its checks
(`gh pr checks <PR#> --watch`) and fix forward on any failure — see the "PR
CI babysitting" note in `specs/001-akv-eventgrid-sync/tasks.md` for the exact
loop. A task/PR is not done until its checks are green; never merge it
yourself, and never weaken a test or lint rule to get there.

## Workflow: everything goes through a PR

- Never commit directly to `master`. Create a branch, commit there, push, open a PR.
- The human operator (Nino) reviews every PR and decides when it gets merged. Do not merge a PR yourself, and do not assume approval — wait for an explicit "merge it".
- One PR per logical change. Keep the diff focused on what was asked.
- When merging: use squash merge (`gh pr merge --squash --delete-branch`), and always delete the branch (local and remote) after merging.

## PR descriptions

Write them in plain, simple English, matching how Nino writes (Italian, not a native English speaker). Concretely:

- Short, direct sentences. One idea per sentence.
- Simple everyday words. No marketing tone, no "comprehensive", "robust", "seamlessly", "leverage".
- No emoji, no bold-heavy formatting, no long bullet trees.
- Say what the PR does and why, then what to check when reviewing. That is enough.
- If something is unfinished or uncertain, say it plainly ("this part is still missing", "not sure about X, tell me what you think").

Example of the tone to aim for:

```
This PR adds the watcher for Key Vault.

It polls the vault every 30 seconds and updates the K8s secret when a value
changes. For now the interval is hardcoded, I will move it to config later.

To review: check the retry logic in watcher.go, I am not sure the backoff is
right.
```
