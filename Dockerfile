# Build the manager binary
FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build
# the GOARCH has no default value to allow the binary to be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot

# Links the published package to this repository, so it shows up on the repo
# page and inherits the repository's access permissions. Note this controls
# linking only: a newly published GHCR package is private until it is made
# public once in its package settings.
LABEL org.opencontainers.image.source=https://github.com/tabman83/kvsynk8s

# The commit this image was built from. The release pipeline creates the git tag
# LAST, so between the image push and the tag there is a published version with
# no tag to identify it; hack/check-release-overwrite.sh reads this label back
# off the registry to tell a legitimate same-commit re-run of a failed release
# from a different commit silently overwriting a published version. Empty when
# nobody passed it, which that guard deliberately treats as "cannot tell" and
# refuses, rather than assuming the safe case.
ARG GIT_REVISION=""
LABEL org.opencontainers.image.revision=$GIT_REVISION

WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
