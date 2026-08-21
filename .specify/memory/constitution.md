<!--
Sync Impact Report
==================
Version change: (unversioned template) → 1.0.0
Rationale: initial ratification — all placeholders filled for the first time.

Modified principles: none (all newly defined)
Added sections:
  - Core Principles (5 principles):
    I. Secrets Are Never Exposed (NON-NEGOTIABLE)
    II. Reliability of Sync
    III. Simplicity First
    IV. Tested Changes
    V. Least-Privilege Access
  - Security Requirements
  - Development Workflow
  - Governance
Removed sections: none
Templates status: dependent templates read the constitution at runtime; no updates required.
Follow-up TODOs: none.
-->

# kvsynk8s Constitution

## Core Principles

### I. Secrets Are Never Exposed (NON-NEGOTIABLE)

Secret values MUST NOT appear in logs, error messages, metrics, traces, CLI output,
or commit history. Logging MAY reference a secret by name, vault, and version, never
by value. Test fixtures MUST use fake values that are clearly fake. Any code path
that serializes a secret for a purpose other than writing the Kubernetes Secret
object is a defect.

Rationale: the whole purpose of this project is moving secrets; a single value leak
defeats it entirely.

### II. Reliability of Sync

The sync loop MUST converge: after a Key Vault change, the corresponding Kubernetes
Secret MUST eventually reach the new value without manual intervention. Transient
failures (network, throttling, auth token expiry) MUST be retried with backoff, not
crash the process. A failed sync of one secret MUST NOT block the sync of others.
The current sync state (in sync / pending / failing) MUST be observable, e.g. via
logs or status fields.

Rationale: consumers trust that what is in the cluster matches the vault; silent
drift is worse than a loud failure.

### III. Simplicity First

Start with the simplest mechanism that works and grow only when a real need appears.
Do not add configuration options, abstractions, CRDs, or dependencies speculatively.
Documentation MUST describe only what exists; do not write docs for planned or
imagined features (this applies to CLAUDE.md sections as well).

Rationale: the project is young; every speculative layer is maintenance cost with no
user.

### IV. Tested Changes

Every behavior change MUST come with tests that fail without the change. Sync logic,
retry/backoff behavior, and secret-writing code require automated tests; glue code
with no logic MAY be exempt, and the PR MUST say so when it is. The full test suite
MUST pass before a PR is considered reviewable.

Rationale: a sync tool fails quietly in production; tests are the only cheap place
to catch that.

### V. Least-Privilege Access

Identities used by the tool MUST hold only the permissions needed: read access to
the specific Key Vault secrets, and write access to only the Kubernetes Secret
resources it manages (scoped by namespace where possible). Credentials MUST come
from the platform (workload identity, managed identity, service account tokens),
never from values hardcoded or committed to the repository.

Rationale: this tool bridges two secret stores; over-broad access turns a small bug
into a cluster-wide incident.

## Security Requirements

- Dependencies MUST be pinned and come from official registries.
- Authentication to Azure and to the Kubernetes API MUST use short-lived,
  platform-issued credentials; long-lived static secrets are not acceptable.
- Any file that could contain real credentials (kubeconfig, .env, local settings)
  MUST be covered by .gitignore before it can exist.
- Security-relevant changes (auth, RBAC, secret handling) MUST be called out
  explicitly in the PR description so review can focus on them.

## Development Workflow

- Never commit directly to `master`. Every change goes through a branch and a PR.
- The human operator (Nino) reviews every PR and decides the merge. Do not merge or
  assume approval; wait for an explicit "merge it".
- One PR per logical change; keep the diff focused on what was asked.
- PR descriptions MUST be plain, simple English: short direct sentences, no
  marketing tone, no emoji. Say what the PR does, why, and what to check when
  reviewing. State plainly anything unfinished or uncertain.
- When the first real code lands, CLAUDE.md MUST be updated with the actual build,
  lint, and test commands and the real architecture — not before.

## Governance

This constitution supersedes other practice documents where they conflict; CLAUDE.md
remains authoritative for day-to-day agent instructions and MUST stay consistent
with it.

Amendments are made by PR that edits this file, reviewed and merged by the project
owner like any other change. Each amendment MUST update the version below using
semantic versioning: MAJOR for removing or redefining a principle in a backward
incompatible way, MINOR for adding a principle or materially expanding guidance,
PATCH for clarifications and wording fixes.

Compliance is checked at PR review: a PR that violates a principle MUST either be
changed or explicitly justify the deviation in its description, and the reviewer
decides whether the justification stands.

**Version**: 1.0.0 | **Ratified**: 2026-08-21 | **Last Amended**: 2026-08-21
