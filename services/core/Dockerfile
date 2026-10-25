# Tamarind core service: tool gateway (REST + MCP), migrations and validation.
# Build from the repository root:  docker build -f services/core/Dockerfile -t tamarind-core .

FROM golang:1.24-alpine AS build
WORKDIR /src/services/core
COPY services/core/go.mod services/core/go.sum ./
RUN go mod download && go mod verify
COPY services/core/ ./
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags="-s -w" -o /out/tamarind ./cmd/tamarind
RUN mkdir -p /out/blobs

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/tamarind /usr/local/bin/tamarind
# Owned by the nonroot user, so a named volume mounted here starts out writable.
COPY --from=build --chown=65532:65532 /out/blobs /var/lib/tamarind/blobs
COPY tools/ /app/tools/
COPY policy-packs/ /app/policy-packs/
COPY deploy/compose/principals.yaml /app/config/principals.yaml
ENV TAMARIND_TOOLS_DIR=/app/tools \
    TAMARIND_POLICY_PACK=/app/policy-packs/baseline/sme-working-capital \
    TAMARIND_PRINCIPALS=/app/config/principals.yaml \
    TAMARIND_ADDR=:8080 \
    TAMARIND_BLOB_DIR=/var/lib/tamarind/blobs
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["tamarind"]
CMD ["serve"]
