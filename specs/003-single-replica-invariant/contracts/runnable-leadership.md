# Contract: which manager runnables may run without leadership

**Feature**: [../spec.md](../spec.md) | **Satisfies**: FR-004, FR-005, FR-011 | **Date**: 2026-08-30

## The rule

A `manager.Runnable` registered with the operator's manager MAY run on an instance that does
not hold leadership **only if** everything it produces is consumed on that same instance.

A runnable that hands work to the reconcile loop does not qualify, because the reconcile loop
is itself gated on leadership. Such a runnable MUST require leadership — either by
implementing `manager.LeaderElectionRunnable` and returning `true` from
`NeedLeaderElection()`, or by not implementing the interface at all, which is
controller-runtime's default for the same reason.

## Applied to the current runnables

| Runnable | Requires leadership | Why |
|---|---|---|
| `internal/events.Listener` | **Yes** | Its only output is `event.GenericEvent` values written to a channel that the controller's `source.Channel` watch drains. The controller requires leadership, so without it nothing consumes them. |
| `SecretSyncReconciler` (via `SetupWithManager`) | Yes | controller-runtime's default for a controller. |
| Metrics server, health probes, caches | No | Managed by controller-runtime, started on every instance by design. Their behaviour under multiple replicas is a known consideration for any future HA work and is recorded in the spec's rejected-alternative section, not changed here. |

## What returning `false` from the listener would cause

Not merely a redundant poller. Traced through the current code, it is silent event loss:

1. `handleMessage` blocks sending into `l.Events`; on an instance without the controller,
   nothing drains it and the 256-slot buffer fills.
2. `Start`'s poll loop is sequential, so it stops mid-batch, holding messages already
   received with no deadline.
3. `internal/azure/queue.go` sets no `VisibilityTimeout`, so Azure's 30-second default
   returns those messages to the queue.
4. Each redelivery raises `DequeueCount`; past `DefaultPoisonThreshold` (5) the message is
   deleted without ever being parsed or matched.

Rotation events disappear, the operator silently falls back to the periodic reconcile, and
`kvsynk8s_queue_consecutive_receive_failures` stays at zero the whole time because `Receive`
never failed. The comment on `NeedLeaderElection()` MUST describe this consequence
(FR-011); saying leadership is "not currently used" is what allowed the wrong value to look
correct.

## What this contract does NOT do

It does not enable leader election. `cmd/main.go` still constructs the manager with
`LeaderElection: false`, the operator still runs one replica, and no `coordination.k8s.io`
permission is granted (FR-006, Constitution V). The `--leader-elect` flag is still parsed and
still ignored, so a hand-written Deployment that passes it keeps working (FR-005).

This contract states what MUST already be true so that enabling leader election is a safe
change to make later, rather than one that silently drops events.

## Enforcement

An envtest spec starts a manager with leader election enabled against a Lease already held by
another identity, registers a `Listener` backed by a fake queue source, and asserts the fake
receives no calls. It fails against the current `false` and passes against `true`. Placement
and alternatives are covered in research [R4](../research.md).
