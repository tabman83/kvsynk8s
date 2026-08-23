# Contract: Release Artifacts

**Feature**: `002-helm-chart` | Producer: `.github/workflows/release.yml` on
`v*` tag push | Consumers: `helm install` users, GitHub Release page visitors
(FR-014, SC-003)

For every tag `vX.Y.Z`, a successful release run MUST produce all of:

| # | Artifact | Reference | Contract |
|---|---|---|---|
| 1 | Container image | `ghcr.io/tabman83/kvsynk8s:vX.Y.Z` and `:latest` | multi-arch (amd64+arm64), unchanged from today |
| 2 | Install manifest | `install.yaml` asset on the GitHub Release | unchanged from today; references image #1 |
| 3 | OCI chart | `oci://ghcr.io/tabman83/charts/kvsynk8s:X.Y.Z` | `helm install kvsynk8s oci://ghcr.io/tabman83/charts/kvsynk8s --version X.Y.Z` resolves it on Helm >= 3.8 with no repo configuration |
| 4 | Chart archive | `kvsynk8s-X.Y.Z.tgz` asset on the GitHub Release | `helm install` from the downloaded file is equivalent to #3 |

## Version invariants

- Chart `version` == chart `appVersion` == `X.Y.Z` (tag minus `v` prefix).
- The image is pushed under the tag **with** the `v` (`:vX.Y.Z`, artifact #1),
  because the image build uses the git ref name directly. The chart's default
  `image.tag` is therefore `v` + appVersion, not the bare appVersion, so a
  chart install pulls image #1 without any values set. Getting this wrong makes
  every default install fail with ImagePullBackOff and nothing else in CI
  notices, so `.github/workflows/helm.yml` asserts the rendered tag explicitly.
- `Chart.yaml` in git stays at the `0.0.0` placeholder; versions are applied
  by `helm package --version --app-version` in the pipeline only.

## Ordering and failure behavior

- Chart publish runs after the image build job (`needs: build-and-push`);
  the GitHub Release job runs after the chart job and attaches #2 and #4
  together.
- Any failed step fails the whole workflow visibly; no partial release is
  reported green.
- Re-running the workflow on the same tag republishes #1–#4 idempotently
  (OCI tags are mutable; `action-gh-release` replaces existing assets).

## One-time manual step: GHCR package visibility

**Check the visibility of `ghcr.io/tabman83/charts/kvsynk8s` after the first
release.** GitHub's rules here are not clear-cut: a package published by a
workflow can inherit the repository's visibility, but a newly published
package scoped to a personal account defaults to private, and `helm push`
sends no `org.opencontainers.image.source` label to link the package to this
repository the way the image build does. Which of the two applies will only be
known when the first tag is pushed. If the package lands private, the
documented anonymous install fails with a 401 until it is set to Public:

```bash
helm install kvsynk8s oci://ghcr.io/tabman83/charts/kvsynk8s --version X.Y.Z
# Error: ... unauthorized
```

The fix is a one-time step per package: package page on GitHub → Package
settings → Change visibility → Public. Later releases keep whatever visibility
the package has. If it is needed, it is the one place SC-003's "zero manual
publishing steps" does not hold, and only for the first release.

## Credentials

- Only `GITHUB_TOKEN` with the existing `packages: write` (GHCR) and
  `contents: write` (release) permissions. No new secrets are introduced
  (Constitution: platform-issued credentials only).
