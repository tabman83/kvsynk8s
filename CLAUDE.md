# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

`kvsynk8s` — realtime sync from Azure Key Vault to Kubernetes secrets (repo: `tabman83/kvsynk8s`, default branch `master`).

## Current state

The repository contains only `README.md` — no source code, build system, or tests yet. There are no build/lint/test commands to document.

When the first real code lands, update this file with:
- the actual build, lint, and test commands (including how to run a single test)
- the architecture: what watches Key Vault, how changes are propagated, how the K8s secrets are written (operator/controller? CRDs? sidecar?), and how auth to Azure and to the cluster is done

Do not invent those sections before the code exists.

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
