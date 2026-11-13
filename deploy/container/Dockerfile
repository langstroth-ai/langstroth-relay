# syntax=docker/dockerfile:1
#
# Non-root, distroless image for the Ligus relay, for Kubernetes and any other
# runtime that runs containers as an unprivileged user. Unlike the root
# Dockerfile (Alpine, starts as root to chown a snapshot volume, then drops
# privileges), this image has no shell and no entrypoint script: the binary
# runs directly as UID 65532 and passes Pod Security "restricted" with a
# read-only root filesystem.
#
#   docker build -f deploy/container/Dockerfile -t ligus-relay:dev .
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -f deploy/container/Dockerfile -t <registry>/ligus-relay:<version> --push .
#
# Cross-compiles on the build platform (CGO is off), so multi-arch builds do
# not run the Go toolchain under emulation.
#
# Storage: set DATABASE_URL (Postgres) for anything shared or durable. Without
# it the relay keeps state in memory, plus a snapshot in LIGUS_RELAY_DATA_DIR
# when that is set to a writable volume (give the pod an fsGroup so the
# volume is writable by 65532).

ARG GO_VERSION=1.25

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/ligus-relay ./cmd/ligus-relay

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="Ligus Relay" \
      org.opencontainers.image.description="Content-blind, end-to-end-encrypted sync relay for Ligus (non-root, distroless)" \
      org.opencontainers.image.source="https://github.com/ligus-ai/relay" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/ligus-relay /usr/local/bin/ligus-relay
USER 65532:65532
EXPOSE 8443
ENTRYPOINT ["/usr/local/bin/ligus-relay"]
