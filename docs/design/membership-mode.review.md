# Membership mode: adversarial security review

Reviews [membership-mode.md](membership-mode.md). The reviewer's job is to
break the design, not to like it. Every claim below either cites the design or
the code, or names the executable test that would prove it. Fixes marked
**folded** are already in the design. The design was revised once after the
first pass, and §5 records both verdicts.

## 1. Assets and attackers

Assets this mode is meant to protect: new ciphertext in a room (envelopes,
keyring, presence, blobs) from non-members; the room from writes by
non-members; the workspace from being locked out of its own relay.

Attackers considered:

* **S, a stranger** who knows or guesses a room id. Legacy name-derived rooms
  make guessing realistic. New rooms carry 122 bits.
* **X, a removed member** (or a revoked device). X knows the room id, holds old
  keys, has an up-to-date client, and may modify it.
* **M, a current member** who misbehaves (any role, including a box).
* **A, a rogue or compromised Admin device.**
* **O, the relay operator.** Out of scope for *enforcement*, which O can
  switch off. In scope for: forging what clients believe, metadata, and
  breaking the workspace in ways clients can't detect.
* **N, a network attacker** in front of TLS (an ingress or proxy that logs
  headers).

## 2. Findings, first pass

Severity is Critical (a goal fails), High (a goal fails in a plausible case),
Medium, or Low.

### F1. A rogue Admin evicts every other Admin (Critical, **folded**)

**Attack.** v1 of the design had two classes, `a` and `m`, and rule 4 let any
`a` signer publish any list. A, about to be removed (or compromised),
publishes `ver n+1` listing only A's key as `a`. The relay accepts it: the
signature is valid, A is `a` in `cur`, and the chain continues. Every honest
Admin is now `m` or absent, and rule 4 refuses their republish. The audit
raises the alarm, but nobody but the operator can repair the list, and A
keeps reading and writing.

**Fix.** Three classes `o > a > m` plus the founder flag `f`. The relay
applies Ligus's own rank rules to the difference between `cur` and the new
list (design §5.2, rule 4a in §5.5): nobody raises an entry above their own
class, an `o` entry is cut only by an `o` signer, an `f` entry only by an `f`
signer, and at least one `o` remains. A can now evict Admins and members but
not Owners. Any Owner device's next audit restores the list. Once an Owner
removes A in Ligus, the next publish drops A's key.

**Residual.** A can still churn `a` and `m` entries until an Owner device is
online. That is a nuisance, bounded by the publish rate (§9). A rogue *Owner*
can evict other Owners at the relay. Ligus trusts Owners the same way, apart
from the founder, whom `f` protects.

**Tests.** R20 `TestRankRules`; C14 `rogue_admin_converges`.

### F2. Forged attestations (none found)

* S or X can't sign as a listed `o`/`a` kid: Ed25519, rule 3.
* O can't forge one that clients accept either. Clients check the signature
  and the chain (§5.6 step 2), and authorization never flows from the
  attestation into the client (§1). The most O can do is enforce a list of
  its own choosing, which it could do by running a different binary.
* A forged **birth claim** by S on a new room needs the room id before the
  founder shares an invite. The founder claims before writing the seed
  (§5.5), so S has nothing to race. On a legacy name room, `operator`
  late-claim is the default.
* **Gap found:** the v1 audit compared a birth claim (`cfg 0`) with the fold
  at seq 0, which is empty, so every honest birth claim would have raised the
  alarm. **Folded**: a birth claim is valid when its one key is the seed
  author's bound key (§5.6 step 4).

**Tests.** R11 (bad signature, proof signer ≠ `signer`); C8
`audit_detects_foreign_claim`; a new case in C7 for an honest birth claim
raising no alarm.

### F3. Rollback and replay of old attestations (Medium, covered)

* At the relay: `ver = cur.ver+1` and `prev = digest(cur)` (rule 4) refuse a
  stored old `lma1`. A replayed PUT also fails the write-nonce check. **R11**.
* By O serving an old list to clients: the audit compares `ver` and digest
  with the last version this device saw and with the latest
  `RelayAttestationPublished` in the log (§5.6 steps 2-3). O can hide the log
  event too, but only by withholding envelopes, which is plain denial of
  service. **C7**.
* `cfg` rollback by a stale Admin: `cfg ≥ cur.cfg` (rule 4) plus the
  `stale_attestation` 409. **C6**, **R12**.

### F4. A removed member keeps access (High, mostly covered)

Each window, and what closes it:

| Window | Length | Closed by |
|---|---|---|
| The removing Admin publishes in the same action | one round trip after the event's seq | §5.7 |
| The removal was authored offline, or is a non-Admin self-revocation | until an `o`/`a` device audits | the audit loop. **Residual R-2** |
| X's open SSE stream | until the commit | the relay closes streams of dropped kids. **R8** |
| X's request already past the check | that request | accepted |
| X replays a *write* proof captured from another member | none | the body hash binds the proof to that member's own write (X can't alter it), and the nonce makes it single-use. **R5**, **R6** |
| X replays a *read* proof captured by N | up to 300 s | gains ciphertext X couldn't open without the new key, and only if a key rotation followed. **Residual R-5** |
| X keeps a relay key through a copied binding | none | a binding counts only when signed by the device itself; a duplicate key counts only for the earlier binder (§5.1) |
| X reads ciphertext held from before removal | forever | out of scope: limitation 14, "removal is not erasure" |

