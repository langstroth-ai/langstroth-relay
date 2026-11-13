# Ligus relay (ligus-relay) — build from this repo root: docker build -t ligus-relay .
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ligus-relay ./cmd/ligus-relay

FROM alpine:3.20
RUN apk add --no-cache su-exec && adduser -D -H hive
LABEL org.opencontainers.image.title="Ligus Relay" \
      org.opencontainers.image.description="Content-blind, end-to-end-encrypted sync relay for Ligus (formerly Hive)" \
      org.opencontainers.image.source="https://github.com/ligus-ai/relay" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/ligus-relay /usr/local/bin/ligus-relay
# Former binary name, kept as an alias until v3.0 for custom entrypoints and
# scripts that exec hive-relay directly.
RUN ln -s ligus-relay /usr/local/bin/hive-relay
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
# Listens on 0.0.0.0:8443 unless $PORT or LIGUS_RELAY_ADDR (legacy
# HIVE_RELAY_ADDR) says otherwise. entrypoint.sh defaults the snapshot store to
# /data when neither LIGUS_RELAY_DATA_DIR nor HIVE_RELAY_DATA_DIR is set (no
# ENV here, so an operator's legacy HIVE_RELAY_* value is never shadowed by an
# image default under the new name). Set DATABASE_URL for a shared Postgres
# store (horizontal scaling / HA, no data migration) — it takes precedence.
EXPOSE 8443
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
