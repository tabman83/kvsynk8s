# Phase 0 Research: Single-Replica Invariant Made Real

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Date**: 2026-08-30

The spec carried no `[NEEDS CLARIFICATION]` markers, so this document resolves the open
design choices the plan had to make rather than unknowns the spec left behind.

---

## R1 — `Recreate` versus a zero-surge rolling update

**Decision**: `spec.strategy.type: Recreate`, with no `rollingUpdate` block.

**Rationale**: Both candidates produce identical behaviour at one replica, so the choice is
about which one states the intent and which one can be defeated.

The current default is the trap. With no `strategy` set, Kubernetes applies
`RollingUpdate` with `maxSurge: 25%` and `maxUnavailable: 25%`. At `replicas: 1`, `maxSurge`
rounds **up** to 1 and `maxUnavailable` rounds **down** to 0 — so the Deployment is required
to add a pod before it may remove one. That rounding is why the overlap exists at all, and
it is exactly the kind of thing a reader does not derive when scanning a manifest.

`RollingUpdate` with `maxSurge: 0, maxUnavailable: 1` would also serialise the rollout today.
It is rejected for two reasons. It is a two-field statement of a one-word intent, and both
fields have to stay correct together. And its guarantee is proportional, not absolute: if
anyone ever raises the replica count, `maxSurge: 0` still permits N−1 instances to run
concurrently, whereas `Recreate` keeps meaning "stop everything, then start everything"
however many replicas are asked for. Since the whole feature is about an invariant that was
true only in principle, the option that cannot be quietly weakened is the right one.

**Alternatives considered**: leaving the default and documenting the overlap (rejected — the
project's documentation asserts the overlap never happens, and a document that describes a
hazard as acceptable is not the same as one that says the hazard does not exist); adding a
`preStop` hook or a longer grace period to shorten the overlap (rejected — it narrows the
window without closing it, and adds a moving part to a feature whose point is removing one).

**Cost accepted**: an upgrade now has a window with no operator running. Bounded below by
the existing `terminationGracePeriodSeconds: 10` plus the readiness probe's
`initialDelaySeconds: 5`; SC-003 sets the ceiling at 60s with the image already present.

---

## R2 — What actually pins the strategy, and why one check is not enough

**Decision**: two checks, because they catch different failures.

1. `hack/check-render.sh` gains a positive assertion: in any render of the chart, the
   operator Deployment must declare `spec.strategy.type: Recreate`. This script already runs
   over six different value combinations in `helm.yml` and is the right home for an
   invariant that must hold for every render, not just the default one.
2. `hack/compare-helm-kustomize.sh` gains `.spec.strategy` to its compared Deployment field
   list, next to `.spec.replicas`.

**Rationale**: The equivalence check alone is not sufficient, and this is the subtle part. It
asserts the chart and the kustomize output **agree**. Two manifests that both omit `strategy`
agree perfectly. So on its own it would pass today, pass after a revert, and pass forever —
it can only catch drift between the paths, never absence from both. The render check supplies
the positive statement; the equivalence check then propagates it to the manifest install,
because a kustomize output missing `Recreate` no longer matches a chart render that has it.

Verified against the current source: `compare-helm-kustomize.sh` compares an explicit field
list (`replicas`, `selector.matchLabels`, pod annotations and labels, pod `securityContext`,
`serviceAccountName`, `terminationGracePeriodSeconds`, and the manager container's `args`,
`command`, `ports`, `securityContext`, probes and `resources`). `strategy` is not in it.
Setting the field in both paths without touching the script would therefore be silently
unguarded.

**One gap found while checking this**: `make helm-verify` does not run
`hack/check-render.sh` today. Its recipe is `helm lint` twice, `hack/check-values.sh` and
`hack/compare-helm-kustomize.sh`; the render check runs only in the `helm.yml` workflow's
"Render the chart" step. Putting one of this feature's two enforcement points in a script
that no local target runs would mean a developer's `make helm-verify` passes on a change CI
then rejects. The plan therefore adds `hack/check-render.sh` to the `helm-verify` recipe —
two lines, no new tooling, and it makes the local target honest about what it covers. It is
the smallest scope creep in the feature and can be dropped without affecting FR-001 to
FR-003, since CI enforces the check either way.

