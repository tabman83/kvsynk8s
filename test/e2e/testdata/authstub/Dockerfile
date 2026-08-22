# authstub: a throwaway Entra ID token-endpoint stub used only by
# test/e2e (T032). Never built or published outside that test run.
FROM golang:1.26 AS builder
WORKDIR /workspace
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux go build -o authstub main.go

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /workspace/authstub /authstub
USER 65532:65532
ENTRYPOINT ["/authstub"]
