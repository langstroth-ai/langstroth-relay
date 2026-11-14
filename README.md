# Ligus Relay

A small, **content-blind** rendezvous + envelope-forwarding server for
[Ligus](https://ligus.ai) ([source](https://github.com/ligus-ai/ligus),
[docs](https://docs.ligus.ai)). Ligus (LEE-gus) is short for
*ligustica*, as in *Apis mellifera ligustica*, the Italian honeybee; the project
was called Hive until v2.0, and this relay was `hive-relay`.

Clients post opaque (end-to-end encrypted) event envelopes keyed by workspace
and fetch everything after a cursor; the relay also brokers short pairing codes,
an identity directory, a poll-based account inbox, the friend graph, and
presence. **It never sees plaintext.**

> **Renamed from Hive.** The Go module is now `github.com/ligus-ai/relay`
> (v0.2.0+; `github.com/honeyhive-ai/relay` v0.1.x is deprecated), the binary is
> `ligus-relay` (the container image keeps a `hive-relay` alias, and
> `cmd/hive-relay` still builds, until v3.0), and settings are named
> `LIGUS_RELAY_*`. Every legacy `HIVE_RELAY_*` name still works: the new name
> wins if both are set, and the relay logs a one-time deprecation warning per
> legacy name. Wire formats do not change: the `/v1` API, `hrt1` tokens,
> `X-Hive-*` headers, the `hive-tunnel.1` subprotocol and the `hive_relay_*`
> metric names are protocol identifiers from the project's former name.

This is the production relay (Go). It speaks the same JSON `/v1` contract and
`hrt1` entitlement-token format as the original Rust reference relay
(`crates/hive-relay`), which is now kept only as an in-process test fixture for
the Rust client (in the Ligus app repo).

## Run

```sh
go run ./cmd/ligus-relay            # serves on :8443 (in-memory)
# or build:
go build -o ligus-relay ./cmd/ligus-relay && ./ligus-relay
```

Operator commands (mint/verify signed entitlement tokens — the serving relay
only ever *verifies*):

```sh
ligus-relay keygen                  # prints an Ed25519 issuer keypair
ligus-relay issue --key <priv-hex> --sub <id> --plan team --exp-days 365 \
  --max-members 50 --turn --cap remove_member
```

## Configuration (env)

Each `LIGUS_RELAY_*` setting below is also read under its legacy
`HIVE_RELAY_*` name (the new name wins if both are set). Rates are tokens per
second per client; a client is the TCP peer, or the address a trusted proxy
names (`LIGUS_RELAY_TRUSTED_PROXIES`), with IPv6 keyed on its /64.

**Core and access**

| Var | Default | Meaning |
|---|---|---|
| `PORT` / `LIGUS_RELAY_ADDR` | `0.0.0.0:8443` | bind address (`$PORT` wins) |
| `DATABASE_URL` | empty | shared **Postgres** store → horizontal scaling / HA (no data migration) |
| `LIGUS_RELAY_DATA_DIR` | empty | in-memory store + JSON snapshot here (single instance). Ignored if `DATABASE_URL` is set |
| `LIGUS_RELAY_REQUIRE_DB` | off | `1`: refuse to start without `DATABASE_URL` instead of falling back to memory |
| `LIGUS_RELAY_TOKEN_PUBKEY` | empty | Ed25519 public key → require **signed** entitlement tokens. A key that doesn't parse denies every token |
| `LIGUS_RELAY_ACCESS_TOKENS` | empty | comma-separated static allowlist (coarse gate) |
| `LIGUS_RELAY_WORKSPACE_SCOPE` | `off` | `off`, `claims` or `strict`: confine signed tokens to the workspace ids in their `workspaces` claim. See [Optional access gating](#optional-access-gating) |
| `LIGUS_RELAY_MEMBERSHIP` | off | `1`: relay-managed team membership (roster, claim, invites, join) |
| `LIGUS_RELAY_MEMBERSHIP_PUBKEY` | empty | Ed25519 key whose signed tokens identify callers for membership on an otherwise open relay |
| `LIGUS_RELAY_METRICS_TOKEN` | empty | bearer required on `/metrics` (empty = open) |
| `LIGUS_RELAY_FRIEND_CAP` | none | max accepted friends per account |
| `LIGUS_RELAY_INVITE_REQUIRE_FRIEND` | off | `1`: `POST /v1/account/invites` only reaches existing friends |
| `LIGUS_RELAY_TRUSTED_PROXIES` | empty | comma-separated CIDRs/IPs of reverse proxies whose forwarding header names the client (empty = use the TCP peer, ignore every forwarding header). Set it behind an ingress or load balancer, or every client shares one rate-limit bucket. On Fly.io (`FLY_APP_NAME` set) with this unset, `Fly-Client-IP` is used |
| `LIGUS_RELAY_CLIENT_IP_HEADER` | `X-Forwarded-For` | header read from a trusted proxy (walked right to left past trusted hops), or a single-address header such as `X-Real-IP` |

**Storage, retention and size bounds**

| Var | Default | Meaning |
|---|---|---|
| `LIGUS_RELAY_MAX_ENVELOPES` | `50000` | per-workspace retained-envelope cap (`0` = unbounded). The memory store enforces it on write; Postgres prunes to it hourly |
| `LIGUS_RELAY_RETENTION_DAYS` | `0` (off) | prune envelopes older than N days; both stores |
| `LIGUS_RELAY_MAX_STORE_BYTES` | `0` (unbounded) | memory store: cap on workspace bytes held across all workspaces (envelope bodies, keyring rows, candidate and presence blobs, per-workspace overhead). Writes past it answer `507`. Set it below your memory limit on any relay reachable by untrusted clients |
| `LIGUS_RELAY_MAX_BODY_BYTES` | `4194304` (4 MiB) | max JSON request-body size |
| `LIGUS_RELAY_MAX_LIST_BYTES` | `16777216` (16 MiB) | bound on one `GET …/keyring`, `…/candidates` or `…/presence` response, by stored blob bytes (`0` = unbounded). At least one row is always returned |
| `LIGUS_RELAY_MAX_KEYRING_PER_WS` | `2000` | key rotations stored per workspace. Past it a new rotation gets `507`; stored rotations are never evicted |
| `LIGUS_RELAY_MAX_KEYRING_BYTES_PER_WS` | `16777216` (16 MiB) | stored keyring bytes per workspace; `507` past it. Keep it at or below `LIGUS_RELAY_MAX_LIST_BYTES` so `GET …/keyring` always returns the whole keyring |
| `LIGUS_RELAY_MAX_ROTATION_BYTES` | `262144` (256 KiB) | one key rotation's size; `413` past it (`0` = only the body cap) |
| `LIGUS_RELAY_MAX_DEVICE_BLOBS_PER_WS` | `1000` | candidate and presence device entries per workspace |
| `LIGUS_RELAY_MAX_PAIRINGS` | `50000` | live pairing codes per instance (`503` past it) |
| `LIGUS_RELAY_INBOX_MAX_ROWS` | `5000` | account-inbox rows per recipient (oldest evicted) |
| `LIGUS_RELAY_INBOX_RETENTION_DAYS` | `30` | Postgres: prune inbox rows older than N days (`0` = off) |
| `LIGUS_RELAY_FRIEND_REQ_RETENTION_DAYS` | `30` | Postgres: prune answered friend requests older than N days (`0` = off) |

**Rate and concurrency limits** (a rate of `0` disables that limit)

| Var | Default | Meaning |
|---|---|---|
| `LIGUS_RELAY_PUSH_RATE` / `_BURST` | `100` / `300` | every unauthenticated write (envelopes, keyring, presence, candidates, pairing) per client |
| `LIGUS_RELAY_WS_WRITER_RATE` / `_BURST` | `20` / `200` | envelope appends per (workspace, writer) |
| `LIGUS_RELAY_READ_RATE` / `_BURST` | `100` / `400` | workspace reads (envelopes, keyring, presence, candidates, members, SSE connects) per client; `429` past it |
| `LIGUS_RELAY_MAX_SSE` | `2000` | concurrent SSE streams per instance (`503` past it; `0` = unlimited) |
| `LIGUS_RELAY_MAX_SSE_PER_CLIENT` | `100` | concurrent SSE streams per client (token subject, else address; `429` past it; `0` = unlimited) |
| `LIGUS_RELAY_GITHUB_RATE` / `_BURST` | `30` / `120` | GitHub-backed routes (directory, account, friends) per client |
| `LIGUS_RELAY_INVITE_RATE` / `_BURST` | `0.05` / `10` | account invites per (sender, recipient) |

**Relay tunnel and push**

| Var | Default | Meaning |
|---|---|---|
| `LIGUS_RELAY_TUNNEL` | `off` | relay tunnel (phone ↔ home host): `off` (`/v1/tunnel/*` → 404), `open` (any host may register), `allowlist` (only `LIGUS_RELAY_TUNNEL_HOSTS`). See [Relay tunnel](#relay-tunnel-hive-tunnel1) |
| `LIGUS_RELAY_TUNNEL_HOSTS` | empty | comma-separated `host_id`s admitted when `LIGUS_RELAY_TUNNEL=allowlist` |
| `LIGUS_RELAY_APNS_KEY_FILE` | empty | APNs auth key (`AuthKey_<KEYID>.p8`) for the built-in push sender; or `LIGUS_RELAY_APNS_KEY` with the PEM inline (`\n` escapes allowed). Push is **off** unless the key and the three vars below are all set. See [Push from your own relay](#push-from-your-own-relay) |
| `LIGUS_RELAY_APNS_KEY_ID` / `LIGUS_RELAY_APNS_TEAM_ID` | empty | the key's Key ID and your Apple developer Team ID |
| `LIGUS_RELAY_APNS_TOPIC` | empty | bundle id of the iPhone app build you publish (the APNs topic) |
| `LIGUS_RELAY_APNS_ENV` | `auto` | `auto` (the environment each phone reports with its token: `development` → sandbox, `production` → production), `sandbox` or `production` |

Storage selection: `DATABASE_URL` → Postgres; else `LIGUS_RELAY_DATA_DIR` →
memory+snapshot; else in-memory only.

## Test

```sh
go test -race ./...                            # unit + HTTP + snapshot + seams (+ chart render tests when helm is installed)
TEST_DATABASE_URL=postgres://… go test ./...   # also runs Postgres integration
helm lint deploy/helm/ligus-relay              # the Kubernetes chart
```

## Upgrading

See [CHANGELOG.md](CHANGELOG.md) for each release's behavior changes and
upgrade notes for self-hosters.

## Deploy

A tiny static binary in a ~10 MB Alpine image — any container host works.
Clients require `https://`, so terminate TLS at the platform edge or a reverse
proxy.

### Docker (anywhere)

```sh
docker build -t ligus-relay .            # or straight from GitHub:
# docker build -t ligus-relay https://github.com/ligus-ai/relay.git
docker run -d -p 8443:8443 -v ligus-data:/data -e LIGUS_RELAY_DATA_DIR=/data ligus-relay
curl localhost:8443/v1/health           # → ok
```

### Managed platforms

No lock-in — it's a standard container. The binary honors `$PORT` (else
`$LIGUS_RELAY_ADDR`, else `0.0.0.0:8443`), so most PaaS work with zero config:
point Render / Railway / Cloud Run / Kubernetes / a VM at the image (or the
Dockerfile), attach a disk at `/data`, and set `LIGUS_RELAY_DATA_DIR=/data`.

A sample **`deploy/fly.toml`** is included as one worked example (`fly launch
--copy-config --no-deploy` → `fly volumes create hive_data …` → `fly deploy`) —
adapt it, or use your platform's equivalent. (The example volume keeps its
pre-rename name so existing deployments of the file keep their data.)

### TLS

Clients need `https://` — either let your platform terminate TLS at its edge, or
run any reverse proxy (Caddy example):

```caddyfile
relay.example.com {
    reverse_proxy localhost:8443
}
```

### Persistence & scaling

- **Single instance:** mount a volume at `/data` and set `LIGUS_RELAY_DATA_DIR=/data`
  (JSON snapshot store; survives restarts). Nothing to back up but that volume.
- **HA / multiple instances:** set `DATABASE_URL` to a shared Postgres — it takes
  precedence over the snapshot dir, so every instance shares state (no migration).

#### Envelope retention

The in-memory / snapshot store bounds how many envelopes it keeps per workspace
so a long-running self-host doesn't grow without limit and eventually OOM. By
default each workspace retains its newest `LIGUS_RELAY_MAX_ENVELOPES` (50 000)
envelopes; set `LIGUS_RELAY_RETENTION_DAYS` to also drop envelopes older than N
days. Set either to `0` to disable that bound (`0` for both = unbounded, the old
behavior).

**Tradeoff (vs. offline catch-up):** clients sync by fetching everything after a
cursor (`?after=seq`). The relay is a cache of *recent* E2EE traffic, not the
source of truth — pruning below a cursor means a device offline longer than the
retention window can't replay the gap from the relay and must re-sync from a peer
that still holds history. Size the bounds above your expected offline window.

The Postgres backend applies the same two settings, hourly rather than on write
(so a workspace can briefly exceed the cap). Its history is on disk rather than
in RAM, so it can afford a much larger cap; for unbounded history set
`LIGUS_RELAY_MAX_ENVELOPES=0` (and leave `LIGUS_RELAY_RETENTION_DAYS` at `0`)
and watch the database's disk.

### Envelope paging

`GET /v1/workspaces/{id}/envelopes?after=<seq>[&limit=<n>]` returns one bounded
page, oldest first: at most `limit` envelopes (default and maximum 1000) and, past
the first envelope, at most 4 MiB of stored body bytes. The body is the same bare
JSON array as always; two response headers describe the page:

- `X-Hive-Next-After: <seq>` — the cursor for the next request (the last seq in
  the page, or `after` when the page is empty);
- `X-Hive-More: true` — present only when envelopes past this page exist.

A client loops `after = X-Hive-Next-After` until `X-Hive-More` is absent. A client
that ignores the headers still converges: it sets its cursor to the highest seq it
received and its next poll continues from there.

### Idempotent push

`POST /v1/workspaces/{id}/envelopes` drops a re-push instead of appending it
twice and answers with the original `seq`. A re-push is recognized by the
client's `Idempotency-Key` (or `X-Hive-Event-Id`) header, scoped to the writer
(the token subject, else the client address), so one writer can't claim a key
another writer will use; without a header, by the SHA-256 of the sealed body.

### Live push (Server-Sent Events)

`GET /v1/workspaces/{id}/events` (`Content-Type: text/event-stream`) lets a
client wake instantly on new traffic instead of polling on a timer. It is a
content-blind **nudge** channel — never envelope bodies:

- on connect: `: connected` (a comment), then optionally `data: {"seq":<head>}`;
- on every new envelope appended to the workspace: `data: {"seq":<newSeq>}` —
  the client pulls with its own `?after=` cursor over `GET /envelopes`;
- every ~25s idle: a `: keep-alive` comment to survive proxy idle timeouts.

Read-authorized exactly like the other workspace reads (`enforceRead`), and
counted against the read rate. Each client may hold at most
`LIGUS_RELAY_MAX_SSE_PER_CLIENT` streams (`429` past it), and the instance at
most `LIGUS_RELAY_MAX_SSE` (`503`). Fan-out
is **single-instance / in-process** today; cross-instance push (Postgres
`LISTEN/NOTIFY`) is tracked as a follow-up, and multi-instance deployments keep
the `?after=` poll as the backstop.

### Pairing codes

`POST /v1/pair` `{payload, ttl_secs?}` stores a payload behind a short code
(`ttl_secs` 30–3600, default 600) and `GET /v1/pair/{code}` returns it. The
broker keeps payloads in cleartext in the relay's memory, so it takes only
payloads that are safe for the relay operator to read:

- a public peer friend code: `hive_` + base64url of a 32-byte public key, or
  the legacy `hivepeer1:` + 64 hex characters;
- a sealed payload: `sealed1:` + base64url of at least 32 bytes of ciphertext.
  The relay can't check that it is really encrypted; the prefix is the
  client's statement that it is.

Anything else gets `422`, including `hivews1:` / `ligusws1:` workspace
invites, which carry the workspace's end-to-end key: share those over a
channel you trust. A code is **single-use**: the first successful resolve
consumes it, and a second resolve gets `404`. Unresolved codes still expire at
their TTL. Failed resolves are capped per client (10 per minute, then `429`),
so the code space can't be enumerated.

### GitHub-backed routes

The identity directory, account and friends routes
(`/v1/directory/*`, `/v1/account/*`, `/v1/friends/*`) identify the caller by a
GitHub token, sent in `X-Hive-Github-Token` (or `X-Ligus-Github-Token`). The
relay verifies it against `api.github.com/user` and caches the result briefly
by the token's hash. Only that header is used: the `Authorization` bearer
carries the relay access token and is never sent to GitHub, so a request with
only a bearer gets `401`.

**Residual:** the relay operator can see the GitHub tokens clients send to
these routes, since the relay itself has to present them to GitHub. Use these
routes only with a relay you trust, and give the client a GitHub token with
the least scope it needs. Replacing the raw token with a proof the relay can
check without holding the token is a pending client change.

### Optional access gating

Open by default (self-host — the URL isn't a secret). To gate:

- **Allowlist:** `LIGUS_RELAY_ACCESS_TOKENS=tokA,tokB` (opaque bearer tokens).
- **Signed tokens:** set `LIGUS_RELAY_TOKEN_PUBKEY=<hex>` and mint per-subject
  tokens with `ligus-relay keygen` / `ligus-relay issue` — keep the issuer private
  key off the relay host (the relay only ever verifies).
- **Durable users + tokens:** manage per-person access over the admin API
  (below) instead of an env list — no redeploy, survives restarts, instant
  revocation. Pair it with `StoreEntitlementVerifier` to make the store the
  access gate.

Workspace keyrings are bounded by refusal, not eviction: past
`LIGUS_RELAY_MAX_KEYRING_PER_WS` rotations or `LIGUS_RELAY_MAX_KEYRING_BYTES_PER_WS`
bytes, a new rotation gets `507` and the stored ones stay, so a flood of junk
rotations can't push a workspace's real rotations out. One rotation is capped at
`LIGUS_RELAY_MAX_ROTATION_BYTES`.

Gating applies symmetrically to **reads and writes**: both `GET`
(envelopes / keyring / presence / candidates) and `POST` on a workspace require
the same entitlement (`enforceRead` mirrors `enforceWrite`). Under a token-gated
policy an unauthenticated read is `401`; under the open policy reads stay open,
just like writes. This is a coarse, content-blind token gate — it checks the
presented entitlement, never the body — so workspace ciphertext + keyring are no
longer readable by anyone who merely learns a workspace id. Clients already send
their access token on every request, so no client change is needed.

**Per-workspace token scope (opt-in, `LIGUS_RELAY_WORKSPACE_SCOPE`).** The
token gate is coarse: any entitled caller may read and write any workspace.
Signed tokens can carry an optional `workspaces` claim (`ligus-relay issue …
--workspace <id> [--workspace <id>]`, `*` = any). With
`LIGUS_RELAY_WORKSPACE_SCOPE=claims` a token that carries the claim is confined
to those ids on every `/v1/workspaces/{id}/…` route (envelopes, events,
keyring, presence, candidates, and the membership routes); tokens without it
keep coarse access. `strict` requires the claim on every workspace route (so
it needs signed tokens; the open policy and static allowlist get `403`). `off`
(default) ignores the claim. The claim lives inside the signed claims JSON, so
the `hrt1` format is unchanged, but a relay that doesn't enforce it (older, or
`off`) ignores it: a scoped token is only confined where the operator turns
this on. Relay-managed membership (`LIGUS_RELAY_MEMBERSHIP=1`) is the
alternative when the roster, not the issuer, should decide.

**Deferred (known, needs a product decision — not implemented here):**
- *High-entropy workspace ids.* Clients today derive a workspace id from a
  possibly-guessable room *name*. Making ids high-entropy (so an id can't be
  guessed at all) is a client-side + invite-model change, not a relay change.

### User/token admin API (`/v1/admin/*`)

The relay has a durable user + token store (both backends implement it) and a
management API, gated by the `AdminAuthorizer` seam. It's **disabled by
default** (no authorizer → `404`); supply one via `Options{AdminAuth: …}` to
enable it. The open `ligus-relay` binary has no environment switch for it
(there is no `LIGUS_RELAY_ADMIN_LOGINS`): it needs a downstream build. Tokens are stored only as SHA-256 hashes; the raw value is returned
once at creation.

- `POST /v1/admin/users` `{name, login?}` → create user + first token (raw once)
- `GET /v1/admin/users` → users + their tokens (no hashes)
- `POST /v1/admin/users/{id}/tokens` `{label?}` → another token (raw once)
- `POST /v1/admin/users/{id}/disabled` `{disabled}` → enable/disable a user
- `DELETE /v1/admin/tokens/{id}` → revoke a token

Admin request bodies are capped at 16 KiB (`413`). Store errors are logged, not
returned: a failing store answers `500 internal error`.

### Status page

`GET /` serves a small public HTML status page ("Ligus relay · online") so a
human who opens the URL sees an intentional page rather than a `401`. `GET
/v1/health` round-trips the store and returns `200 ok`, or `503` if the backend
is unreachable — wire it to your orchestrator's liveness/readiness probe.
Everything else is token-gated.

### Relay tunnel (`hive-tunnel/1`)

Lets the Ligus iPhone app reach a `ligus serve` home host through the relay —
no VPN, no inbound port on the host. The relay checks **who may connect** and
then forwards opaque bytes: phone and host run a Noise IK session end to end
through it, so the relay can't read, alter or forge traffic, and can't
impersonate either side. The protocol is open; the spec lives in the Ligus repo
(`docs/relay-tunnel.md`, with golden vectors in `docs/relay-tunnel/`).

**Off by default.** A relay upgraded to this version does not become a public
tunnel provider. Enable it with `LIGUS_RELAY_TUNNEL=open` (any host may
register) or `LIGUS_RELAY_TUNNEL=allowlist` + `LIGUS_RELAY_TUNNEL_HOSTS=<host_id>,…`.
When the relay's entitlement gate is configured (`LIGUS_RELAY_TOKEN_PUBKEY` /
`LIGUS_RELAY_ACCESS_TOKENS`), a host's first registration must also present a
token that passes it (`ligus remote enable --reg-token …`).

Endpoints (WebSockets require the subprotocol `hive-tunnel.1`, else `426`;
they authenticate themselves, so they bypass the bearer-token gate):

| Route | Who | What |
|---|---|---|
| `GET /v1/tunnel/info` | anyone | `{"versions":[1],"push":bool,"billing":"none"}` (`404` when off) |
| `GET /v1/tunnel/host` | host | control socket: Ed25519-signed challenge; `host_id = H(host key)`, registered implicitly on first connect |
| `GET /v1/tunnel/accept?sid=…` | host | one data socket per phone session (`sid` is single-use, valid 10 s) |
| `GET /v1/tunnel/connect?host=…` | phone | host-signed `hpc1` credential + proof-of-possession signature (or a pairing-window tag to enroll), then a 1:1 splice to the host |

What the relay stores (both backends; part of the snapshot / database):
registered hosts `(host_id, pub, created_at, last_seen)`, revoked phones
`(host_id, pid, exp)` until their credentials would have expired, and APNs push
tokens. It never holds a key that opens session traffic.

Limits (spec defaults): 8 KiB text / 65 535 B binary messages; 8 concurrent
sessions per host and 3 per phone; 60 sessions opened per host per minute; 30
connects per IP per minute, and 10 failed auths per minute blocks the IP for
5 minutes; 2 enrollment windows per host (≤ 15 min, 5 attempts each);
enrollment sessions capped at 5 min / 4 MiB; 20 control messages per second.
Every tunnel socket is pinged every 20 s and closed after 60 s of silence; the
`http.Server` read/write timeouts are cleared on upgrade. On shutdown every
tunnel socket is closed with `4011 shutting_down` (wired via
`http.Server.RegisterOnShutdown(srv.ShutdownTunnels)`).

**One instance per host.** Live control connections are kept in process: run a
single relay instance for the tunnel, or route `/v1/tunnel/connect?host=H` and
`/v1/tunnel/accept` to the instance holding H's control connection.

**Self-hosting the tunnel.** `LIGUS_RELAY_TUNNEL=open` gives your own relay the
same remote access the paid hosted relay offers, for free: every registered
host is `active`. `GET /v1/tunnel/info` then reports `"billing":"none"`, and
the Ligus app uses that to skip its subscription paywall (it shows "ask the
relay operator" on `4006` instead). Only the hosted build reports
`"billing":"appstore"`.

**Push on a self-hosted relay.** Without a `PushSender` host pushes are acked
`push_unavailable`, and `ligus serve` falls back to ntfy when it's configured.
APNs only accepts a push signed with a key from the Apple developer team that
publishes the app, so a relay can push only to builds of the Ligus app its
operator publishes (see below). It can't push to someone else's App Store
build. Proposed (not built): an optional push gateway run by the app's
publisher that accepts the same end-to-end encrypted `ct` from a self-hosted
relay and forwards it to APNs, so the gateway sees only ciphertext plus the
device token. It would need its own abuse controls (a per-relay key and rate
limits).

### Push from your own relay

If you build and sign the iPhone app under your own Apple developer account,
your relay can push to it with the built-in APNs sender:

1. In the Apple developer portal (Certificates, Identifiers & Profiles → Keys)
   create a key with **Apple Push Notifications service (APNs)** enabled.
   Download the `.p8` once and keep it secret; note its Key ID and your Team ID.
2. Run the relay with the tunnel on and the key configured:

   ```sh
   LIGUS_RELAY_TUNNEL=open \
   LIGUS_RELAY_APNS_KEY_FILE=/secrets/AuthKey_ABC123DEFG.p8 \
   LIGUS_RELAY_APNS_KEY_ID=ABC123DEFG \
   LIGUS_RELAY_APNS_TEAM_ID=<your team id> \
   LIGUS_RELAY_APNS_TOPIC=<your app's bundle id> \
   ligus-relay
   ```

   The relay logs `tunnel push via APNs (topic …, env auto)`, and
   `GET /v1/tunnel/info` and the host's `welcome` report `"push": true`. An
   incomplete configuration or a key that doesn't parse is logged and leaves
   push off; the relay still starts.
3. Build the app with push enabled (the iOS repo's docs), pair the phone with a
   host that uses this relay, and allow notifications.

The relay forwards the host's ciphertext untouched in the spec §14 payload
(`{"aps":{"alert":{"title":"Hive","body":"New activity on your host"},"mutable-content":1},"hive":{…,"ct":…}}`)
with `apns-push-type: alert`, priority 10 (`high`) or 5 (`normal`),
`apns-expiration` now + 1 h and the host's opaque collapse id. When APNs says
a device token is dead (`410 Unregistered`, `BadDeviceToken`,
`DeviceTokenNotForTopic`) the relay forgets it; the phone sends a fresh one
on its next connect. One relay serves one topic, so point
`LIGUS_RELAY_APNS_TOPIC` at the build you use (a dev build and an App Store
build have different bundle ids). With `LIGUS_RELAY_APNS_ENV=auto` a debug
build from Xcode (sandbox token) and a TestFlight/App Store build
(production token) both work.

Hosted deployments inject their own `TunnelPolicy` (entitlement: hosts can be
`pending` — enrollment works, sessions get `4006`; pending hosts unseen for 30
days are forgotten) and an APNs `PushSender`, through `MainWith` (below).

## Extending (seams)

This package is a complete relay on its own. It also exposes extension points
(see `seams.go`) so a downstream build can add custom behavior via
`New(Options{...})` without forking.

A downstream binary that must behave exactly like `cmd/ligus-relay` plus a few
additions (for example a hosted build that shares an app with the open relay)
calls `relay.MainWith(relay.Extension{...})` instead of `relay.Main()`.
Storage, env, the entitlement gate, every open route, subcommands and shutdown
are the open binary's; the extension can only set the tunnel seams, serve extra
routes under `/v1/<name>/` prefixes the relay doesn't use, and add
subcommands. If its setup fails, the relay logs it and serves as the open
binary. `relay.Assemble` builds the same handler without a listener, for
side-by-side tests.

Seams:

- **`Store`** — durable backend (in-memory/snapshot or Postgres built in);
  includes the user/token store behind the admin API.
- **`EntitlementVerifier`** — admission policy (open / allowlist / signed from
  env, `StoreEntitlementVerifier` for managed tokens, or your own).
- **`AdminAuthorizer`** — gates `/v1/admin/*` user management (`nil` = disabled).
- **`WriteGuard`** — optional pre-write authorization hook (`nil` = content-blind).
- **`Hooks`** — optional lifecycle observers (e.g. audit / accounting; no-op by
  default).
- **`TunnelPolicy`** / **`PushSender`** — relay-tunnel host admission and
  entitlement (`nil` = `LIGUS_RELAY_TUNNEL` env policy), and end-to-end
  encrypted push delivery (`nil` = the built-in APNs sender when
  `LIGUS_RELAY_APNS_*` is set, else unavailable). The sender itself is the
  importable `github.com/ligus-ai/relay/apns` package; a sender that wraps
  `relay.ErrPushTokenGone` makes the relay forget that phone's token. A `Store` that also implements
  `TunnelStore` keeps tunnel state durable (both built-in backends do).

## License

MIT — see `LICENSE`.
