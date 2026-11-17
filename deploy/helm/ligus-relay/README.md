# ligus-relay Helm chart (preview)

> **Preview.** This chart and the relay image are not published to a registry
> yet: build the image yourself and install the chart from a checkout (relay
> v0.3.0 or later). Values may change before they are published.

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
| `split` | `<release>-sync` Deployment (every route except `/v1/tunnel/*`, `LIGUS_RELAY_TUNNEL=off`, exactly one replica in 2.0, `Recreate`) and `<release>-tunnel` Deployment (`/v1/tunnel/*`, exactly one replica, `strategy: Recreate`) | Postgres (required) |
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
# The development Postgres's password and DSN (the chart generates none).
PW=$(openssl rand -hex 24)
kubectl -n ligus-relay create secret generic relay-dev-postgres \
  --from-literal=password=$PW \
  --from-literal=DATABASE_URL="postgres://relay:$PW@ligus-relay-postgres:5432/relay?sslmode=disable"
helm install ligus-relay deploy/helm/ligus-relay -n ligus-relay \
  --set image.repository=ligus-relay --set image.tag=dev \
  --set postgres.enabled=true --set postgres.existingSecret=relay-dev-postgres
helm test ligus-relay -n ligus-relay
```

Use the release name `ligus-relay` to get Service names `ligus-relay-sync` and
`ligus-relay-tunnel`; other release names are prefixed.

`postgres.enabled` runs one development Postgres pod (no backups, no TLS
(`sslmode=disable`), no HA); it is unsupported in production. For anything you keep, use a managed database or an operator such as
CloudNativePG and pass it as `externalDatabase.existingSecret` (a Secret with a
`DATABASE_URL` key, `sslmode=verify-full` and the server's CA). With a database configured the chart sets
`LIGUS_RELAY_REQUIRE_DB=1`, so a pod with a missing or broken URL fails instead
of silently serving from memory.

## Values

| Key | Default | Notes |
|---|---|---|
| `image.repository` / `tag` / `digest` | `ghcr.io/ligus-ai/ligus-relay`, appVersion | Pin by digest in production |
| `image.command` | `[/usr/local/bin/ligus-relay]` | Runs the binary directly as UID 65532; also skips the Alpine image's root-only entrypoint |
| `topology` | `auto` | `split`, `single`, `auto` |
| `tunnel.mode` | `off` | `off`, `open`, `allowlist` (`tunnel.allowedHosts`). `open` admits any host; pair it with a token gate |
| `tunnel.apns.*` | off | Built-in push sender on the tunnel pod; key file from `existingSecret`, a Secret of its own (the chart refuses `config.existingSecret` or `LIGUS_RELAY_APNS_*` in `config.env`, which reach relay-sync too) |
| `sync.replicas`, `sync.autoscaling`, `sync.pdb` | 1, off, off | Fixed in 2.0: anything else fails to render (see Scaling) |
| `postgres.enabled` / `existingSecret` | `false`, required | Development Postgres in the release; the Secret holds `password` and `DATABASE_URL` |
| `externalDatabase.url` / `existingSecret` | empty | Managed Postgres; mounted as a file (`DATABASE_URL_FILE`) |
| `memoryStore.maxBytes` | `100000000` | Memory store only: heap budget for workspace data (`LIGUS_RELAY_MAX_STORE_BYTES`); writes past it get 507 |
| `contacts.enabled` | `true` | `/v1/contacts/*` (contact records + sealed drop boxes); `false` renders `LIGUS_RELAY_CONTACTS=off` (404) |
| `contacts.*` (`maxRecordBytes`, `maxDropBytes`, `ttlDays`, `dropMaxRows`, `dropMaxBoxBytes`, `dropRetentionDays`, `reclaimHoldDays`, `maxContacts`, `dropRate`/`dropBurst`, `dropCidRate`/`dropCidBurst`, `writeRate`/`writeBurst`, `createRate`/`createBurst`) | `null` (binary defaults) | Contact size caps, TTL, drop-box bounds and rate limits; each maps to a `LIGUS_RELAY_CONTACT*` variable (see the relay README). Rate limiters are per pod |
| `githubRoutes` | `false` | Deprecated GitHub social routes for 1.x clients (`LIGUS_RELAY_GITHUB_ROUTES=on`); removed in relay v0.4 |
| `persistence.enabled` | `false` | Memory-store snapshot PVC (`single` only) |
| `config.env` | `{}` | Plain `LIGUS_RELAY_*` settings (membership mode, retention, ...) |
| `config.existingSecret` | empty | Secret-valued settings, one key per setting name (`LIGUS_RELAY_ACCESS_TOKENS`, `LIGUS_RELAY_METRICS_TOKEN`, `LIGUS_RELAY_TOKEN_PUBKEY`, ...), mounted 0400 as a directory the relay reads (`LIGUS_RELAY_SECRETS_DIR`); never env |
| `clientIP.trustedProxies` / `clientIP.header` / `allowSharedBucket` | empty, `X-Forwarded-For`, off | Exact addresses of the ingress or LB whose forwarding header names the client (never the whole pod CIDR; see Scaling) |
| `ingress.*`, `httpRoute.*` | off | TLS required (`ingress.tls.secretName`, or `tls.terminatedUpstream` behind a TLS load balancer). `ingress.streamingDefaults` (on) sets 3600 s nginx/ALB timeouts and `proxy-buffering: off`; `httpRoute.timeouts` 3600 s. Requires `clientIP.trustedProxies` (or `allowSharedBucket`) and `metrics.token` (or `metrics.allowPublicUnauthenticated`) |
| `metrics.token.existingSecret` / `key` | empty, `token` | `LIGUS_RELAY_METRICS_TOKEN` as a file; the ServiceMonitor uses it too |
| `metrics.serviceMonitor.*` | off | Prometheus Operator; set `bearerTokenSecret` when a metrics token is configured |
| `networkPolicy.enabled` | `true` | Ingress to 8443 from `ingressFrom` (empty = anywhere) |
| `networkPolicy.restrictEgress` | `true` | Egress only to DNS, the in-chart Postgres and `extraEgress` (add your database and, for APNs, 443) |
| `networkPolicy.dns.namespaceSelector` / `podSelector` | `kube-system`, `k8s-app: kube-dns` | Where DNS egress may go. Change them if your resolver pods are labeled differently, or DNS stops resolving |

The chart refuses combinations that cannot work: more than one relay-sync
replica, autoscaling or a PDB, `split` without a database,
a snapshot PVC with a database or with `split`, `allowlist` without hosts. It
also refuses APNs settings outside `tunnel.apns.*`.

## Security

- Distroless static image, UID 65532, read-only root filesystem, all
  capabilities dropped, `RuntimeDefault` seccomp: passes Pod Security
  `restricted`.
- No Kubernetes API access: the service account has no token mounted.
- No secret in the environment: the database URL, `config.existingSecret` and
  the APNs key are files mounted `0400` (with `fsGroup` the kubelet adds group
  read: `0440`, group 65532). The relay reads any setting `X` from `X_FILE` or
  from `$LIGUS_RELAY_SECRETS_DIR/X` when `X` itself is unset, and refuses to
  start when one of those files can't be read or is empty (a wrong
  `fsGroup` or `runAsUser` then fails the rollout instead of opening the
  access gate).
- `checksum/secrets` rolls the pods when a Secret they read changes. It uses
  `lookup`, a constant under `helm template`; with Argo CD or Flux use
  [Reloader](https://github.com/stakater/Reloader) or bump `podAnnotations`.
- The chart generates no Secrets, so every render is the same.
- Clients require `https://` outside the cluster: terminate TLS at the Ingress
  or Gateway (cert-manager); the chart refuses an Ingress without TLS.
- `/metrics` is open unless a metrics token is set, so a public relay must
  have one.

## Scaling

`relay-sync` runs exactly one replica in 2.0, with `Recreate` (a rolling
update's surge pod would be a second store for a moment); the chart refuses
more replicas, an HPA or a PDB. With more than one:

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

## Backups and upgrades

- **Back up Postgres, not pods.** Use your database's point-in-time recovery
  (WAL archiving: a managed database's PITR, or CloudNativePG / pgBackRest)
  and connect with `sslmode=verify-full` and the server's CA
  (`sslrootcert=` pointing at a mounted file). The relay holds ciphertext only,
  but the database also holds push tokens and pairing state: keep its dumps
  apart from the APNs key (together they are a push channel to every phone).
- **Migrations** run at startup in one transaction under a transaction-scoped
  Postgres advisory lock, so pods starting together can't race; it is safe
  behind a transaction-pooling PgBouncer (a session lock would leak there).
  A pod waits at most two minutes for the lock, then exits and is restarted.
  They are additive (expand/contract): a release
  never drops or renames what the previous release still reads, so an old and
  a new pod can overlap during an upgrade and you can roll back one release.
- **Upgrade order:** the relay first, then ligus-auth, then the workspaces
  (`ligus` chart), with each serve image pinned to the desktop release your
  members run. relay-sync and the tunnel use `Recreate`: expect a few seconds
  without sync, and hosts reconnecting their tunnels.
- **Turning on `LIGUS_RELAY_WORKSPACE_SCOPE=strict`:** issue
  workspace-scoped tokens first, roll them out to every client, then flip to
  `strict`; the reverse order locks out clients with unscoped tokens.
- The workspaces' serve and worker volumes hold identities and keys: the
  `ligus` chart's docs say not to back them up (revoke and re-enrol instead).

## Explicitly unsupported in 2.0

- More than one relay-sync replica, autoscaling, or tunnel/ligus-auth HA.
- Mutually distrusting tenants on one relay, ligus-auth or cluster.
- The in-chart Postgres (`postgres.enabled`) for anything but development.

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

`.github/workflows/kind-e2e.yml` renders every documented combination and the
refusals, builds the image, installs the chart on kind in both topologies
under Pod Security `restricted` and runs `helm test`. `helm_chart_test.go`
(`go test`, needs helm) checks the rendered security properties.
