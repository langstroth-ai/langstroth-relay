# Changelog

## v0.3.0 (unreleased)

Additive for every existing `/v1` route and for `hrt1`: their requests and
responses are byte-identical to v0.2.0 (checked by golden responses recorded
against v0.2.0), and no crypto domain-separation string changed. The one
behavior change is that the deprecated GitHub social routes are off by
default.

### Renamed to Langstroth

The server side is now **Langstroth** (after the Langstroth frame hive); the
client apps stay **Ligus**. No wire format, token, crypto string, header,
route or database table changes.

- Go module `github.com/langstroth-ai/langstroth-relay` (was
  `github.com/ligus-ai/relay`); `cmd/langstroth-relay`, `cmd/langstroth-auth`;
  package `langstrothauth` (was `ligusauth`).
- Images `ghcr.io/langstroth-ai/langstroth-relay` and
  `ghcr.io/langstroth-ai/langstroth-auth`. The relay images keep
  `/usr/local/bin/ligus-relay` and `hive-relay`, and the auth image
  `ligus-auth`, as symlinks until v3.0.
- Charts `langstroth-relay`, `langstroth-auth` and the `langstroth-postgres`
  library, published from each release tag at
  `oci://ghcr.io/langstroth-ai/charts/<name>`. The auth chart's appVersion
  follows the repository (0.3.0). *Upgrade:* a release installed from the
  `ligus-relay` or `ligus-auth` chart upgrades with `--set
  nameOverride=ligus-relay` (or `ligus-auth`) to keep its resource names and
  selector labels; the charts now mount under `/etc/langstroth-relay` and
  `/etc/langstroth-auth`.
- Settings are `LANGSTROTH_RELAY_*` and `LANGSTROTH_AUTH_*`. `LIGUS_RELAY_*`,
  `LIGUS_AUTH_*` and `HIVE_RELAY_*` keep working. A `LANGSTROTH_*` name and a
  former name for the same setting set to different values stop the relay or
  `langstroth-auth` at startup; so do `LANGSTROTH_RELAY_<X>` and
  `LIGUS_RELAY_<X>` files with different contents in the secrets directory,
  which may use either name.
- `langstroth-auth` reads `/etc/langstroth-auth/config.json`, or
  `/etc/ligus-auth/config.json` when only that exists (the image no longer
  passes `--config`), and the systemd credential
  `langstroth-auth-signing-key`, or `ligus-auth-signing-key`.
- The agent scripts read `LANGSTROTH_NAMESPACE`, `LANGSTROTH_RELEASE`,
  `LANGSTROTH_SCENARIO`, ... and still accept the `LIGUS_*` names (two
  different values are a usage error); `local.sh` keeps using a
  `~/.local/state/ligus-relay` data directory, pid file and metrics token from
  before the rename.
- `langstroth-relay bootstrap-agent` prints `brew install langstroth-ai/hive/ligus`.
- The Fly deploy runs only for a release tag `v*`, after `ci` passes on it,
  not on every push to main. Fly concurrency counts open connections
  (1500/2000), and the health check uses the current `http_service.checks`
  schema.

### Added

- **Phone revocation over HTTPS.** `POST /v1/tunnel/revoke` (new, additive):
  a host revokes one of its phones with a request signed by its registered
  host key, without needing its control connection, and learns what was done
  (`sessionsClosed`, `pushTokenRemoved`). Timestamped within ±300 s and
  single-use per nonce (a replay answers `409`; each host may hold an eighth
  of the nonce cache); a forged or cross-host request answers `401`; a
  validly signed request with a skewed `ts` answers `401
  {"error":"clock_skew","now"}` and isn't counted as a failed
  authentication. A revocation that races a phone's session admission
  leaves neither the session nor its push token. It adds one signing label, `hive-tunnel/1
  host-revoke`; no existing label changes. The control connection's `revoke`
  is unchanged.
