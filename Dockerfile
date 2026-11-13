# hive-relay — build from this repo root: docker build -t hive-relay .
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/hive-relay ./cmd/hive-relay

FROM alpine:3.20
RUN apk add --no-cache su-exec && adduser -D -H hive
COPY --from=build /out/hive-relay /usr/local/bin/hive-relay
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
