# Membership mode: relay enforcement from the workspace's own signed roster

Status: **design, not implemented.** Review: [membership-mode.review.md](membership-mode.review.md).
Verified against `langstroth-relay` at `060ba73` and `langstroth-ai/ligus` at
`origin/main` `8429b9873`. Paths without a prefix are in this repository;
`ligus:` paths are in the Ligus repository.

## 1. Summary

An opt-in mode, `LANGSTROTH_RELAY_MEMBERSHIP=attested`, in which a workspace's
Owner and Admin devices publish a **membership attestation** to the relay: a signed,
versioned list of the room's member keys, each with a relay class: `o`
(Owner), `a` (Admin) or `m` (member). The list is built from the workspace's own
signed event log, the same log every Ligus device folds. Once a room holds an
attestation, every read and write on it must carry a **member proof**: a
per-request Ed25519 signature by a key on the list. The relay checks only
signatures and list membership. It never sees content, never reads the log,
and never decides who is a member. It refuses callers only on rooms whose own
Owners or Admins have published a list, and then admits exactly that list.

* Off by default. When off, no new route is registered and no new header is
  read, so the relay is byte-identical to today.
* Opt-in per workspace. A room with no attestation behaves exactly as it does
  today, even on a relay running `attested`.
* Keys are per relay and per room: each device derives a fresh Ed25519 key
  for this room on this relay. The relay never learns device signing keys,
  device ids, account ids or Ligus roles (only the coarse class), and keys
  can't be linked across rooms or relays.
* Authorization flows one way: log → attestation → relay. Clients never take
  any authorization from an attestation. They audit it against their fold
  and republish or raise an alarm when it is stale.
* It replaces the existing relay-side roster (`LANGSTROTH_RELAY_MEMBERSHIP=1`),
  which keys on token subjects and is a second source of truth for "who is a
  member" (`ligus:docs/design/v2/architecture-review.md:158`). It also folds
  in the one idea from the private enterprise relay worth keeping (relay-side
  per-workspace enforcement, opt-in by claim) without its GitHub identity,
  so that repository can be archived (§11).

## 2. Today (code-verified)

### 2.1 What the relay enforces

| Layer | Where | What it decides |
|---|---|---|
| Access gate | `server.go:397` `gate`, `EntitlementVerifier` (open / `LANGSTROTH_RELAY_ACCESS_TOKENS` / signed `hrt1` / store tokens) | may this client use the relay at all |
| Workspace scope | `scope.go` `LANGSTROTH_RELAY_WORKSPACE_SCOPE=off\|claims\|strict` | may this token touch this room id (issuer decides) |
| Tenancy | `tenancy.go`, `docs/tenancy.md` | which tenant namespace the room lives in |
| Relay roster ("roster mode") | `members.go`, `invites.go`, `LANGSTROTH_RELAY_MEMBERSHIP=1` | per-room roster of token subjects (`claims.Sub`) with roles; guard on reads (`viewer`) and writes (`contributor`) via `membershipGuard` (`members.go:156-189`), default-wired at `server.go:272` |
| Guards | `WriteGuard` / `ReadGuard` seams (`seams.go`), called by `enforceWrite` (`server.go:471`) and `enforceRead` (`server.go:495`) | nil on the open relay |

`enforceWrite` runs on envelopes, keyring, presence, candidates and blob
upload and delete; `enforceRead` on the matching GETs, the SSE stream
(`server.go:680`), the roster list and blob download. Contacts
(`/v1/contacts/*`), pairing (`/v1/pair`) and the tunnel (`/v1/tunnel/*`) are not
room-scoped and don't call either guard.

On the open relay with no token gate, **anyone who knows a room id can read
every envelope, the keyring, presence and blobs, and can append to them.**
Content stays confidential because envelopes are sealed
(`SealedEnvelope{version, nonce, ciphertext, kid}`, `ligus:crates/ligus-core/src/e2ee.rs:189-212`).
This is Ligus threat-model limitation 16 ("The relay's open mode is open",
`ligus:docs-site/docs/operate/security/threat-model.md:731`) and the read half
of hardening item S5 (`ligus:docs/security-hardening-plan.md:187-196`). The
hardening residual tracked as "H5, relay read-authz" isn't written down on
`origin/main` under that name. S5 and limitation 16 are the written forms, and
this design is their fix.

### 2.2 What the relay can't see (and must keep not seeing)

From the Ligus client:

* **One relay room per workspace.** The path id is the `room` string, and the
  workspace UUID is `UUIDv5(room)` (`ligus:crates/ligus-core/src/channel.rs:26-30`).
  The config log and every chat share the room. New rooms are
  `random_token(32)`, 122 random bits (`ligus:app/src/lib.rs:13586`;
  `ligus:crates/ligus-runtime/src/workspace_invite.rs:198-206`). Legacy
  name-derived rooms still exist.
* **The signer is inside the ciphertext.** `signer_device_id`, `session_id`
  and `event_id` are all sealed. The `Idempotency-Key` is a keyed hash, so
  "the relay learns no event id" (`ligus:crates/ligus-runtime/src/relay_client.rs:154-173`).
  So today the relay can't tell who wrote an envelope, apart from the bearer
  subject or address.
* **Membership** is the folded config log: `MemberAdded`, `MemberRemoved`,
  `MemberRoleChanged`, `DeviceCertificateAdded` (Ed25519 or P-256 device keys
  certified by the account key, `ligus:crates/ligus-core/src/crypto.rs:416-465`),
  `DeviceRevoked`, `JoinApproved`, and roles `Owner(3) > Admin(2) >
  Contributor(1) > Viewer(0)` (`ligus:crates/ligus-core/src/identity.rs:95-112`).
  Service members (`ligus serve`, `ligus worker` boxes) are their own members,
  capped at Contributor (`ligus:crates/ligus-core/src/authz/mod.rs:1196-1253`).
  Agents sign with host-certified keys. Phones hold P-256 keys and **never
  call `/v1/workspaces`**: a serve box carries their signed events.
* **Viewers write.** A Viewer legitimately authors `DeviceKeyAgreementPublished`
  (needed to receive key rotations), `PasskeyRegistered`, `ChatLockdownRaised`
  and `AttachmentShared` (`ligus:crates/ligus-core/src/authorization.rs:203-344`).
  A content-blind relay can't tell these from a chat message. **Finding:**
  roster mode's "writes need `contributor`" (`members.go:157`) therefore
  locks a Viewer out of publishing its key-agreement key, so on a roster-claimed
  room a Viewer never receives a rotation.
