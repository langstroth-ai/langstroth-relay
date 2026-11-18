# Blobs: encrypted file attachments

The blob channel stores the files people attach in Ligus. A client seals a
file with its workspace key before it uploads it, so the relay holds
ciphertext only: it never sees a file's contents, name or type, and it can't
scan them. Every content check (type detection, size and policy limits,
blocklists, scanning) happens on the clients. What the relay does is keep
the channel from being a free storage or distribution service: it
authenticates every request, refuses past quotas and rates, expires blobs,
and deletes them on request.

A blob on its own grants nothing. The signed workspace event that refers to
it (its id, the hash and size of its plaintext, the author's device, the
declared type and name) lives inside the workspace's sealed envelope log.
Clients check a downloaded blob against that event and refuse one whose hash,
size or author doesn't match, so a relay that swaps, truncates or replays a
blob is caught on the client and the download fails closed.

## Routes

All three sit behind the access gate and the workspace scope check like the
other `/v1/workspaces/{id}/…` routes; with relay membership on, uploads and
deletes need Contributor and downloads need Viewer, as for envelopes.

| Method | Path | Request | Response |
|---|---|---|---|
| `POST` | `/v1/workspaces/{id}/blobs` | the ciphertext as the raw body (any `Content-Type`) | `201 {"id":"<43 chars>","size":<bytes>,"sha256":"<hex of the stored bytes>","expiresAt":<unix s, 0 = never>}` |
| `GET` | `/v1/workspaces/{id}/blobs/{blob}` | | `200 application/octet-stream`, with `Content-Length`, `Content-Disposition: attachment`, `X-Content-Type-Options: nosniff`, `Cache-Control: no-store` |
| `DELETE` | `/v1/workspaces/{id}/blobs/{blob}` | | `204`, whether or not the blob existed |

- **Ids are minted by the relay:** 32 random bytes, base64url without padding
  (43 characters of `A–Z a–z 0–9 - _`). There is no route that lists a
  workspace's blobs and no way to choose an id (`PUT` answers `405`), so a
  blob can only be fetched by someone who read its id from the workspace log.
- **404 is the same for everything absent:** an unknown, expired, deleted or
  malformed id, and a blob of another workspace (or another tenant's
  workspace of the same name) all answer `404 {"error":"blob_not_found"}`.
- **Uploads ride an existing workspace.** A blob can't create a room: an
  upload to a workspace that holds nothing yet answers
  `404 {"error":"no_workspace"}`. Room limits (`LANGSTROTH_RELAY_MAX_ROOMS`
  and the per-client and per-tenant room counts) therefore bound the number
  of workspaces that can hold blobs.
- Other errors: `400` empty body (`empty_blob`), `413` larger than
  `LANGSTROTH_RELAY_BLOB_MAX_BYTES` (`{"error":"blob_too_large","maxBytes":n}`),
  `401` no valid access token, `403` the workspace scope or roster refuses,
  `429`/`507` limits (below).
- A blob upload or download may take up to five minutes; the relay's 30 s
  read and write timeouts are for JSON requests.

### Deleting

Deleting is a capability: write access to the workspace plus the blob's id.
The relay has no notion of who may delete which attachment; that rule lives
in the signed workspace log. Ligus clients delete a blob only after a signed
deletion event (a message removal, or a member's removal) and every member's
client stops offering a blob once that event folds, whether or not the relay
deleted it.

When relay membership is on, removing a member through
`DELETE /v1/workspaces/{id}/members/{account}` also deletes the blobs that
member's token subject uploaded to that workspace (best effort: a failure is
logged and the removal still succeeds). Blobs uploaded without an identified
token are tagged by client address instead and aren't removed this way.

A room purge (`DELETE /v1/tenant/rooms/…`) and idle-room retention delete a
room's blobs with its envelopes.

## Limits

Every limit refuses; nothing is evicted to make room. Storage refusals are
`507`, the upload rate `429`, both through the shared refusal format
(README, "Refusals: 429 and 507") with `Retry-After`.

