# ligus-relay Helm chart

The Ligus relay on Kubernetes: content-blind, end-to-end encrypted sync, plus
the phone tunnel. The relay never holds a workspace key, a device key or
plaintext; its namespace holds only the database password, its token-issuer
public key and (on the tunnel pod) the APNs key.

## Topologies

The relay keeps some state in process (live tunnel control connections, SSE
fan-out, short pairing codes, rate limiters), so the chart runs the same image
in two shapes:

| `topology` | Renders | Store |
|---|---|---|
| `split` | `<release>-sync` Deployment (every route except `/v1/tunnel/*`, `LIGUS_RELAY_TUNNEL=off`, 1..N replicas) and `<release>-tunnel` Deployment (`/v1/tunnel/*`, exactly one replica, `strategy: Recreate`) | Postgres (required) |
| `single` | one Deployment, every route, one replica, `Recreate` | Postgres, or memory with an optional snapshot PVC |
| `auto` (default) | `split` when a database is configured, else `single` | |

`Recreate` matters for the tunnel: with a rolling update the old pod keeps a
host's control socket while a phone's connect lands on the new pod. On
shutdown the relay closes tunnel sockets with `4011 shutting_down` and hosts
reconnect within seconds.

With an Ingress or HTTPRoute enabled, `/v1/tunnel` routes to the tunnel
Service and everything else to the sync Service. In-cluster clients use the
two Services directly (`LIGUS_RELAY_URL` and `LIGUS_REMOTE_RELAY`); the
install notes print both URLs.

## Quick start (kind or k3d)

```sh
docker build -f deploy/container/Dockerfile -t ligus-relay:dev .
kind load docker-image ligus-relay:dev

kubectl create namespace ligus-relay
kubectl label namespace ligus-relay pod-security.kubernetes.io/enforce=restricted
helm install ligus-relay deploy/helm/ligus-relay -n ligus-relay \
  --set image.repository=ligus-relay --set image.tag=dev \
  --set postgres.enabled=true
helm test ligus-relay -n ligus-relay
```

Use the release name `ligus-relay` to get Service names `ligus-relay-sync` and
`ligus-relay-tunnel`; other release names are prefixed.

`postgres.enabled` runs one development Postgres pod (no backups, no TLS, no
HA). For anything you keep, use a managed database or an operator such as
CloudNativePG and pass it as `externalDatabase.existingSecret` (a Secret with a
`DATABASE_URL` key). With a database configured the chart sets
`LIGUS_RELAY_REQUIRE_DB=1`, so a pod with a missing or broken URL fails instead
of silently serving from memory.

## Values

| Key | Default | Notes |
|---|---|---|
| `image.repository` / `tag` / `digest` | `ghcr.io/ligus-ai/ligus-relay`, appVersion | Pin by digest in production |
| `image.command` | `[/usr/local/bin/ligus-relay]` | Runs the binary directly as UID 65532; also skips the Alpine image's root-only entrypoint |
| `topology` | `auto` | `split`, `single`, `auto` |
| `tunnel.mode` | `open` | `off`, `open`, `allowlist` (`tunnel.allowedHosts`) |
| `tunnel.apns.*` | off | Built-in push sender on the tunnel pod; key file from `existingSecret` |
| `sync.replicas`, `sync.autoscaling`, `sync.pdb` | 1, off, off | See Scaling |
| `postgres.enabled` | `false` | Development Postgres in the release |
| `externalDatabase.url` / `existingSecret` | empty | Managed Postgres |
| `memoryStore.maxBytes` | `100000000` | Memory store only: heap budget for workspace data (`LIGUS_RELAY_MAX_STORE_BYTES`); writes past it get 507 |
| `persistence.enabled` | `false` | Memory-store snapshot PVC (`single` only) |
| `config.env` | `{}` | Plain `LIGUS_RELAY_*` settings (membership mode, retention, ...) |
| `config.existingSecret` | empty | Secret whose keys become env (`LIGUS_RELAY_TOKEN_PUBKEY`, `LIGUS_RELAY_METRICS_TOKEN`, ...) |
| `clientIP.trustedProxies` / `clientIP.header` | empty, `X-Forwarded-For` | Proxies whose forwarding header names the client (see Scaling) |
| `ingress.*`, `httpRoute.*` | off | TLS at the edge; idle timeout must exceed 60 s |
| `metrics.serviceMonitor.*` | off | Prometheus Operator; set `bearerTokenSecret` when a metrics token is configured |
| `networkPolicy.enabled` | `true` | Ingress to 8443 from `ingressFrom` (empty = anywhere) |
| `networkPolicy.restrictEgress` | `true` | Egress only to DNS, the in-chart Postgres and `extraEgress` (add your database and, for APNs, 443) |

The chart refuses combinations that cannot work: `split` without a database,
a snapshot PVC with a database or with `split`, `allowlist` without hosts.

## Security

- Distroless static image, UID 65532, read-only root filesystem, all
  capabilities dropped, `RuntimeDefault` seccomp: passes Pod Security
  `restricted`.
- No Kubernetes API access: the service account has no token mounted.
- Clients require `https://` outside the cluster: terminate TLS at the Ingress
  or Gateway (cert-manager).

## Scaling

Keep `relay-sync` at one replica for now. With more than one:

- short pairing codes (`/v1/pair`) live in one process, so a code created on
  one replica is unknown to another;
- SSE nudges fire only on the replica that stored the envelope (clients still
  poll, so this costs latency, not correctness);
- rate limits are per process.

Rate limits key on the connection's remote address. Behind a Kubernetes
ingress every client shares the proxy's address (and one bucket: ten bad
pairing-code guesses from anyone block pairing for everyone for a minute), so
set `clientIP.trustedProxies` to the CIDRs your ingress controller or load
balancer connects from. The relay then reads `X-Forwarded-For` (or
`clientIP.header`) from those peers only, walking right to left past trusted
hops; a header from any other peer is ignored.

## Image

`deploy/container/Dockerfile` builds the non-root distroless image used here,
cross-compiled for `linux/amd64` and `linux/arm64` with buildx:

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
  -f deploy/container/Dockerfile -t <registry>/ligus-relay:<version> --push .
```

The repository-root `Dockerfile` (Alpine, root entrypoint that chowns a data
volume) is unchanged for hosts that mount root-owned volumes.

## CI

`.github/workflows/kind-e2e.yml` builds the image, installs the chart on kind
in both topologies under Pod Security `restricted` and runs `helm test`.