* **Keyring rows** (`SignedRotation{version, prev_version, sealed{device_id →
  blob}, key_commitment}`) are posted only by Admin key rotations
  (`ligus:crates/ligus-runtime/src/keyring.rs:360-390`; rotations need Admin
  plus presence). The relay already sees the recipient **device ids in
  plaintext**, so it already knows how many devices a workspace has.
* **Presence slot names are plaintext** (`human:<device_id>`, `host:<host_id>`,
  `lease:…`, `revocations:<host_id>`, `phone-enroll:…`). The values are sealed.

### 2.3 The client today

The desktop already drives roster mode: it has a "Team members
(server-enforced)" section with a Claim button, invites and remove
(`ligus:web/src/components/RightRail.tsx:2388-2503`; Tauri commands
`ligus:app/src/lib.rs:14337-14485`). The agent-box bootstrap upserts a random
`ligus:<uuid>` subject (`ligus:app/src/lib.rs:15140-15170`). Clients send only
`Authorization: Bearer`, `Idempotency-Key` and, on contacts,
`X-Ligus-Contact-Key` (`ligus:crates/ligus-runtime/src/relay_client.rs:1014-1090`).

Two findings to fix whatever is decided here:

* `upsert_member`, `remove_member`, `revoke_invite` and `claim_membership`
  don't treat a non-2xx status as an error, so `workspace_remove_member` goes on
  to remove and rotate after the relay refused.
* The box bootstrap's random `ligus:<uuid>` subject contradicts
  `ligus_account_id_for`, which reads that subject as an account id
  (`ligus:app/src/lib.rs:14414-14424`).

## 3. Goals and non-goals

Goals:

1. Opt-in per relay and per workspace. With the mode off, or on a room that
   never opted in, every existing route answers byte for byte as today.
2. The relay's authority comes from the workspace's own signed membership. It
   uses no GitHub identity, no relay user table and no token subject.
3. Enforce reads as well as writes, on every room-scoped route.
4. Revocation that takes effect within one request of an Admin acting.
5. No change to `/v1` paths, `hrt1`, `hpc1`, `hive-tunnel/1`, `X-Hive-*`,
   `X-Ligus-*` or existing refusal codes. New things get new names.

Non-goals:

* **Defending against the relay operator.** An operator can run a binary that
  ignores all of this, as it can ignore the token gate today. Confidentiality
  against the operator stays with E2EE. This design must not give the
  operator anything new beyond the metadata in §8, and must let clients
  detect an honest-looking relay serving a stale list.
* Making roles limit reading. Every member can read the whole workspace
  (limitation 15). The relay distinguishes members from non-members and
  Admins from the rest, nothing finer (§6.2).
* Phones and the tunnel. The tunnel already authenticates end to end with
  host-signed credentials and host-signed revocation (`docs/reference.md`
  "Relay tunnel"). It is not room-scoped and stays as it is (§6.4).

## 4. Options considered

| | Option | How the relay learns membership | Verdict |
|---|---|---|---|
| A | **Signed attestation + per-request member proof** | Admin devices publish a signed key list derived from the log; each request is signed by a listed key | **Recommended** |
| B | Relay folds the event log | Governance events in plaintext, or a relay-readable copy | Rejected: puts membership, roles, account ids and device keys in plaintext; makes the relay a second fold implementation that must match the client's authz engine exactly |
| C | Admin-minted capability tokens | An Admin device mints short-lived bearer tokens for each member; the relay knows only the Admin keys | Rejected as primary: members lose access when no Admin device is online to re-mint; revocation waits for expiry or needs a revocation list, which is an attestation again. Kept as a possible optimization (§13) |
| D | Keep roster mode | Token subjects from the access issuer | Rejected: a second source of truth; identity from the token issuer, not the workspace; no device granularity; breaks Viewers (§2.2) |
| E | Anonymous credentials (BBS+, ring or group signatures) | The relay verifies "some member signed" without learning which | Deferred: best privacy, but new cryptography in two languages for a feature that is opt-in; revisit if per-device attribution (§8) proves to matter |

Option A leaves the relay doing signature checks and set lookups, nothing more.
It keeps the workspace's log as the only authority. It degrades safely: a
stale attestation over-admits until any Admin device republishes, and clients
detect that it is stale.

## 5. Design

### 5.1 Relay keys

Each device that talks to the relay (desktop, `ligus serve`, `ligus worker`)
holds one **relay key** per (relay, room, epoch):

```
seed = HKDF-SHA256(ikm  = device Ed25519 signing seed,
                   salt = "langstroth-relay-key-v1",
                   info = relay_id || 0x00 || room || 0x00 || u32be(epoch))
relay key = Ed25519 keypair from seed;  kid = base64url(public key)  (43 chars)
```

* `relay_id` is 16 random bytes the relay creates in its store the first time
  it starts in `attested` mode and reports in `GET /v1/membership` (§5.8). It
  doesn't depend on the URL, so two members who reach the same relay through
  different URLs (a LAN name and a public name, say) derive keys for the same
  relay, and renaming the relay changes nothing. A relay whose store is lost
  gets a new id, but it has lost its attestations too.
* Deriving the key separates it from the device signing key, so the device key
  never signs in a new protocol context. Because it is deterministic, a
  reinstalled device that keeps its seed gets the same key back. Bumping
  `epoch` rotates it.
* Phones (P-256, Secure Enclave) never need one: they don't call room routes.
* The device announces the key **inside the E2EE log**, on the config log, in
  a new event:

```
RelayKeyBound { relay_id, room, epoch, relay_public_key }   // signed by the device key
```

  The fold accepts it at the Viewer floor when its signer is a certified,
  unrevoked device of a current member, and when `room` is this workspace's
  room. The latest binding per (device, relay_id) wins. If two devices bind
  the same key, only the earlier binding counts. A copied key is useless
  without its private half, but it must not muddle blob ownership. Older clients
  fold it as an unknown event: with the Owner floor `Unknown` gets
  (`ligus:crates/ligus-core/src/authorization.rs`), they keep it out of their
  projection. Test C9 checks that this never holds or blocks a fold.

### 5.2 The attestation (`lma1`)

The compact form mirrors `hrt1`, so the relay verifies the bytes it receives
and never re-serializes them (see `ligus:docs/design/v2/architecture-review.md`
RC-5):