| Variable | Default | `error` | Meaning |
|---|---|---|---|
| `LANGSTROTH_RELAY_BLOBS` | `on` | | `off` unregisters the three routes (`404`) |
| `LANGSTROTH_RELAY_BLOB_MAX_BYTES` | `26214400` (25 MiB) | `413 blob_too_large` | one blob. `0` keeps the default: there is always a cap |
| `LANGSTROTH_RELAY_BLOB_ROOM_MAX_BYTES` | `536870912` (512 MiB) | `507 blob_room_bytes` | blob bytes per workspace (`0` = off) |
| `LANGSTROTH_RELAY_BLOB_ROOM_MAX_COUNT` | `2000` | `507 blob_room_count` | blobs per workspace (`0` = off) |
| `LANGSTROTH_RELAY_BLOB_WRITER_MAX_BYTES` | `0` (off) | `507 blob_writer_bytes` | blob bytes per uploader per workspace: the per-member quota. The uploader is the token subject (tenant-qualified), else the client address |
| `LANGSTROTH_RELAY_BLOB_WRITER_MAX_COUNT` | `0` (off) | `507 blob_writer_count` | blobs per uploader per workspace |
| `LANGSTROTH_RELAY_BLOB_TOTAL_MAX_BYTES` | `0` (off) | `507 blob_total_bytes` | blob bytes across the relay; `Retry-After` ten minutes |
| `LANGSTROTH_RELAY_BLOB_RETENTION_DAYS` | `30` | | a blob expires this many days after upload (`0` = kept until deleted). Expired blobs answer `404` at once and are deleted hourly |
| `LANGSTROTH_RELAY_BLOB_RATE` / `_BURST` | `1` / `20` | `429 blob_rate` | uploads per second per uploader per workspace (`0` rate = off). Downloads count toward the normal read rate |

Tenancy adds per-tenant limits in the tenancy config (`defaults` or a
tenant's `limits`), all `0` = off: `blob_max_bytes` and `blob_max_count`
(the tenant's total, `507 blob_tenant_bytes` / `blob_tenant_count`), and
`blob_room_max_bytes` and `blob_room_max_count`, which replace the relay-wide
per-workspace values for that tenant's workspaces.

Refusals count in `hive_relay_limit_rejections_total{limit="blob_…"}`.

### Relay-wide storage

Blobs don't count toward the envelope room quotas
(`LANGSTROTH_RELAY_ROOM_MAX_BYTES`, `…_MAX_TOTAL_ROOM_BYTES`): a full
attachment quota never stops a workspace's messages. They are not a way
around the relay's storage bounds, though:

- On the memory store every blob counts toward
  `LANGSTROTH_RELAY_MAX_STORE_BYTES` (the heap budget), and blobs are saved
  in the JSON snapshot. Raise the budget, the pod memory and the volume if
  you expect many attachments, or use Postgres.
- Blobs never create a room, so with `LANGSTROTH_RELAY_MAX_ROOMS` set the
  relay holds at most `MAX_ROOMS × BLOB_ROOM_MAX_BYTES` of blobs. Set
  `LANGSTROTH_RELAY_BLOB_TOTAL_MAX_BYTES` for a hard relay-wide bound.

### Sizing retention

Thirty days suits chat attachments: members download what they need while
it is fresh, and the relay doesn't become an archive. An expired blob
answers `404` like a deleted one, so a client can tell the user it is no
longer on the relay. Raise the retention (or set `0`)
for workspaces that need older files from the relay; lower it, or the
per-workspace quotas, on a relay open to untrusted clients.

## Storage

- **Memory store:** blobs are held in memory under the store lock and
  written to `relay-state.json`; on restart an expired blob, or one whose
  workspace no longer holds anything, is dropped.
- **Postgres:** `relay_blobs` (one row per blob, the ciphertext stored
  uncompressed out of line) and `relay_blob_usage` (running byte and count
  totals per workspace, per uploader in a workspace, per tenant and for the
  relay). The migration runs under the relay's migration advisory lock and is
  idempotent. An upload locks its totals in one fixed order and checks the
  quotas against the locked values in the same transaction as the insert, so
  concurrent uploads can't overshoot a quota; a purge or an idle expiry takes
  the room's row lock first, so an upload in flight finishes before the
  room's blobs are deleted.

Behind ingress-nginx the chart sets `proxy-body-size` to the blob size cap
(with `ingress.streamingDefaults`); other proxies in front of the relay need
their request body limit raised to `LANGSTROTH_RELAY_BLOB_MAX_BYTES` too.