**Gap found:** v1 built the list from "the fold at `cfg`", but a device's fold
can hold events that have no relay seq (authored but not yet pushed). An
Admin could then publish a list that no other device can reproduce at `cfg`,
so honest devices raise false alarms. Worse, an Admin who folded an unpushed
`MemberAdded` would admit someone the log on the relay doesn't admit.
**Folded**: the list comes from relay seqs `1..cfg` only (§5.2). **C13**.

### F5. Split view between clients (Medium, accepted for O)

* An honest relay has one current attestation per room, and the CAS publish
  keeps it single-valued even with Postgres (R12). Two clients therefore see
  the same list.
* O can serve different lists to different clients, or enforce a list other
  than the one it serves. A client can't see what the relay enforces, only
  what it serves. This is accepted: O can ignore enforcement altogether, and
  confidentiality never rested on it. The audit catches the detectable half
  (a served list that disagrees with the log).
* **Gap found:** v1 derived keys from the relay URL as each client had it
  configured. Two members reaching one relay by different URLs would bind
  different keys, and whichever URL the publishing Admin used would decide
  who got admitted. That is an honest-relay split view. **Folded**: keys
  derive from a relay-chosen `relay_id` (§5.1). **C15**.

### F6. Metadata leakage (Medium, accepted with a decision)

What the design adds (design §8):

* **Per-device attribution** of every request on an enforcing room. This is
  the real cost.
* The **Owner, Admin and founder** subsets.
* Whether a room is claimed: anyone with the id learns it from a 401 or 404.

What it doesn't add:

* Device count (already in keyring recipient ids).
* Device ids, account ids or Ligus roles: kids are derived per room and per
  relay, so the relay can't link them to device ids or across rooms.

**Checked:** a client never sends a proof to an unclaimed room or to a relay
without the mode (C10), so attribution is limited to rooms that opted in.
**Checked:** `RelayKeyBound` and `RelayAttestationPublished` ride inside the
E2EE log, so O never sees the kid-to-device mapping.

Owner decision D6 stands. Option E (anonymous credentials) is the only way to
remove attribution.

### F7. Malicious operator O (no new power found)

For each thing O might want, whether this mode gives O something new:

* **Read content:** no. No key material reaches the relay.
* **Make clients trust a forged member:** no. The attestation never feeds
  client authorization (§1).
* **Lock the workspace out:** O could already do that by refusing requests.
* **Take a workspace over at the relay** (`membership reset`): O could
  already ignore enforcement. A reset is visible to every client as "reset by
  the relay operator" (§10.2).
* **Learn more metadata:** yes, F6.
* **Weaponize permits** by issuing a late-claim permit to S: that only matters
  on rooms that haven't opted in. Members see the foreign claim (C8).
  Socially engineering an honest operator into issuing a permit is a real
  risk. The operator guide must say to verify the requester through the
  workspace, not by email. **Residual R-4**.

### F8. Restart reopens the write-replay window (High, **folded**)

**Attack.** The nonce cache is in memory. A captured write proof (N, from a
logging proxy) replayed right after a relay restart, still inside 300 s,
would be accepted again. For envelopes the store's idempotency key usually
makes the repeat a duplicate, but that holds only when the client sent
`Idempotency-Key`; presence overwrites and blob uploads have no such key.

**Fix.** The relay refuses a proof whose `ts` is older than the process start
(`401 proof_expired`); the client re-signs and retries once (§5.3, §5.4).
**R21**.

### F9. Cross-relay proof replay (Low, accepted)

A proof doesn't sign the relay's identity. The kid does depend on
`relay_id`, so a proof verifies only where that kid is listed. A malicious
relay that copies an honest relay's `relay_id` could forward a member's proofs
to the honest relay inside the window. It would get ciphertext it can't open,
or a second copy of the member's own write. Adding `relay_id` to the signed
message wouldn't stop a relay that lies about its id. Accepted.
**Residual R-6**.

### F10. Slow uploads age out of the window (Medium, **folded**)

A 25 MiB blob on a slow link can take longer than 300 s. In v1 the timestamp
check ran after the body was read, so honest uploads would fail and retry
forever. **Fix**: check `ts` when the headers arrive and the signature after
the body is read (§5.3). **R22**.

### F11. DoS (Medium, partly **folded**)

* **Signature flooding by S:** an unlisted kid is refused after a map lookup,
  before any signature work. **R17**.
* **Lockout by bad proofs (v1 gap):** v1 blocked a whole client address after
  20 failed proofs, so S could lock out an office NAT by sending garbage.
  **Folded**: unlisted kids are never counted, and failures are keyed per
  (address, listed kid), which only members can name (§9). **R23**.
* **Oversized attestation:** bounded before parsing; the body cap bounds the
  read. **R18**.
* **Publish churn** by M can't happen (`m` can't publish). Churn by A is
  bounded by the publish rate. **R18**.