```
lma1.<b64url(claims_json)>.<b64url(ed25519_sig over "lma1." || b64url(claims_json))>
```

```json
{
  "v": 1,
  "room": "<room id exactly as in the path>",
  "ver": 7,
  "prev": "<b64url SHA-256 of the previous lma1 string, \"\" for ver 1>",
  "cfg": 1834,
  "iat": 1795000000,
  "signer": "<kid>",
  "members": [{"k": "<kid>", "c": "o", "f": true}, {"k": "<kid>", "c": "a"}, {"k": "<kid>", "c": "m"}]
}
```

* `c` is the relay class, ranked `o` (3) > `a` (2) > `m` (1). `o` and `a`
  keys may publish the next attestation, delete any blob and post key
  rotations. `m` keys read and write. `f: true` marks the founder's keys.
  Mapping from the fold at `cfg`: Owner human members → `o`, the founder's
  devices also `f`; Admin human members → `a`; Contributor, Viewer and
  service members → `m`. Agents and phones get no entry.
* The relay applies the Ligus rank rules to each change between `cur` and the
  new list (rule 4a in §5.5), so a compromised or rogue Admin device can't
  evict the Owners from the relay list and lock the workspace out. Ligus says
  nobody grants above their own role, only Owners cut Owners, and nobody cuts
  the founder (`ligus:crates/ligus-core/src/authz/mod.rs:1326-1391`). Each current member contributes the
  latest `RelayKeyBound` key of each of its certified, unrevoked devices for
  this relay. Entries are sorted by `k`, without duplicates.
* `cfg` is the relay seq through which the signer had folded the room
  (contiguously from 1). It is what makes concurrent Admins converge (§5.6).
  The list is built from the fold of **relay seqs 1..cfg only**. An event this
  device holds that hasn't been assigned a relay seq (one it authored but
  hasn't pushed yet, or one it got another way) doesn't count, or the list
  could reflect something no other device can check.
* Attestations don't expire. An expiry would lock the workspace out whenever
  no Admin device is online, and staleness is caught by the audit (§5.6).
* The **digest** of an attestation is SHA-256 of the full `lma1` string.

The Admin who publishes also appends a signed log event:

```
RelayAttestationPublished { relay_id, room, ver, digest, cfg }   // Admin floor, no presence
```

It grants nothing on the client. It lets every device detect rollback or
withholding by the relay (§5.6). It needs no passkey presence, because Admin
devices republish automatically and it changes no client-side authorization.

### 5.3 Member proof (`lmp1`)

Each request to an enforcing room carries one new header:

```
Langstroth-Member-Proof: lmp1.<kid>.<ts>.<nonce>.<sig>
```

* The kid is per room and per relay, so a proof is useless at another relay.
  The one exception is a malicious relay replaying it at another relay that
  uses the same `relay_id`, which only a relay pretending to be another can
  arrange. That gains it ciphertext it can't open, or a second copy of the
  client's own write (review F9).
* `ts`: unix seconds, decimal. `nonce`: 16 random bytes, base64url.
  `sig`: base64url Ed25519 by `kid` over

```
"langstroth-member-proof-v1" 0x00 METHOD 0x00 REQUEST_TARGET 0x00 ts 0x00 nonce 0x00 hex(SHA-256(body))
```

  `REQUEST_TARGET` is the escaped path plus `?query` exactly as sent (it names
  the room). A GET's body hash is that of the empty string.
* The relay's check order is cheapest first: parse (bounded length); look up
  `kid` in the room's current attestation (an unknown kid means 403 with no
  signature work); `|now - ts| ≤ 300 s`, judged **when the request headers
  arrive**, so a slow blob upload can't age out of its own window; refuse a
  `ts` older than the process start (the replay cache is in memory, so a
  restart must not reopen the window: the client re-signs and retries once);
  for non-GET requests, `(kid, nonce)` unseen; verify the signature over the
  body hash once the body is read; record the nonce until `ts + 300`.
* GETs aren't replay-cached. Replaying a captured GET inside the window
  returns ciphertext the captor could already have read off the wire. Writes
  are, because a replayed write would append.
* Blob uploads: the relay already reads the whole body and computes its
  SHA-256 (`blobs.go` `postBlob`). It checks the proof against that hash
  **before** `PutBlob`.
* SSE: the proof is checked at connect. When an attestation drops a kid, the
  relay closes that kid's open streams (§5.7).
* A client sends the header **only to rooms it knows are enforcing** (§5.8).
  Sending it everywhere would hand per-device attribution to relays that
  don't need it.

### 5.4 What gets enforced on an enforcing room

| Route | Needs |
|---|---|
| `GET …/envelopes`, `…/events` (SSE), `…/keyring`, `…/presence`, `…/candidates`, `…/blobs/{blob}`, `…/attestation` | proof by any listed key |
| `POST …/envelopes`, `…/presence`, `…/candidates`, `…/blobs` | proof by any listed key |
| `POST …/keyring` | proof by an `o` or `a` key (rotations are Admin-only in the client) |
| `DELETE …/blobs/{blob}` | proof by its uploader's kid, or by an `o` or `a` key |
| `PUT …/attestation` | proof by the new attestation's `signer`, which must be `o` or `a` in the current one (§5.5) |
| roster routes (`…/members*`, `…/invites*`, `…/join`) | `409 attested_room` |
| `/v1/contacts/*`, `/v1/pair*`, `/v1/tunnel/*` | unchanged (not room-scoped) |

* The access gate, workspace scope and tenancy still run first. Membership
  narrows what they admit and never widens it.
* On an enforcing room the relay uses `dk:<kid>` as the writer tag in place of
  `sub:`/`ip:` (`hardening.go:116`). That one change scopes idempotency keys,
  per-writer rates, blob ownership and blob quotas to the device.
* When a key leaves the attestation, the relay does **not** delete that key's
  blobs. Roster mode does (`members.go` `removeMemberBlobs`), but Ligus history
  still refers to them. An Admin deletes them explicitly.

Refusals follow `refusal.go`'s JSON shape and carry the header
`Langstroth-Membership: attested`:

| Status | `error` | When |
|---|---|---|
| 401 | `member_proof_required` | enforcing room, no proof header |
| 401 | `bad_member_proof` | malformed header or bad signature |
| 401 | `clock_skew` (+ `now`) | `ts` outside the window (same shape as `POST /v1/tunnel/revoke`) |
| 403 | `not_a_member` | kid not in the current attestation |
| 403 | `needs_admin_key` | an `m` key on an Admin route |
| 401 | `proof_expired` | `ts` earlier than the relay process start (re-sign and retry) |
| 409 | `replayed` | write nonce seen |
| 409 | `stale_attestation` (+ `ver`, `digest`) | publish not based on the current version |
| 409 | `attested_room` | roster route on an attested room |
| 422 | `bad_attestation` (+ `reason`) | publish fails a rule in §5.5 |
| 429 | `attestation_rate`, `proof_failures` | §9 |

### 5.5 Publishing and claiming

`PUT /v1/workspaces/{id}/attestation` with body `{"attestation":"lma1…"}`. The
relay accepts it only if every rule holds, checked in a single store
transaction (compare-and-swap on `ver`):

1. Size ≤ `LANGSTROTH_RELAY_ATTEST_MAX_BYTES` (128 KiB), entries ≤
   `LANGSTROTH_RELAY_ATTEST_MAX_ENTRIES` (1024); every `k` decodes to 32 bytes;
   no duplicates; `c ∈ {o, a, m}`; `f` only on `o` entries; at least one `o`.
2. `room` equals the path id; `v` is 1.
3. The signature verifies under `signer`, and the request's member proof is
   by `signer`.
4. If the room already has an attestation `cur`: `signer` is `o` or `a` in
   `cur`; `ver = cur.ver + 1`; `prev = digest(cur)`; `cfg ≥ cur.cfg`.
   4a. **Rank rules** between `cur` and the new list, with `S` the signer's
   class in `cur`. Every entry that is added, or whose class rises, ends at a
   class ≤ `S`. Every entry that is removed, or whose class falls, started at
   a class ≤ `S`, and an `o` entry is removed or lowered only when `S = o`.
   An `f` entry is removed, and an `f` flag added, only when the signer is
   itself `f`. The founder losing every device is the operator-reset case
   (§10.2). Since at least one `o` must remain (rule 1), nobody can empty
   the Owners.
5. `cfg ≤` the room's current envelope head.
6. `|iat − now| ≤ 300 s`.
7. Rate limits (§9).

**Claiming** is publishing `ver = 1` to a room with no attestation, and
`LANGSTROTH_RELAY_MEMBERSHIP_CLAIM` controls it:

* **Birth claims** (always allowed): the room holds nothing yet (no
  envelopes, keyring rows, blobs or device blobs). The founder's client
  publishes `ver 1 = {signer: founder kid, members: [{founder kid, o, f}], cfg: 0}`
  **before** writing the seed, before any invite exists, so nobody else can
  know the room id yet. It then writes the seed and its own `RelayKeyBound`.
* **Late claims** (an existing room opting in): `operator` (default) needs a
  one-time permit from `langstroth-relay membership permit --room R --signer
  <kid> --ttl 1h`; `tofu` takes the first valid `ver 1`; `off` takes no late
  claim at all.
* **From roster mode**: a room with a relay roster accepts `ver 1` when the
  bearer's subject is `owner` or `admin` on that roster. The roster rows are
  then deleted and the room becomes attested. That way switching a relay from
  `roster` to `attested` never opens a claimed room (§10.3).

A claim creates room state, so it counts toward `LANGSTROTH_RELAY_MAX_ROOMS`
and `_MAX_ROOMS_PER_CLIENT`.

### 5.6 Client audit (rollback, staleness and split view)

After every sync, and at most every 5 minutes on an enforcing room, each new
client:

1. Fetches `GET …/attestation`. The response carries `ETag: "<digest>"` and
   honours `If-None-Match`, so an unchanged attestation costs a 304.
2. Checks it: signature, `room`, and a chain unbroken from the last version
   this device saw. A lower `ver` than it saw before, or a different digest at
   the same `ver`, means rollback or equivocation. That raises the relay alarm
   (below) and changes nothing.
3. Compares it with the latest `RelayAttestationPublished` it has folded for
   this relay. A relay serving an older `ver` than the log records is rolling
   back. It raises the alarm.
4. Compares it with the list its own fold implies at its head. If they
   match, it is done. Otherwise it recomputes the list from its fold of relay
   seqs `1..cfg` (the expensive path, taken only on a mismatch). A **birth
   claim** (`ver 1`, `cfg 0`) is checked differently: its single key must be
   the `RelayKeyBound` key of the seed author's device.
   * The list at `cfg` doesn't match the device's own fold at `cfg`: the
     signer mis-derived the list or folded differently. Alarm, name the
     signer, and republish if this device is `o` or `a` and outranks the
     change (rule 4a).
   * It matches at `cfg`, but the head has moved on (a removal, a new device,
     a role change): stale. If this device is `o` or `a`, it republishes
     now. If not,
     it shows "the relay still admits N removed devices" or "your new device
     isn't admitted yet".

