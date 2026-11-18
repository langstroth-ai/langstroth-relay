# syntax=docker/dockerfile:1
#
# Non-root, distroless image for the Langstroth relay, for Kubernetes and any other
# runtime that runs containers as an unprivileged user. Unlike the root
# Dockerfile (Alpine, starts as root to chown a snapshot volume, then drops
# privileges), this image has no shell and no entrypoint script: the binary
# runs directly as UID 65532 and passes Pod Security "restricted" with a
# read-only root filesystem.
#
#   docker build -f deploy/container/Dockerfile -t langstroth-relay:dev .
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -f deploy/container/Dockerfile -t <registry>/langstroth-relay:<version> --push .
#
# Cross-compiles on the build platform (CGO is off), so multi-arch builds do
# not run the Go toolchain under emulation.
#
# Storage: set DATABASE_URL (Postgres) for anything shared or durable. Without
# it the relay keeps state in memory, plus a snapshot in LANGSTROTH_RELAY_DATA_DIR
# when that is set to a writable volume (give the pod an fsGroup so the
# volume is writable by 65532).

# Base images are pinned by tag and index digest; bump both together.
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine3.24@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/bin/langstroth-relay ./cmd/langstroth-relay && \
    ln -s langstroth-relay /out/bin/ligus-relay && ln -s langstroth-relay /out/bin/hive-relay

FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
LABEL org.opencontainers.image.title="Langstroth Relay" \
      org.opencontainers.image.description="Content-blind, end-to-end-encrypted sync relay for Ligus (non-root, distroless)" \
      org.opencontainers.image.source="https://github.com/langstroth-ai/langstroth-relay" \
      org.opencontainers.image.licenses="MIT"
# ligus-relay and hive-relay are symlinks to langstroth-relay: former binary
# names, kept until v3.0 for charts and scripts that exec them directly.
COPY --from=build /out/bin/ /usr/local/bin/
USER 65532:65532
EXPOSE 8443
ENTRYPOINT ["/usr/local/bin/langstroth-relay"]