* **Nonce-cache exhaustion:** caps of 8192 per kid and 262 144 relay-wide. A
  member can fill its own slot (and gets 429), not others'. The relay-wide
  cap could be reached by about 33 members writing at full per-writer rate,
  each needing its own listed kid. That is an insider, and it is visible.
  **Residual R-7**. **R18** `TestNonceCacheBounds`.
* **Claim spam:** counted as room creation. **R13**.
* **Audit polling:** an ETag/304 every 5 minutes per device and room is in
  the noise next to envelope polling.

### F12. Classes that are wrong for real Ligus traffic (High, covered by tests)

* **Viewers write** (`DeviceKeyAgreementPublished`, `AttachmentShared`): `m`
  may write envelopes and blobs, unlike roster mode's `contributor` floor,
  whose bug the design records (§2.2).
* **Keyring is Admin-only at the relay:** correct only if no non-Admin path
  ever posts a rotation row. The design rests this on the client's
  `rotate_workspace_key_for` being the only caller. That needs proof, not
  belief. **C11** must cover the CLI, serve and the desktop.
* **Boxes relay phone events:** the bearer isn't the signer. That is fine,
  because the relay checks the box's kid and the fold checks the phone's
  signature inside.

### F13. Old clients (Low, covered)

* An old client on an enforcing room fails closed with
  `401 member_proof_required` (§5.8). It can't bypass enforcement by omitting
  the header, because the relay requires it.
* An old client folding the new events must not stall. **C9** runs the
  previous release's fold over a fixture log. If it shows a hold, the events
  must move to a side channel (a presence slot) before shipping.

### F14. Operator reset and recovery (Low)

* `reset` names the next signer kid. Rule 4a doesn't bind that publish
  (otherwise a reset couldn't recover from a lost founder), so a reset is as
  powerful as O. It is logged, and clients flag it.
* The `r` recovery key is only as safe as the Owner's account key at rest.
  The Ligus threat model (limitation 1) keeps account keys in the OS secret
  store with a file fallback. D4 should stay off until the account-key story
  is clear.

## 3. Proposed executable tests (summary)

Already in the design's test plan, plus the ones this review added:

* **Relay:** R1-R3 (compatibility), R4-R10 (enforcement matrix and binding),
  R11-R16 (publish rules, CAS, claims, roster bridge, tenancy), R17-R19 (DoS
  and benchmark), R20 rank rules, R21 restart replay, R22 slow upload, R23
  failure keying.
* **Client:** C1-C12, plus C13 relay prefix only, C14 rogue Admin converges,
  C15 two URLs give one kid.
* **End to end:** E1 removal, E2 old client.

Two more tests to add during implementation:

* **R24 `TestAttestedFuzz`:** go-fuzz over the `lma1` and `lmp1` parsers;
  no panic, and bounded allocation per byte of input.
* **C16 `golden_vectors_cross_language`:** the Go and Rust implementations
  verify each other's attestations and proofs from `testdata/attest/`,
  including the rank-rule cases from R20.

## 4. Residual risks

| # | Risk | Why it is accepted |
|---|---|---|
| R-1 | The operator can ignore or bend enforcement, and sees more metadata (per-device attribution, Owner/Admin subsets) | enforcement is against third parties; confidentiality stays with E2EE; D6 |
| R-2 | Removals authored offline, and non-Admin self-revocations, wait for an Owner/Admin device to audit | needs a signer; the alternative (relay-side folding) was rejected |
| R-3 | Under `tofu` late claims, someone holding the id can squat a room that hasn't opted in | default is `operator`; detected (C8); `release` repairs |
| R-4 | An honest operator talked into a permit or reset | operator guide: verify through the workspace; every reset is visible to clients |
| R-5 | A read proof captured in front of TLS replays for up to 300 s | yields only ciphertext; writes are nonce-bound |
| R-6 | Cross-relay replay by a relay impersonating another's `relay_id` | ciphertext only, or a duplicate of the member's own write |
| R-7 | An insider with many listed kids can fill the relay-wide nonce cache | visible, bounded, and per-kid caps protect other members |
| R-8 | A rogue Admin churns `a`/`m` entries until an Owner is online; a rogue Owner evicts other Owners (not the founder) | same trust Ligus gives those roles |

## 5. Verdict

**First pass: Revise.** F1 (a rogue Admin evicting all Admins) broke the
goal of keeping a workspace in control of its own relay list. F8, F4's
relay-prefix gap, F5's URL-derived keys, F10 and F11's NAT lockout each broke
an honest deployment or reopened a closed window.

**After the revision now in the design** (three classes and the founder flag
with the Ligus rank rules, building from the relay prefix, `relay_id`-derived
keys, refusing proofs from before a restart, checking the timestamp at the
headers, failure counting per listed kid, and the birth-claim audit rule):
**Approve with risks.** The risks are R-1 to R-8 above.

What must be true before it ships:

1. C9 passes against the then-current Ligus release.
2. C11 is proven across the CLI, serve and the desktop.
3. The R and C tests in §3 are green in both repositories, with shared golden
   vectors.
4. D4 stays off until account-key storage is settled.
