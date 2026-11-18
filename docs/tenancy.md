# Tenancy and community limits (design delta)

Status: implemented in v0.3.0, off by default. This document fills in what
the Ligus multi-tenant design page (`docs-site/docs/operate/deploy/multi-tenant.md`
in the Ligus repo) leaves open, for this relay only. That page describes three
layers (the Ligus roster and keys, relay tokens with tenant claims, the
tenant's SSO and SCIM); this delta covers layer 2 and the storage, rate and
metrics limits that make one relay safe to share.

Two separate features:

1. **Community limits.** Per-room and per-client quotas for a public relay
   anyone can use (the owner's relay at `relay.langstroth.ai`, one process).
   No tenants; every client shares one namespace.
2. **Tenancy.** One deployment serving several organisations that don't
   trust each other. Each tenant gets its own room namespace, quotas, rate
   limits, admin scope and metrics.

Both stay **single replica**: tenancy is logical isolation inside one
process, not high availability. Pairing codes, SSE fan-out, rate limiters and
tunnel control connections are in-process state, as before.

## What a client needs from the relay (retention constraint)

Studied in the Ligus client (`crates/hive-runtime/src/sync_engine.rs`,
`hive-core/src/events.rs`, `envelope_verifier.rs`, `relay_client.rs`):

- A device rebuilds a workspace by folding the **whole** envelope log from
  seq 1: the founder's signed creation seed (`SessionSnapshot`), every
  `MemberAdded`, `AccountKeyRegistered`/`Rotated`, `DeviceCertified` and
  `WorkspaceKeyRotated` event. Compaction snapshots do not replace history
  (a non-seed snapshot folds as a no-op).
- Every event names a Lamport `log_head`; an event whose head was pruned
  stays unresolved forever.
- The client never detects a gap and never backfills from a peer. A pruned
  prefix fails silently for a new or long-offline device.
- Envelopes are `SealedEnvelope {version, nonce, ciphertext, kid?}`. The
  client sends no header that marks an envelope's kind, so the relay cannot
  tell a seed or governance event from a chat message.
- The keyring route is an accelerator only; the config log carries every
  rotation.
- The client treats 429 and 507 as a generic error and retries on its next
  tick; it does not read `Retry-After`.

Consequences for this design:

- **Retention never deletes envelopes from an active room.** The only safe
  unit of deletion is the whole room: `LANGSTROTH_RELAY_ROOM_RETENTION_DAYS`
  deletes a room's envelopes, key rotations and device blobs once nothing
  has been appended to it (envelope or key rotation) for N days. A room that
  is written to keeps its whole log, however old.
- An expired room keeps its sequence counter (a returning device's cursor
  stays valid: new envelopes get higher seqs) and, in membership mode, its
  roster and invites (dropping the roster would unclaim the room and let
  anyone claim it).
- The per-envelope bounds `LANGSTROTH_RELAY_MAX_ENVELOPES` (rolling window,
  once on by default at 50 000) and `LANGSTROTH_RELAY_RETENTION_DAYS` (age)
  **broke every new joiner** of a room larger or older than the bound, so
  they are removed: the relay refuses to start while either is set to
  anything but `0`. Rooms are bounded by quota (refusal, 507) instead.
- A future client could mark its seed and governance envelopes so a relay
  could prune chat traffic around them; that needs a client change and leaks
  event classes to the relay, so it is out of scope here.

## Community limits

All off (0) by default, so a self-hoster sees no change. Over a quota the
relay answers **507** (storage) or **429** (rate, room count) and counts the
refusal in `hive_relay_limit_rejections_total{limit}`.

| Setting | Unit | Refusal |
|---|---|---|
| `LANGSTROTH_RELAY_ROOM_MAX_BYTES` | stored bytes per room: envelope bodies + key rotations | 507 `room_bytes` |
| `LANGSTROTH_RELAY_ROOM_MAX_ENVELOPES` | stored envelopes per room | 507 `room_envelopes` |
| `LANGSTROTH_RELAY_MAX_TOTAL_ROOM_BYTES` | sum of room bytes across the relay, both backends. Postgres re-reads the sum at most every 10 s and adds its own writes, so it can overshoot by what another writer adds in that window (none on one replica) | 507 `total_bytes` |
| `LANGSTROTH_RELAY_MAX_ROOMS` | live rooms across the relay, both backends (Postgres re-reads the count at most every 10 s). `MAX_ROOMS_PER_CLIENT` keys on the address without a token, so a client rotating addresses (IPv6 /64s, IPv4 pools) is only bounded by this | 507 `total_rooms` |
| `LANGSTROTH_RELAY_MAX_STORE_BYTES` | unchanged: memory store only, every workspace byte | 507 `store_bytes` |
| `LANGSTROTH_RELAY_ROOM_RETENTION_DAYS` | delete a room idle for N days (above) | none |
| `LANGSTROTH_RELAY_IP_RATE` / `_BURST` | every request per client address (IPv6 /64), before authentication | 429 `ip_rate` |
| `LANGSTROTH_RELAY_ROOM_RATE` / `_BURST` | every request to one room, all clients together | 429 `room_rate` |
| `LANGSTROTH_RELAY_MAX_ROOMS_PER_CLIENT` | rooms a client may create: the token subject (tenant-qualified), else the address | 429 `client_rooms` |
| `LANGSTROTH_RELAY_TUNNEL_PUSH_RATE` / `_BURST` / `_PER_DAY` | pushes per tunnel host (were fixed at 1/s, 5, 2000) | ack `rate_limited` |

Rules:

- A room "exists" from its first write of any kind (envelope, key rotation,
  candidate, presence). Creating it is what counts against
  `MAX_ROOMS_PER_CLIENT`; reads never create a room. A room deleted by
  retention or by a tenant admin no longer counts.
- The creator is stored as `sha256("relay-room-creator\0" + key)`, never the
  raw address or subject.
- Quotas are **refusals, not eviction**, so a flood can't push a room's seed
  out. A full room stays readable; new writes get 507 until retention or an
  admin frees it.
- Quota checks are atomic with the write: under the memory store's lock, and
  in the Postgres append transaction (the room's usage row is incremented
  with `RETURNING` and the transaction rolls back when over).
- An idempotent re-push of an envelope already stored is answered as before
  (its seq) even when the room is full.
- Every 429 and 507 carries `Retry-After` (for a rate, the seconds until a
  token is available, rounded up; for a room or tenant quota an hour; for
  relay-wide storage ten minutes) and a JSON body
  `{"error":"<code>","message":"...","retryAfter":n}` whose `error` is the
  limit name above (README, "Refusals: 429 and 507").

## Tenancy

### Turning it on

`LANGSTROTH_RELAY_TENANCY` holds a JSON document, normally read from a file
with `LANGSTROTH_RELAY_TENANCY_FILE` (the usual settings-from-files rule).
Unset = tenancy off; the relay behaves exactly as without this feature and
`/v1` is byte-identical.

```json
{
  "defaults": { "max_bytes": 0, "max_rooms": 0, "room_max_bytes": 0,
                "room_max_envelopes": 0, "rate": 0, "burst": 0,
                "max_pairings": 0, "max_hosts": 0, "push_per_day": 0 },
  "tenants": [
    { "id": "acme",
      "issuer_pubkeys": ["<hex or base64 Ed25519 public key>"],
      "shared_issuer": true,
      "admin_token_sha256": "<hex sha256 of a tenant admin token>",
      "limits": { "max_bytes": 10737418240, "rate": 200, "burst": 400 } }
  ]
}
```

Startup refuses (exit 1, naming the problem) when: the JSON doesn't parse or
has unknown fields; a tenant id isn't `^[a-z0-9][a-z0-9-]{0,62}$` (the
langstroth-auth rule) or repeats; one public key is listed for two tenants or
equals `LANGSTROTH_RELAY_TOKEN_PUBKEY(_NEXT)`; no tenant can be authenticated
(no tenant key and no shared key); `LANGSTROTH_RELAY_TENANTS` or
`LANGSTROTH_RELAY_ACCESS_TOKENS` is also set (tenancy replaces both); the
GitHub social routes are on (their namespace is global); an admin hash is
not 64 hex characters; or `LANGSTROTH_RELAY_METRICS_TOKEN` is unset (an open
`/metrics` would list every tenant and its usage).

### Who is the tenant

The tenant comes **only from a verified access token**. No header, query
parameter or path segment can name it; a client-sent `X-Ligus-Tenant`,
`X-Tenant-Id` or similar is ignored.

1. The token must be a signed `hrt1` token with a `tid` claim naming a
   configured tenant. A token without `tid`, or naming an unknown tenant,
   gets 401.
2. Its signature must verify under that tenant's own `issuer_pubkeys` (a
   tenant running its own langstroth-auth, i.e. its own OIDC issuer), or,
   when the tenant has `shared_issuer: true` (the default), under the
   relay-wide `LANGSTROTH_RELAY_TOKEN_PUBKEY(_NEXT)` (one langstroth-auth
   serving several tenants, which already refuses to let one tenant grant
   another's workspaces).
3. A token signed by tenant A's key that says `tid: "b"` is refused (401):
   only B's keys or the shared key can vouch for B. This is the token
   confusion check.
4. Expiry, `workspaces` scope (`LANGSTROTH_RELAY_WORKSPACE_SCOPE`) and caps
   apply as before. Scope is checked against the id the client sent, inside
   its tenant.

The open policy and the static allowlist can't identify a tenant, so they are
refused in tenancy mode.

### Isolation

Every room is stored under `<tenant>/<id>`, where `<id>` is the workspace id
the client uses. `<tenant>` can't contain `/`, so the mapping is one to one,
and a tenant can only ever name keys under its own prefix. Two tenants using
the same workspace id get two unrelated rooms. Consequences:

- **Reads, writes, SSE, keyring, presence, candidates, members, invites,
  join** all resolve to the caller's own namespace. A request for another
  tenant's room reaches the caller's own (empty) room: the response doesn't
  depend on whether the other tenant has such a room, so there's no
  existence oracle.
- **Enumeration:** there is no listing route across tenants. The tenant
  admin listing is filtered by prefix in the store query, not after it.
- **Contacts** (`/v1/contacts/{cid}`) are stored under `<tenant>/<cid>` too:
  a drop box is only reachable from the same tenant.
- **Pairing codes** are kept per tenant. Codes are drawn per tenant, so the
  same 6 characters can be live in two tenants with different payloads; a
  resolve only looks in the caller's tenant. The live-code cap applies per
  tenant (`max_pairings`) and globally (`LANGSTROTH_RELAY_MAX_PAIRINGS`).
  The failed-resolve lockout stays per client.
- **Tunnel hosts** are bound to a tenant at first registration, from the
  registration token (which must pass the tenancy gate). The binding is
  stored (`relay_tunnel_hosts.tenant`, the snapshot's host record) and never
  changes; a host can't move to another tenant. `max_hosts` caps hosts per
  tenant. Phones are authorized by the host's signed credential, as before;
  their **push registrations** belong to the host and so to its tenant, and
  pushes are limited per host and per tenant (`push_per_day`).
- **Seams** (`WriteGuard`, `ReadGuard`, `Hooks`) receive the storage key
  `<tenant>/<id>` as the workspace, so a downstream guard sees the same
  namespace as the store.
- Turning tenancy on for a relay that already holds rooms hides those rooms
  (they are under unprefixed keys). Tenancy is for new deployments.

### Per-tenant limits

Each tenant's `limits` override `defaults` field by field; 0 = no tenant
limit (the relay-wide limits above still apply).

| Field | Meaning | Refusal |
|---|---|---|
| `max_bytes` | stored bytes across the tenant's rooms | 507 `tenant_bytes` |
| `max_rooms` | rooms the tenant holds | 507 `tenant_rooms` |
| `room_max_bytes`, `room_max_envelopes` | per-room quotas for this tenant (instead of the relay-wide ones) | 507 |
| `rate` / `burst` | every request from the tenant | 429 `tenant_rate` |
| `max_pairings` | live pairing codes | 503 |
| `max_hosts` | registered tunnel hosts | registration refused |
| `push_per_day` | tunnel pushes across the tenant's hosts | ack `rate_limited` |

Tenant usage is kept in `relay_tenant_usage` (Postgres) and updated in the
same transaction as each write, so a tenant can't overshoot by racing.

### Tenant admin scope

New routes, registered only in tenancy mode (404 otherwise, so the golden
routes don't change):

| Route | Answer |
|---|---|
| `GET /v1/tenant` | `{"tenant","usage":{"bytes","rooms","hosts"},"limits":{...}}` |
| `GET /v1/tenant/rooms?after=<id>&limit=<n>` | `{"rooms":[{"id","bytes","envelopes","lastWrite"}],"next"}`, ids without the prefix, sorted, at most 1000 |
| `DELETE /v1/tenant/rooms/{id}` | delete the room's envelopes, key rotations, device blobs, roster and invites; `{"ok":true,"existed":bool}` |

They bypass the room access gate and authenticate themselves: a tenant token
(as above) carrying the cap `tenant_admin`, or the tenant's static admin
token (compared by SHA-256 in constant time against every tenant's hash).
Either admits that one tenant only. The operator-wide `/v1/admin/*` and
`/metrics` are unchanged.

### Metrics

Added to `/metrics` (behind `LANGSTROTH_RELAY_METRICS_TOKEN`, which tenancy
requires; the tenant series are never served on an open `/metrics`):

- `hive_relay_limit_rejections_total{limit}`: every refusal above, fixed
  label values.
- `hive_relay_rooms`, `hive_relay_room_bytes`: totals from the store.
- With tenancy: `hive_relay_tenant_requests_total{tenant}`,
  `hive_relay_tenant_limit_rejections_total{tenant,limit}`,
  `hive_relay_tenant_bytes{tenant}`, `hive_relay_tenant_rooms{tenant}`.

Tenant label values come only from configured tenant ids. At most
`LANGSTROTH_RELAY_METRICS_MAX_TENANTS` (default 50) distinct values are
emitted; the rest are summed under `tenant="_other"`. Metrics never carry a
room id, address or token.

### Postgres schema

Added in place by the migration that already runs in one transaction under
`pg_advisory_xact_lock` (additive; v0.3.0 before this change keeps working
against the new schema):

- `relay_rooms (workspace PK, tenant, creator, bytes, envelopes, last_write)`
  with indexes on `(tenant, workspace)`, `creator` and `last_write`. A one-time
  backfill from `relay_envelopes` and `relay_keyring` runs the first time
  (recorded in `relay_migrations`).
- `relay_tenant_usage (tenant PK, bytes, rooms)`.
- `relay_tunnel_hosts.tenant TEXT NOT NULL DEFAULT ''`.

### Wire compatibility

- Tenancy off: no route, header, status code or body changes. With the
  community limits set, the only new behavior is 507/429 past a limit.
- Tenancy on: `/v1` requests and responses keep their shapes; the new
  `/v1/tenant*` routes are additive. `hrt1` is unchanged (`tid` already
  exists).

### What tenancy does not do

- No HA and no per-tenant processes or databases: one process, one store.
- The relay operator still sees every tenant's metadata (room ids, sizes,
  timing, addresses); tenants are isolated from each other, not from the
  operator.
- SCIM deprovisioning still ends access only as short-lived tokens lapse
  (langstroth-auth), and removing a member from the Ligus roster still needs
  an Admin's device.