**Alternatives considered**: asserting it only in the e2e suite (rejected — a ~4 minute
kind run is a poor place for a one-field manifest assertion, and it would not cover the
chart's non-default renders); a golden-file snapshot of the whole rendered Deployment
(rejected — it would fail on every unrelated change and train people to regenerate it
without reading the diff).

---

## R3 — Can the e2e suite observe the overlap directly?

**Decision**: No. The e2e suite asserts the *deployed* Deployment carries
`strategy.type: Recreate`; it does not attempt to observe a rollout and count pods.

**Rationale**: Watching a rollout for "at most one pod running at any instant" is a sampling
problem. Catching the overlap requires polling faster than the window it is trying to
observe, and a passing run would only ever mean "we did not sample it", not "it did not
happen". A test whose failure mode is a flake and whose success mode is inconclusive is worse
than no test, especially in a suite the project already keeps to about four minutes.

The declarative assertion is stronger anyway: `Recreate` is a Kubernetes-guaranteed property
of the object, so asserting the field is present in the live cluster proves the behaviour
without racing it. SC-001 remains verifiable by a human during the manual validation run
described in [quickstart.md](./quickstart.md).

**Alternatives considered**: a dedicated slow spec that upgrades the operator image and
watches pods (rejected — flaky, slow, inconclusive as above).

---

## R4 — How to test that the listener waits for leadership

**Decision**: an envtest-backed test in `internal/controller`, using a Lease that a
different holder already owns, asserting the listener never starts.

**Rationale**: This is the only requirement in the feature whose test needs a configuration
that production never runs, so it deserves the reasoning written down.

`NeedLeaderElection()` is a one-line declaration. Asserting it returns `true` is a tautology
test: it restates the implementation and would pass even if controller-runtime changed what
the value means. The behaviour worth pinning is the consequence — *a listener does not run
where its events are not consumed* — and controller-runtime decides that by sorting
runnables into groups at `mgr.Add()` time, then starting the leader-election group only after
the lease is acquired.

The shape that exercises it:

- build a manager against the existing envtest `cfg` with `LeaderElection: true`, a unique
  `LeaderElectionID`, and an explicit `LeaderElectionNamespace` (required when not running
  in-cluster);
- pre-create the `coordination.k8s.io/v1` Lease that ID maps to, held by another identity
  with a long duration, so this manager stays a candidate for the whole test;
- add a real `events.Listener` wired to a fake `QueueSource` (the pattern already used
  throughout `internal/events/listener_test.go`);
- start the manager, wait for the cache to sync, and assert the fake's receive count stays
  at zero.

Today that fails: `NeedLeaderElection()` returns `false`, so the listener is placed in the
"start immediately" group and begins polling as soon as the manager starts. After the fix it
passes, because the listener waits for a lease it never gets.

**Placement**: `internal/controller`, not `internal/events`. `internal/controller` already
owns the envtest suite and already builds managers in tests
(`startManagerFor` in `secretsync_controller_test.go`). Putting the test in
`internal/events` would give that package an envtest dependency and invalidate the property
CLAUDE.md documents — that `internal/sync` and `internal/events` run under plain `go test`
with no `KUBEBUILDER_ASSETS`. A test-only import of `internal/events` from the controller
package's tests costs nothing and keeps that promise. The new file must carry a comment
saying why it lives there, or the next reader will move it.

**Alternatives considered**: asserting `NeedLeaderElection() == true` directly (rejected as a
tautology, though it is fine as a one-line companion assertion inside the same test);
constructing a manager without envtest to inspect its runnable grouping (rejected — manager
construction builds a client and mapper, so it is not reliably offline, and the grouping is
internal state with no exported accessor); a fake `manager.Manager` (rejected — it would test
a stand-in's sorting logic, not controller-runtime's).

---

## R5 — Why the listener's declaration is wrong rather than merely unused

**Decision**: `NeedLeaderElection()` returns `true`, and the comment states the safety
consequence rather than the current irrelevance.

**Rationale**: Traced through the current source, a listener on an instance without a running
reconcile loop fails silently and destructively:

1. `handleMessage` sends each matched object into `l.Events` with
   `select { case l.Events <- ...; case <-ctx.Done(): }` — it blocks until the channel
   accepts or the context is cancelled.
2. On such an instance nothing drains that channel: the `source.Channel` watch belongs to the
   controller, which is a leader-election runnable and is not running. The buffer
   (`eventsChannelBufferSize = 256` in `cmd/main.go`) fills and the send blocks.
3. `Start`'s loop is strictly sequential, so the poll loop stops mid-batch. The messages
   already received are held with no deadline — `QueueCallTimeout` bounds the Receive and
   Delete calls, not the processing between them.
4. `internal/azure/queue.go` calls `DequeueMessages` with no `VisibilityTimeout`, so Azure's
   default of 30 seconds applies. The held messages become visible again and are redelivered.
5. Each redelivery increments `DequeueCount`. Once it passes `DefaultPoisonThreshold` (5),
   `handleMessage` deletes the message without parsing or matching it.

The result is rotation events discarded with no failure signal: the operator falls back to
the multi-hour periodic reconcile, and
`kvsynk8s_queue_consecutive_receive_failures` stays at zero throughout because `Receive`
itself never failed. The existing comment calls this "defensive" documentation of something
"not necessary". It is the opposite: the declaration is load-bearing, and its current value
is wrong.

**Alternatives considered**: removing the `LeaderElectionRunnable` implementation entirely
(rejected — controller-runtime's default for a runnable that does not implement the interface
is to require leadership, which is the correct behaviour, but relying on an unstated default
for a safety property is how this defect happened in the first place); leaving it and adding
a warning comment (rejected — a comment does not stop the runnable from starting).

---

## R6 — Where the failure-behaviour documentation goes

**Decision**: a new top-level README section between `## Operator configuration` and
`## Troubleshooting`, plus three edits to statements that this feature makes inaccurate.

**Rationale**: The claim being documented — the operator is not on any request path — is an
availability property, not a configuration option or a symptom, so it belongs between the two
rather than inside either. Placing it before Troubleshooting also means the reader has it
before they arrive with a problem.

The three existing statements that must change, located in the current source:

- `README.md` around line 77 ("There is no `replicas` value. The operator runs without leader
  election, so a second replica would reconcile the same SecretSync objects…") — still true,
  but it now has a mechanism behind it and should say so.
- `README.md` around line 345, the `--leader-elect` row in the operator-configuration table —
  the flag stays accepted and ignored; the row should stop implying single-replica is merely
  a convention.
- `charts/kvsynk8s/values.yaml`, the "Deliberately not configurable" block at the end, and
  the matching comments in `charts/kvsynk8s/templates/deployment.yaml` and
  `config/manager/manager.yaml`, all of which currently say "keep replicas at 1" as an
  instruction to the reader rather than a property of the manifest.

`hack/check-doc-versions.sh` refuses a pinned release version inside a copy-paste install
command. The new prose contains no install command, so it does not interact with that guard,
but any example added while editing must respect it.

**Alternatives considered**: a separate `docs/availability.md` (rejected — the project keeps
user documentation in one README on purpose, and a second file would be found by nobody);
putting it only in the chart values comments (rejected — manifest-install users never read
them).

---

## R7 — Does `specs/002-helm-chart` need updating?

**Decision**: add a pointer line to
`specs/002-helm-chart/contracts/rendered-resources.md`; leave `spec.md` and the other 002
artifacts as the historical record they are.

**Rationale**: 002's spec and its clarifications record what was decided in August 2026 and
should not be rewritten to look like they anticipated this. But
`rendered-resources.md` is different: `compare-helm-kustomize.sh` names it in its header as
the definition of "equivalent", and this feature widens that definition by one field. A
contract that a running script cites has to stay accurate, so it gets a line pointing at
[contracts/deployment-rollout.md](./contracts/deployment-rollout.md).

**Alternatives considered**: restating the whole rollout contract inside 002 (rejected —
duplicates a contract that now lives here); leaving 002 untouched (rejected — the script's
stated source of truth would be silently incomplete).