The **relay alarm** is a workspace banner ("This relay is serving an outdated
or altered member list"), plus a local audit entry. It never feeds
authorization. An `m` device can't fix it. Every Owner or Admin device that
syncs repairs what its rank allows, so the list self-heals as long as one is
online. If a rogue Admin keeps re-adding a removed crony, every Owner's
republish undoes it. The rank rules stop that Admin from evicting an Owner, so
the ping-pong ends when an Owner removes the Admin in Ligus and the next
publish drops its key.

**Concurrent Admins converge** because of rules 4 and 5. Suppose Admin A
removes M (event at seq `s`), folds through `cfg_A ≥ s` and publishes `ver n`.
Admin B, whose fold is behind, gets `409 stale_attestation`, syncs, and can
only publish `ver n+1` with `cfg_B ≥ cfg_A ≥ s`. So B's list includes the
removal. A B that skips the sync fails rule 4. A B lying about `cfg` is caught
by step 4 on every other device.

### 5.7 Revocation latency

| Event | Relay stops admitting the key |
|---|---|
| Admin removes a member, revokes a device or demotes an Admin | the same action: the client appends the event, waits for its seq, folds and publishes. About one round trip after the event is stored |
| Self-revocation by a non-Admin device, or removal authored while the relay was unreachable | when the next Admin device syncs and audits (minutes; at worst until an Admin device is online) |
| A removed device's open SSE stream | closed when the publish commits |
| A removed device's request in flight | its next request fails; one already past the check completes |
| A box (service member) | same as any member: its key leaves with its membership |
| A phone | unchanged: host-signed `POST /v1/tunnel/revoke` (immediate) |

Membership mode stops a removed device from reading new ciphertext and from
writing at all, even before a key rotation. Rotation is still needed to keep
new content from it if the relay misbehaves or the ciphertext leaks.

### 5.8 Discovery and old clients

* `GET /v1/membership` → `{"v":1,"relayId":"<b64url 16 bytes>","mode":"attested","claim":"birth+operator",
  "maxEntries":1024,"maxBytes":131072,"skewSecs":300}` (`404` when off). It
  passes the access gate like every `/v1` route.
* `GET …/attestation` → `404 {"error":"unclaimed"}` on a room with no
  attestation. On a claimed room, a caller without a valid proof gets `401`,
  so anyone holding the id learns whether the room is claimed (they would
  learn it from any `401` anyway).
* A new client caches the room's state. It sends proofs only to claimed rooms.
  On a `401` that carries `Langstroth-Membership: attested`, it fetches the
  attestation and retries once with a proof.
* An **old client** (no proofs) keeps working on any relay that is off, and on
  any room that isn't claimed. On a claimed room every request gets
  `401 member_proof_required`, which older builds show as a sync error. The
  Ligus troubleshooting page gets an entry ("update Ligus; this workspace
  enforces membership on its relay").
* Before a claim, the Admin's client lists every current human or box member
  device that has no `RelayKeyBound` for this relay (old builds, devices that
  have been offline), and asks for confirmation. Those devices lose relay
  access at the claim and get it back once they update and an Admin
  republishes.

## 6. Roles, boxes, agents, phones

### 6.1 Role mapping

| Ligus role | Relay class | Why |
|---|---|---|
| Owner (human) | `o` (founder's devices also `f`) | governs the list, including other Owners; posts key rotations; deletes any blob |
| Admin (human) | `a` | governs `a` and `m` entries; posts key rotations; deletes any blob |
| Contributor, Viewer | `m` | reads and writes envelopes, presence and blobs. The relay can't hold Viewers to read-only, because Viewers legitimately write (§2.2); the fold keeps enforcing what Viewers may author |
| Service member (box) | `m` | the box's device derives its own key. Capped at Contributor in Ligus, so never `a` or `o` |
| Agent | none | agent events are posted by their host device |
| Phone | none | phones never call room routes |

The enterprise relay's four RBAC roles therefore become three relay classes.
Contributor and Viewer merge because a content-blind relay can't tell them
apart without breaking Viewers. Owner and Admin stay apart because the rank
rules need them to (rule 4a).

### 6.2 What `m` cannot do

An `m` key can't publish an attestation, post a keyring row or delete
another key's blob. It can overwrite a presence slot it doesn't own, as any
writer can today. Slot values are sealed and signed inside, and the relay
doesn't know which device a slot belongs to (by design: kids aren't device ids).

### 6.3 Boxes

`ligus serve` and `ligus worker` derive relay keys like any device and publish
`RelayKeyBound` after admission. The box bootstrap no longer needs
`upsert_member` (§2.3). Its bearer token remains the access-gate credential.

### 6.4 The tunnel

Unchanged. A phone reaches its host through `/v1/tunnel/*`, which isn't
room-scoped. Its credentials are host-signed (`hpc1`), and a workspace
`DeviceRevoked` drives a host-signed `POST /v1/tunnel/revoke`
(`ligus:docs/relay-tunnel.md:782-830`). An optional later addition, not part of
this design: `LANGSTROTH_RELAY_TUNNEL=allowlist` could admit hosts whose box
kid is in some enforcing attestation. That would need a host-to-kid binding,
which today's `hpc1` doesn't carry.

## 7. Configuration

| Var | Default | Meaning |
|---|---|---|
| `LANGSTROTH_RELAY_MEMBERSHIP` | `off` | `off`, `attested`, or `roster` (today's mode; `1` and `true` stay as aliases, deprecated, §10.3). Any other value: refuse to start |
| `LANGSTROTH_RELAY_MEMBERSHIP_CLAIM` | `operator` | late claims: `operator`, `tofu` or `off`. Birth claims are always allowed |
| `LANGSTROTH_RELAY_MEMBERSHIP_UNCLAIMED` | `open` | `open`: unclaimed rooms behave as today. `closed`: unclaimed rooms refuse everything except a claim. This is the enterprise relay's "managed default-closed", for a relay where every workspace must opt in |
| `LANGSTROTH_RELAY_ATTEST_MAX_BYTES` / `_MAX_ENTRIES` | `131072` / `1024` | attestation bounds |
| `LANGSTROTH_RELAY_ATTEST_RATE` / `_BURST` / `_PER_DAY` | `0.5` / `10` / `500` | publishes per room |
| `LANGSTROTH_RELAY_PROOF_SKEW_SECS` | `300` | proof timestamp window (60–900) |

The task brief suggested `enforce|off`. The variable already exists with
`1` meaning roster mode, so the new value is named for what it does
(`attested`), and the old meaning stays reachable during deprecation.

Operator CLI (talks to the store; no HTTP admin API):
`langstroth-relay membership show --room R`,
`… permit --room R --signer KID --ttl 1h`,
`… reset --room R --signer KID` (the next publish may be signed by `KID`
whatever the current `o`/`a` set and the rank rules; `ver` and `prev` still chain),
`… release --room R` (delete the attestation; the room is unclaimed again).
Each one logs at `warn` with the room and the operator action.

## 8. Metadata: what the relay learns

| Fact | Today (open relay) | Attested room |
|---|---|---|
| room ids, sizes, timing, client addresses | yes | yes |
| device count | yes (keyring recipient device ids, presence slots) | yes (entry count), no change |
| device ids | yes (keyring, presence slot names) | not from the attestation: kids are per room and per relay, unlinkable to device ids |
| account ids, Ligus roles, logins | roster mode: token subjects and logins | **no**: only `o`/`a`/`m` and the founder flag |
| who is an Owner, Admin or the founder | no (roster mode: yes, by subject) | **yes**: which kids are `o`, `a`, `f` |
| which device wrote each envelope or blob, and when each device reads | no (bearer subject or address only) | **yes**, per kid |
| the same device across rooms or relays | via device ids in keyring rows | not via kids |

The new leak is **per-device attribution inside one room**, plus the Admin
subset. That is what it costs to enforce per device, and it is why proofs go
only to rooms that opted in. Option E (§4) would remove it. The keyring's
plaintext device ids (S5) leak more than this design adds. Hashing them is a
separate client change.

## 9. Abuse and DoS

* **Sizes:** attestation ≤ 128 KiB and ≤ 1024 entries, checked before parsing.
  The proof header must be ≤ 256 bytes. The relay keeps the current
  attestation plus the last 8 versions (≤ 1.1 MiB per room), counted in
  `LANGSTROTH_RELAY_MAX_STORE_BYTES` and the room byte quota.
* **Publish rate:** per room, `_ATTEST_RATE`, `_BURST`, `_PER_DAY`
  (`429 attestation_rate`).
* **Signature cost:** at most one Ed25519 verify per request, and only after
  the kid lookup and the timestamp check succeed. A stranger with random kids
  costs a map lookup. Existing read, write and per-writer limits bound
  listed-key traffic.
* **Proof failures:** a kid that isn't listed costs a map lookup and is never
  counted, so strangers can't lock anyone out. Failed signatures **by a
  listed kid** are counted per (client address, kid): after 20 in a minute,
  that pair's proofs are refused for 5 minutes (`429 proof_failures`).
  Keying on the kid as well means bad proofs from one address can't lock out
  everyone behind the same NAT, and only members can see kids (the
  attestation is readable by members only). Counted apart from tunnel
  failures, like `fix/host-fail-limit-separate`.
* **Nonce cache:** entries live until `ts + skew`. There are at most 8192 per
  kid (`429 replay_cache` past it, which a writer at the default 20/s per-writer
  rate never reaches) and 262 144 relay-wide (about 16 MiB). It is in-process,
  like pairing and SSE. The chart runs one sync instance.
* **Replayed attestations:** an old `lma1` fails rule 4 (`ver`, `prev`). A
  replayed PUT fails the nonce check. A relay that serves an old one is
  caught by the audit (§5.6).
* **Claim spam:** a claim is room creation (room caps). Birth claims need an
  empty room. Late claims are off for strangers by default.
* **Squatting:** a new room's id is unknown until its founder shares an invite,
  and the founder claims before that. An existing room can be claimed only
  with an operator permit (default) or under `tofu`. Under `tofu` someone
  holding the id (a former member, say) could claim first. Members' clients
  then see a `ver 1` whose signer has no `RelayKeyBound` in their fold, raise
  the alarm, and the operator runs `release` or `reset`.

## 10. Bootstrapping, recovery and migration

### 10.1 Turning it on

New workspace, on a relay that answers `GET /v1/membership`: the founder's
client birth-claims, if the workspace setting "Enforce membership on this
relay" is on (default: owner decision D2).

Existing workspace: an Admin clicks "Enforce membership on this relay". The
client checks readiness (§5.8), gets a permit from the operator if the relay
needs one, publishes `ver 1` listing every bound key, and appends
`RelayAttestationPublished`.

After that, every Admin device keeps the list current through the audit.

### 10.2 Recovery when no Admin key is left

1. **Another Owner or Admin device.** Any `o` or `a` key can publish, within
   its rank. This is the normal case.
2. **Owner recovery key** (optional, D4): an Owner device can add a
   `{k, c:"r"}` entry whose key derives from the Owner's **account** signing
   key (the same HKDF, with `salt = "langstroth-relay-recovery-v1"`). An `r` key
   may only sign the next attestation, with an Owner's rank under rule 4a. It
   can't read or write. That helps only
   if the account key survives the loss of every device (exported, or held on
   a device that isn't a workspace member), and the implementation has to
   confirm where Ligus keeps account keys before relying on this.
3. **Operator reset.** The operator runs `membership reset --room R --signer
   KID` with the kid a recovering Owner sends out of band. The Owner's new
   device (re-certified through Ligus account recovery) then publishes, and
   the chain continues. The operator is already trusted for enforcement, so
   this gives them nothing new. It is logged, and every client sees a `ver`
   whose signer wasn't `o` or `a` before, which the audit reports as "reset by the
   relay operator".
4. **Release.** `membership release` returns the room to unclaimed (open). The
   workspace keeps working, without enforcement.

### 10.3 From roster mode

* `LANGSTROTH_RELAY_MEMBERSHIP=attested` still honours rooms that have a
  relay roster. They stay roster-enforced, and their roster routes stay
  registered for them, until an `owner` or `admin` on that roster publishes
  `ver 1` (§5.5). New roster claims are refused (`409 use_attestation`).
* The Ligus client replaces the "Team members (server-enforced)" section with
  "Relay enforcement: on / off / N devices not admitted", and stops calling
  the roster routes.
* Roster mode is deprecated in the relay release that ships `attested`, with
  the same `Deprecation` header treatment the GitHub routes got. It is removed
  two minor releases later.

### 10.4 Compatibility checklist

* No existing route, request or response changes shape. New routes:
  `GET /v1/membership`, `GET|PUT /v1/workspaces/{id}/attestation`.
* New header `Langstroth-Member-Proof` (request) and
  `Langstroth-Membership` (response, only on refusals from enforcing rooms).
  No `X-Hive-*` or `X-Ligus-*` name is reused.
* `hrt1` is unchanged. New compact forms use new prefixes: `lma1`, `lmp1`.
* `LANGSTROTH_RELAY_MEMBERSHIP` unset or `off`: none of the above is
  registered or read (golden route tests).

## 11. The enterprise relay's features, and archiving it

`langstroth-ai/hive-relay-enterprise` (private) imports the pre-rename relay
library and injects behaviour through its seams. Its features:

| Feature | What happens to it |
|---|---|
| Per-person tokens (`HIVE_RELAY_USER_TOKENS`, revocation by subject, `StoreBackedVerifier`) | **Already in the open relay**: durable users and tokens (`users.go`, `StoreEntitlementVerifier`), signed per-subject `hrt1` (`langstroth-relay issue`), and langstroth-auth (per-person, short-lived, SCIM deprovisioning). They stay the *access* gate. Membership mode doesn't use token subjects |
| GitHub-authenticated admin API (`x-hive-github-token`, `HIVE_RELAY_ADMIN_LOGINS`) | **Dropped.** The open relay keeps the `AdminAuthorizer` seam (nil by default). The operator CLI (§7) covers membership administration |
| Per-workspace membership with claim, roles `viewer < contributor < admin < owner` | **Replaced** by attested mode, with roles from the workspace's own log, mapped to `o`/`a`/`m` (§6.1). The ported roster mode is deprecated |
| Managed default-closed (`HIVE_RELAY_UNMANAGED_OPEN=0`) | **Kept** as `LANGSTROTH_RELAY_MEMBERSHIP_UNCLAIMED=closed` |
| Claim requires an admin login (`HIVE_RELAY_CLAIM_REQUIRES_ADMIN`) | **Replaced** by birth claims plus operator permits |
| Durable membership store (`relay_memberships`) | **Replaced** by attestation tables in both built-in stores |
| Metering hooks, entitlement, paid-relay and go-live docs, `fly.toml`, deploy workflow | **Dropped** (hosted and paid material is out of scope) |
| Blob channel (`PUT …/blobs/{blobId}`) | **Superseded** by the open relay's blobs (`blobs.go`, relay-minted ids, quotas) |

Archive plan (owner actions; nothing here has been run):

1. Confirm the table above: nothing in the repository is unshipped and
   wanted.
2. Delete the repository's `FLY_API_TOKEN` Actions secret and disable
   `deploy.yml`, so an archived repository can't deploy.
3. Replace its README with a short notice: archived; relay-side membership
   lives in `langstroth-relay` (link to this design).
4. `gh repo archive langstroth-ai/hive-relay-enterprise`. It stays private.
5. Fly: the `hive-relay-managed` app may never have been created. The Fly
   account currently reports that its trial has ended, so nobody checks it
   now. Once the account is usable: if `hive-relay-managed` exists, confirm
   it holds no data anyone needs, then destroy it. **Never touch
   `hive-relay-enterprise` on Fly**: despite its name, it serves the open
   relay.

## 12. Implementation plan and size

### 12.1 Relay (Go)

| Piece | Files | Size (LOC, code + tests) |
|---|---|---|
| `lma1` parse and verify, `lmp1` parse and verify, signed-message construction | `attest.go`, `attest_test.go` | 250 + 400 |
| Store: attestation, history, permits, CAS publish (memory, snapshot, Postgres) | `attest_store.go`, `memory.go`, `postgres.go`, `snapshot.go` | 300 + 300 |
| Guard: `attestedGuard` on the existing `WriteGuard` / `ReadGuard` seams, plus a route-class table; `dk:` writer tag; Admin-class checks on keyring and blob delete; rank rules; roster bridge | `attest_guard.go`, `blobs.go`, `hardening.go`, `members.go` | 250 + 450 |
| Routes, discovery, ETag, refusals, SSE close on kid removal | `attest_routes.go`, `server.go`, `refusal.go` | 200 + 300 |
| Nonce cache, proof-failure limiter, publish rate | `attest_limits.go` | 120 + 200 |
| Operator CLI | `cli.go` | 120 + 100 |
| Config, env aliases, chart values | `envcompat.go`, `deploy/helm/…` | 60 + 80 |
| **Total** | | **about 1,300 + 1,850** |

The guard reuses the existing seams. It needs the raw body hash, which
`enforceWrite` doesn't get today, so the handlers pass it through the request
context (envelopes, presence, keyring and candidates read the body before
`enforceWrite`; blobs reorders its check to after the read).

### 12.2 Client (Rust, Ligus)

| Piece | Where | Size |
|---|---|---|
| Relay-key derivation; `RelayKeyBound` and `RelayAttestationPublished` events and their authz rules | `ligus-core` (crypto, events, authz) | 300 + 400 |
| Attestation builder from the fold at `cfg` (deterministic), `lma1` and `lmp1` encode/sign/verify, golden vectors shared with Go | `ligus-core::relay_attest` | 350 + 450 |
| `RelayClient`: proof on enforcing rooms, attestation GET (ETag) and PUT, refusal codes; fix the non-2xx handling (§2.3) | `ligus-runtime/src/relay_client.rs` | 200 + 250 |
| Sync: publish after governance changes, audit loop, alarm state, birth claim before seed | `ligus-runtime` (sync engine, keyring) | 300 + 400 |
| Desktop and CLI: enforce toggle, readiness list, alarm banner, `ligus relay attest status\|publish\|recover`; retire roster UI | `app/src/lib.rs`, `web/…`, `ligus-cli` | 350 + 200 |
| **Total** | | **about 1,500 + 1,700** |

### 12.3 Docs

* Relay: `docs/reference.md` (vars, routes, refusals), `docs/membership.md`
  (operator guide: claim, permit, reset, release, metadata), README table,
  CHANGELOG, SECURITY.md (what membership mode does and doesn't defend).
* Ligus: replace `docs-site/docs/operate/ops/server-side-membership.md`;
  update the threat model (§The relay metadata; limitation 16 now reads "open
  unless the workspace enforces membership"); troubleshooting entry; the CLI
  reference.

## 13. Test plan

Executable tests. Go tests live in this repository, Rust tests in Ligus, and
golden vectors in `testdata/attest/` here, copied into Ligus and checked there.

**Relay: compatibility**

* R1 `TestMembershipOffRoutesGolden`: with the mode off, `golden_routes_test`
  and `golden_contacts_test` pass unchanged, and `/v1/membership` and
  `…/attestation` answer 404.
* R2 `TestAttestedUnclaimedRoomUnchanged`: with `attested` on, every existing
  route on an unclaimed room returns the same status, headers and body bytes
  as with the mode off (table-driven over the golden route list).
* R3 `TestProofHeaderIgnoredOnUnclaimed`: a valid or garbage proof header on
  an unclaimed room changes nothing.

**Relay: enforcement**

* R4 `TestEnforcedRouteMatrix`: for every row of §5.4 × {no proof, unlisted
  kid, `m`, `a`, `o`, uploader}, the expected status and code.
* R5 `TestProofBindsMethodPathBody`: a proof replayed with a different method,
  path, query, room or body → 401.
* R6 `TestWriteNonceReplay`: the same write proof twice → 409 `replayed`; a GET
  replay inside the window → 200; outside the window → 401 `clock_skew` with
  `now`.
* R7 `TestBlobProofCheckedBeforeStore`: an upload whose body hash differs from
  the proof's stores nothing (store count unchanged).
* R8 `TestRemovedKidLosesAccessNextRequest`: publish `ver n+1` without K; K's
  next GET and POST → 403; K's open SSE stream closes within 1 s.
* R9 `TestKeyringNeedsAdminKey`, `TestBlobDeleteUploaderOrAdmin`.
* R10 `TestDeviceWriterTag`: on an attested room the idempotency scope and the
  per-writer rate are per kid (two kids with the same `Idempotency-Key` both
  append).

**Relay: publish rules**

* R11 `TestPublishRules`: one case per rule in §5.5 (oversize, too many
  entries, bad key length, duplicate, no `o`, `f` on a non-`o` entry, wrong room, bad signature, proof
  signer ≠ `signer`, `m` signer, `ver` gap, wrong `prev`, `cfg` going back, `cfg`
  past the head, `iat` skew).
* R12 `TestPublishCASConcurrent`: 50 goroutines publish `ver n+1`; exactly one
  wins, the rest get 409 with the winner's digest (memory and Postgres).
* R13 `TestBirthClaimOnlyOnEmptyRoom`; `TestLateClaimPolicy` (operator permit
  used once and expiring, `tofu`, `off`); `TestUnclaimedClosed`.
* R14 `TestRosterBridgeClaim`: a roster room stays enforced under `attested`;
  only a roster owner or admin's bearer can publish `ver 1`; then the roster
  rows are gone and the roster routes answer 409 `attested_room`.
* R15 `TestAttestationSurvivesRoomRetentionAndSnapshot`; tenancy:
  `TestAttestationTenantIsolated` (the same room id in two tenants) and
  `TestTenantPurgeDeletesAttestation` (`DELETE /v1/tenant/rooms/{id}` removes
  the attestation, its history and its permits, so a purged room id can't
  keep an old list).
* R16 `TestOperatorReset`, `TestOperatorRelease`.

**Relay: DoS**

* R17 `TestUnknownKidNoSignatureWork`: a counter on the verify call stays 0 for
  unlisted kids.
* R18 `TestProofFailureLimiter`, `TestNonceCacheBounds`,
  `TestAttestRateLimits`, `TestAttestationSizeBeforeParse` (a 10 MiB body is
  refused without allocating it: the body cap bounds the read).
* R19 `BenchmarkProofVerify`: under 100 µs per request on CI hardware.

**Relay: rank rules and windows (from the review)**

* R20 `TestRankRules`: an `a` signer adding an `o`, removing an `o`, lowering
  an `o` to `a` or adding `f` → 422; an `o` non-founder removing an `f` entry
  → 422; an `o` removing another `o` → 200; dropping the last `o` → 422.
* R21 `TestProofBeforeRestartRefused`: a write proof signed, then the server
  restarted (new process start time), then replayed → 401 `proof_expired`; a
  fresh proof → 200.
* R22 `TestSlowBlobUploadTimestampAtHeaders`: an upload whose body takes
  longer than the skew window, but whose `ts` was fresh at the headers, is
  stored.
* R23 `TestProofFailuresKeyedByKid`: 100 bad signatures for kid A from one
  address leave kid B from the same address unaffected; unlisted kids are
  never counted.

**Client (Rust)**

* C1 `relay_key_derivation_vectors`: the HKDF and Ed25519 outputs match the
  shared golden vectors; a different origin, room or epoch gives a different
  kid.
* C2 `attestation_from_fold_is_deterministic`: two devices that folded the
  same prefix produce byte-identical claims JSON.
* C3 `attestation_maps_roles`: Owner → `o` (founder also `f`), Admin → `a`,
  Contributor, Viewer and box → `m`; agents and phones absent; revoked devices and removed members
  absent.
* C4 `proof_matches_go_vectors`: the signed message and signature match
  `testdata/attest/proof_*.json`.
* C5 `admin_republishes_after_removal`: an in-process relay (the Go binary in
  the Ligus relay compatibility job): remove M, then M's device gets 403 on
  its next request.
* C6 `stale_admin_converges`: two Admin devices, B behind; B's publish gets
  409, B syncs, and B's attestation excludes M.
* C7 `audit_detects_rollback_and_equivocation`: a fake relay serves `ver n-1`,
  then a different digest at `ver n`; the alarm is raised and nothing is
  published from it.
* C8 `audit_detects_foreign_claim`: a fake relay serves a `ver 1` signed by an
  unknown kid → the squat alarm.
* C9 `old_build_folds_new_events`: a fixture log with `RelayKeyBound` and
  `RelayAttestationPublished` folds on the previous release's fold code with
  no held events, and its projection is unchanged.
* C10 `no_proof_to_unclaimed_rooms`: with a recording relay, no
  `Langstroth-Member-Proof` header is sent to an unclaimed room or a relay
  without `/v1/membership`.
* C11 `non_admin_never_posts_keyring`: the property that §5.4's Admin-only
  keyring rule depends on.
* C12 `relay_errors_are_errors`: the roster helpers' non-2xx handling (§2.3).
* C13 `attestation_uses_relay_prefix_only`: an event folded locally but not
  yet pushed (no relay seq) doesn't appear in a published list.
* C14 `rogue_admin_converges`: an Admin device that re-adds a removed member
  after every Owner publish stops being able to once an Owner removes it in
  Ligus. Its attempt to evict the Owner's key is refused by the relay.
* C15 `same_relay_two_urls_same_kid`: two clients reaching one relay by
  different URLs derive the kid from `relay_id` and are both admitted.

**End to end (CI job in Ligus, Go relay plus two Ligus CLIs in temp data
dirs, `LIGUS_DATA_DIR` isolated)**

* E1: create a workspace on an attested relay (birth claim), invite a member,
  sync, remove the member. The member's CLI gets `401`/`403` on the next sync,
  and the remaining member keeps syncing.
* E2: an old-release CLI on an enforcing room prints the sync error. The same
  CLI on an unclaimed room syncs.

## 14. Decisions for the owner

* **D1. Value name.** `LANGSTROTH_RELAY_MEMBERSHIP=attested` (recommended,
  because `1` already means roster mode), or rename to a new variable.
* **D2. Default for new workspaces** on a relay that offers the mode:
  birth-claim automatically (recommended, because a birth claim is the only
  squat-proof one), or only when the founder ticks a box.
* **D3. Late-claim default:** `operator` (recommended) or `tofu`.
* **D4. Owner recovery key (`r` class):** include it in v1, or rely on another
  Admin device plus operator reset. It depends on where Ligus keeps account
  keys.
* **D5. Roster mode:** deprecate in the same release and remove two minors
  later (recommended), or keep it indefinitely.
* **D6. Attribution:** accept per-device attribution on enforcing rooms (§8),
  or fund Option E first.
* **D7. Archive** `hive-relay-enterprise` once this design is accepted (§11),
  not when it ships. Nothing in it is needed for the implementation.