- **langstroth-auth: end one device's session.** `POST
  /admin/v1/tenants/{t}/devices/{device}/revoke` (admin listener) revokes
  every refresh token bound to that device id and leaves the person's other
  devices signed in. Idempotent. Access tokens already minted stay valid
  until they expire (at most an hour). A tenant may set `device_revoke_token_file` (0600)
  or `device_revoke_token_sha256`: that token opens this route for that
  tenant only (no other admin route, no other tenant), so a serve box never
  holds the operator token. No env var form.
- **Push to more than one app build.** `LIGUS_RELAY_APNS_TOPICS` (comma
  list) allow-lists more bundle ids next to `LIGUS_RELAY_APNS_TOPIC` (which
  keeps working alone). The phone may report its bundle id with its token
  (`apns.topic`, optional); the relay pushes with it when allowed and refuses
  otherwise, keeping the token. A phone that reports none gets the default
  topic, and `DeviceTokenNotForTopic` for it no longer drops its token.
  Postgres gains `relay_tunnel_push_tokens.topic` (added in place).
- **Push results are logged.** Every host push is logged at info as `tunnel
  push` with its result and, for APNs, env, topic, status, reason and apns-id
  (never the device token). A host push may set `"diag": true` to get the same
  as `"detail"` in its ack; acks without it are byte-identical.

- **Contacts, `/v1/contacts/*`.** An opaque, client-signed contact record per
  random contact id (16 bytes, base64url), owned by a 32-byte write-key
  capability whose SHA-256 the relay stores, plus a sealed, append-only drop
  box per contact that only the key holder reads or empties. The relay does no
  cryptography on either. Size caps (64 KiB record and drop), a 30-day record
  TTL refreshed on every write, per-box row (5000) and byte (4 MiB) caps
  (eviction takes drops already served to the owner first, then the heaviest
  sender's unread drops, so a flood evicts itself and not a pending invite),
  30-day drop retention, and rate limits per sender and
  box, per box, per record and per client. Stored in the memory store, its
  snapshot and Postgres (`relay_contacts`, `relay_contact_drop`). Requests that
  carry a forge token are refused. Settings: `LIGUS_RELAY_CONTACTS`,
  `LIGUS_RELAY_CONTACT_*`, `LIGUS_RELAY_MAX_CONTACTS`; see README "Contacts".
  - An expired cid stays reserved for its old write key for 180 days
    (`LIGUS_RELAY_CONTACT_RECLAIM_HOLD_DAYS`), so nobody who learned the cid
    can claim it while its owner is offline.
  - Drop-box seqs never restart for a cid: a box created again starts after
    its creation second times 2^20, so every device's cursor and ack stay
    correct.
  - Records and drops must be valid UTF-8 (`400` otherwise), so they come back
    byte for byte from every store, across restarts.
  - New records are also limited per IPv6 /48 (16x the per-client limit).
- `ContactStore`, an optional store interface (like `TunnelStore`): a
  downstream `Store` without it keeps compiling and serves `404` on contacts.
- `/v1/contacts/` joins the reserved extension prefixes.
- Helm chart: `contacts.*` and `githubRoutes` values.
- **Helm charts: one database choice, `database.mode`** (ligus-relay and
  ligus-auth, from a shared `deploy/helm/ligus-postgres` library chart):
  `external` (a Postgres you run; DSN Secret, egress `cidrs`), `cnpg` (a
  CloudNativePG `Cluster` in the release namespace: TLS `verify-full` against
  the operator's CA, 1 or 3+ instances, WAL archiving and scheduled backups
  to object storage, optional PodMonitor; needs the operator) and `bundled`
  (one hardened Postgres 16 pod: digest-pinned, Pod Security `restricted`,
  `ReadWriteOncePod`, TLS required from a Secret or cert-manager, a
  non-superuser role with `scram-sha-256`, a `pg_dump` CronJob with
  retention and a values-driven restore). For `cnpg` and `bundled` the DSN
  carries no password: pgx reads a mounted `pgpass` file and CA. Backups can
  only be turned off with `acknowledgeNoBackups`. The egress NetworkPolicy
  allows only the chosen database. See the chart README, "Database".

### Deprecated, and off by default

- **GitHub social routes** (`/v1/directory/*`, `/v1/account/*`,
  `/v1/friends/*`). They need the relay to see users' GitHub tokens, and
  Ligus 2.0 replaces them with contacts, invite links and QR codes. They are
  now registered only with `LIGUS_RELAY_GITHUB_ROUTES=on` (also
  `HIVE_RELAY_GITHUB_ROUTES`, or `LIGUS_RELAY_GITHUB_SOCIAL=1`); otherwise they
  answer `404`. When on, responses carry `Deprecation` and `Link:
  rel="deprecation"` headers. **They are removed in v0.4**, together with the
  `relay_directory*`, `relay_account*`, `relay_login_index`, `relay_inbox` and
  `relay_friend*` tables and the GitHub verifier.
  *Upgrade:* a relay serving Hive/Ligus 1.x clients that use
  invite-by-handle, friends or the account inbox must set
  `LIGUS_RELAY_GITHUB_ROUTES=on` until those clients are gone.

- **Helm: `postgres.enabled`, `externalDatabase.*` (ligus-relay) and
  `store.databaseUrl` (ligus-auth)** are deprecated aliases for
  `database.mode` (`bundled` in its old development form, `external`). They
  render exactly as before when `database.mode` is empty, with a note at
  install; the README has the move from `postgres.enabled` to a real mode.

### Removed

- **The downstream build hooks** `MainWith`, `Extension`, `Addon` and
  `Assemble`, and the `Options.TunnelBilling` override. The relay binary is
  `Main()`; a custom build still composes `New(Options{...})` with the seams
  (`TunnelPolicy`, `PushSender`, `EntitlementVerifier`, `WriteGuard`, `Hooks`).
  `GET /v1/tunnel/info` keeps its `billing` field, now always `"none"`, so
  clients that read it see the same bytes as before.

### Also in this release

- **ligus-auth** (`cmd/ligus-auth`): optional OIDC sign-in service that mints
  short-lived `hrt1` relay tokens, with SCIM 2.0 deprovisioning, per-tenant
  IdPs and workspace grants, device-bound rotating refresh tokens, a distroless
  image (`deploy/container/Dockerfile.ligus-auth`), a Helm chart
  (`deploy/helm/ligus-auth`) and compose examples for Pocket ID and Dex. It
  gates relay access only; workspace membership stays in the signed roster.
- **Token claims `tid`, `uid`, `did`** (all optional and omitted when empty):
  tokens minted without them are byte-identical to before.
- **`LIGUS_RELAY_TENANTS`** (off by default): confine signed tokens to the
  listed `tid` values.
- **`LIGUS_RELAY_TOKEN_PUBKEY_NEXT`** (off by default): accept a second issuer
  key during a key rotation.
- `ligus-relay issue --tenant <id>` sets `tid`.
- Exported `IssueToken`, `VerifyToken`, `ParsePubkey` and `ParseSigningKey`.

## v0.2.0

Ligus Relay is the relay formerly called `hive-relay`. Ligus (LEE-gus) is short
for *ligustica*, the Italian honeybee. The `/v1` API, `hrt1` tokens, `X-Hive-*`
headers, the `hive-tunnel.1` subprotocol and the `hive_relay_*` metric names are
unchanged: existing clients keep working against this relay, except where the
security changes below say otherwise.

### Renamed

- **Module path** is `github.com/ligus-ai/relay`. `github.com/honeyhive-ai/relay`
  (v0.1.x) is deprecated; update imports and `go.mod`.
- **Binary** is `ligus-relay` (`cmd/ligus-relay`). `cmd/hive-relay` still
  builds, and the repository-root (Alpine) image keeps a `hive-relay` symlink,
  until Ligus v3.0. The distroless image (`deploy/container/Dockerfile`) ships
  only `ligus-relay`.
- **Settings** are named `LIGUS_RELAY_*`. Every `HIVE_RELAY_*` name still
  works: if both are set the new name wins, and the relay logs a one-time
  warning per legacy name in use.

### Security changes that affect clients

- **GitHub-backed routes no longer read the `Authorization` bearer.** The
  directory, account and friends routes used to fall back to the bearer when
  no GitHub token header was sent, and verified it against `api.github.com`,
  so a relay access token could reach GitHub. Only `X-Hive-Github-Token` (or
  the new `X-Ligus-Github-Token`) is used now; a request with only a bearer
  gets `401`. Ligus 2.0 clients already send `X-Hive-Github-Token`.
  *Residual:* the relay operator can still see the GitHub tokens sent to these
  routes; a client change to stop sending the raw token is pending.
- **Pairing payloads are restricted.** `POST /v1/pair` takes only a peer
  friend code (`hive_` + base64url of 32 bytes, or `hivepeer1:` + 64 hex) or a
  sealed payload (`sealed1:` + base64url, at least 32 bytes). `hivews1:` and
  `ligusws1:` workspace invites, which carry the workspace key in cleartext,
  and anything else get `422`. Ligus 2.0 clients publish only friend codes
  here and already refuse to broker workspace invites; Hive 1.x clients that
  published `hivews1:` codes through the relay can no longer do so.
- **Pairing codes are single-use.** The first successful resolve consumes the
  code; a second resolve gets `404`. The TTL (default 600 s, 30–3600 s) is
  unchanged.
- **Keyring rotations past the caps are refused, not evicted.** Both stores
  used to keep only the newest rotations past the per-workspace cap (and
  Postgres pruned the same way hourly), so junk rotations could push a
  workspace's real rotations out. Past `LIGUS_RELAY_MAX_KEYRING_PER_WS`
  (2000) rotations or `LIGUS_RELAY_MAX_KEYRING_BYTES_PER_WS` (16 MiB) a new
  rotation gets `507` and the keyring is unchanged. A single rotation over
  `LIGUS_RELAY_MAX_ROTATION_BYTES` (256 KiB) gets `413`. The wire format is
  unchanged.
- **Workspace reads are rate limited per client** (`LIGUS_RELAY_READ_RATE`
  100/s, `LIGUS_RELAY_READ_BURST` 400; `429` past it), and **SSE streams are
  capped per client** (`LIGUS_RELAY_MAX_SSE_PER_CLIENT`, 100; `429`).
- **Idempotency keys are scoped to the writer.** A client-chosen
  `Idempotency-Key` or `X-Hive-Event-Id` is stored as a hash of (writer, key),
  so one writer can't claim another's key and turn that writer's push into a
  silent duplicate. Body-hash dedup (no header) is unchanged. Ligus 2.0 clients
  send neither header.

### Other security fixes

- Admin request bodies are capped at 16 KiB, and admin and friend handlers log
  store errors instead of returning them. The membership claim body is bounded.
- Rate limits and abuse blocks key IPv6 clients on their /64. A client-supplied
  `Fly-Client-IP` is ignored off Fly.io; `LIGUS_RELAY_TRUSTED_PROXIES` and
  `LIGUS_RELAY_CLIENT_IP_HEADER` name the proxies whose forwarding header is
  trusted.
- Membership roles can't be raised above the caller's own or used against
  higher ranks, and a claimed workspace's roster is readable by its members
  only.
- Opt-in per-workspace token scope from a signed `workspaces` claim
  (`LIGUS_RELAY_WORKSPACE_SCOPE`).
- Optional heap budget for the memory store (`LIGUS_RELAY_MAX_STORE_BYTES`),
  charged with each workspace's overhead.
- Bounded workspace and device id lengths, stored idempotency keys, and
  keyring, candidate and presence list responses (`LIGUS_RELAY_MAX_LIST_BYTES`).
- The memory-store snapshot is written owner-only.
- A malformed `LIGUS_RELAY_TOKEN_PUBKEY` fails closed (denies every token).
- Pairing-code sweeps run at most once a second, and the per-client resolve
  block is checked first.

### Added since v0.1.0

- Relay tunnel (`hive-tunnel/1`) for phone ↔ home host remote access, off by
  default, with an optional built-in APNs push sender.
- `MainWith` / `Assemble` extension point, so a downstream binary is a strict
  superset of the open relay; `Server.RefreshTunnelStatus`.
- Relay-managed team membership (roster, claim, invites, join) behind
  `LIGUS_RELAY_MEMBERSHIP`, with a membership identity key
  (`LIGUS_RELAY_MEMBERSHIP_PUBKEY`) independent of the access gate.
- `ligus-relay bootstrap-agent` to stand up a remote agent with one line.
- Durable users and tokens with an admin API (disabled unless a downstream
  build supplies an `AdminAuthorizer`), and a public status page at `/`.
- Bounded envelope pages (`X-Hive-Next-After` / `X-Hive-More`).
- SSE push (`GET /v1/workspaces/{id}/events`), idempotent envelope push,
  structured request logs and `/metrics`.
- Workspace reads require the same access token as writes.
- Postgres: retention, pooler-safe statements, primary pinning, text-bound
  blobs.
- Helm chart (`deploy/helm/ligus-relay`, chart version 0.2.0) and a non-root
  distroless image. Defaults: tunnel off; liveness doesn't depend on the
  database; the APNs key reaches only the tunnel pod, from a Secret of its own
  (the chart refuses `config.existingSecret` for it and `*_RELAY_APNS_*` keys
  in `config.env`); DNS egress only to the cluster resolver
  (`networkPolicy.dns.*`, default `kube-system` pods labeled
  `k8s-app: kube-dns`).

### New settings

All are also read under their `HIVE_RELAY_*` names. See the README's
configuration tables for every setting.

| Setting | Default |
|---|---|
| `LIGUS_RELAY_READ_RATE` / `LIGUS_RELAY_READ_BURST` | `100` / `400` |
| `LIGUS_RELAY_MAX_SSE_PER_CLIENT` | `100` |
| `LIGUS_RELAY_MAX_KEYRING_BYTES_PER_WS` | `16777216` |
| `LIGUS_RELAY_MAX_ROTATION_BYTES` | `262144` |
| `LIGUS_RELAY_MAX_KEYRING_PER_WS` | `2000` (now refuses instead of evicting) |
| `LIGUS_RELAY_MAX_LIST_BYTES` | `16777216` |
| `LIGUS_RELAY_MAX_STORE_BYTES` | `0` (unbounded) |
| `LIGUS_RELAY_MAX_BODY_BYTES` | `4194304` |
| `LIGUS_RELAY_MAX_ENVELOPES` / `LIGUS_RELAY_RETENTION_DAYS` | `50000` / `0` |
| `LIGUS_RELAY_MAX_DEVICE_BLOBS_PER_WS` | `1000` |
| `LIGUS_RELAY_MAX_PAIRINGS` | `50000` |
| `LIGUS_RELAY_MAX_SSE` | `2000` |
| `LIGUS_RELAY_PUSH_RATE` / `_BURST` | `100` / `300` |
| `LIGUS_RELAY_WS_WRITER_RATE` / `_BURST` | `20` / `200` |
| `LIGUS_RELAY_GITHUB_RATE` / `_BURST` | `30` / `120` |
| `LIGUS_RELAY_INVITE_RATE` / `_BURST` | `0.05` / `10` |
| `LIGUS_RELAY_INVITE_REQUIRE_FRIEND` | off |
| `LIGUS_RELAY_INBOX_MAX_ROWS` | `5000` |
| `LIGUS_RELAY_INBOX_RETENTION_DAYS` / `LIGUS_RELAY_FRIEND_REQ_RETENTION_DAYS` | `30` / `30` |
| `LIGUS_RELAY_TRUSTED_PROXIES` / `LIGUS_RELAY_CLIENT_IP_HEADER` | empty / `X-Forwarded-For` |
| `LIGUS_RELAY_WORKSPACE_SCOPE` | `off` |
| `LIGUS_RELAY_MEMBERSHIP` / `LIGUS_RELAY_MEMBERSHIP_PUBKEY` | off / empty |
| `LIGUS_RELAY_METRICS_TOKEN` | empty (open) |
| `LIGUS_RELAY_REQUIRE_DB` | off |
| `LIGUS_RELAY_TUNNEL` / `LIGUS_RELAY_TUNNEL_HOSTS` | `off` / empty |
| `LIGUS_RELAY_APNS_KEY_FILE`, `_KEY`, `_KEY_ID`, `_TEAM_ID`, `_TOPIC`, `_ENV` | empty, `ENV` = `auto` |

### Upgrade notes for self-hosters

1. **Names.** Nothing breaks if you keep `HIVE_RELAY_*`, the `hive-relay`
   binary name or the old module path for now, but rename them: the relay logs
   a warning per legacy setting, and the aliases go away in Ligus v3.0. If you
   build from source, change imports to `github.com/ligus-ai/relay`.
2. **Behind a reverse proxy or ingress, set `LIGUS_RELAY_TRUSTED_PROXIES`.**
   Without it every client shares the proxy's address, and so shares one
   bucket for the new read limit (100/s) and one SSE allowance (100 streams),
   as well as the existing write limits. Raise `LIGUS_RELAY_READ_RATE` and
   `LIGUS_RELAY_MAX_SSE_PER_CLIENT` if you can't.
3. **Pairing.** Codes created before the upgrade live in memory and are gone
   after the restart anyway. Hive 1.x clients that shared workspaces through
   short codes must share the `hivews1:` invite directly (Ligus 2.0 already
   does).
4. **Keyring.** A workspace already over 2000 rotations or 16 MiB keeps what it
   has, but new rotations get `507` until you raise
   `LIGUS_RELAY_MAX_KEYRING_PER_WS` / `LIGUS_RELAY_MAX_KEYRING_BYTES_PER_WS`.
   Postgres no longer prunes the keyring hourly. If you raise the byte cap,
   raise `LIGUS_RELAY_MAX_LIST_BYTES` to match.
5. **Idempotency keys.** Stored keys change form, so a re-push with a header
   of an envelope pushed before the upgrade is appended once more. Clients
   dedupe events by id, so this is harmless.
6. **GitHub routes.** A client that sent its GitHub token only as the
   `Authorization` bearer must send `X-Hive-Github-Token` instead.
7. **Helm.** If your APNs key was in `config.existingSecret` or in
   `config.env`, move it to a Secret of its own and set
   `tunnel.apns.existingSecret`. If your cluster's resolver pods aren't
   labeled `k8s-app: kube-dns` in `kube-system`, set
   `networkPolicy.dns.namespaceSelector` / `podSelector`, or DNS stops
   resolving with `networkPolicy.restrictEgress` on.

## v0.1.0

First release of the Go relay at `github.com/honeyhive-ai/relay`, at parity
with the Rust reference relay's `/v1` API and `hrt1` tokens.
