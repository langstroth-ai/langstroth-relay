package relay

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"
)

// Contacts (relay v0.3): an opaque, signed contact record per random contact id
// (cid) plus a sealed drop box for that cid. The relay does no cryptography on
// either: the record is client-signed and verified only by clients, drops are
// sealed client-side. The relay keeps SHA-256 of the owner's write key (a
// 32-byte random capability chosen on first write) and compares hashes.
//
// ContactStore is an optional interface, like TunnelStore, so a downstream
// Store that predates contacts keeps compiling; the relay then answers 404 on
// /v1/contacts/ (the same answer a v0.2 relay gives, which clients handle).

// ContactRecord is one stored contact record.
type ContactRecord struct {
	CID          string
	WriteKeyHash string          // hex SHA-256 of the owner's write key
	Record       json.RawMessage // opaque, stored and returned byte for byte
	UpdatedAt    int64
	ExpiresAt    int64
}

// ContactDrop is one sealed message in a contact's drop box (wire shape).
type ContactDrop struct {
	Seq       uint64          `json:"seq"`
	Blob      json.RawMessage `json:"blob"`
	CreatedAt int64           `json:"createdAt"`
}

var (
	// ErrContactNotFound: no live (unexpired) record for the cid.
	ErrContactNotFound = errors.New("contact not found")
	// ErrContactKey: the presented write key doesn't match the record's.
	ErrContactKey = errors.New("contact key does not match")
	// ErrContactsFull: the store holds its maximum number of contact records.
	ErrContactsFull = errors.New("relay contact capacity reached")
)

// ContactLimits are the storage bounds the handlers pass to the store, so the
// store stays free of policy (like friendCap).
type ContactLimits struct {
	MaxContacts     int // live records per store (memory store only; 0 = unbounded)
	DropMaxRows     int // rows per drop box, oldest evicted (0 = unbounded)
	DropMaxBoxBytes int // blob bytes per drop box, oldest evicted (0 = unbounded)
	// ReclaimHold is how long after a record expires only its old write key
	// may create the cid again (0 = anyone at once). A cid travels to
	// co-members, invitees and anyone a link was forwarded to; without a hold,
	// any of them could claim the cid of an owner who was offline past the TTL
	// and receive the drops meant for them.
	ReclaimHold time.Duration
}

// ContactStore is the contacts' durable state.
type ContactStore interface {
	// PutContact creates the record when the cid has no live record, storing
	// keyHash as its write key. An expired record's drop box is discarded; for
	// lim.ReclaimHold after its expiry only its old key may create it again
	// (ErrContactKey otherwise). For a live record keyHash must equal the
	// stored hash (ErrContactKey) and the record is replaced. created reports
	// whether a record was created.
	PutContact(ctx context.Context, cid, keyHash string, record json.RawMessage, now, expiresAt int64, lim ContactLimits) (created bool, err error)
	// GetContact returns the live record, or nil.
	GetContact(ctx context.Context, cid string, now int64) (*ContactRecord, error)
	// DeleteContact removes the record and its drop box (key-checked).
	DeleteContact(ctx context.Context, cid, keyHash string, now int64) error
	// AppendContactDrop appends a blob to a live record's drop box, evicting
	// the oldest rows past lim.DropMaxRows / lim.DropMaxBoxBytes.
	AppendContactDrop(ctx context.Context, cid string, blob json.RawMessage, now int64, lim ContactLimits) (uint64, error)
	// ContactDropsAfter returns up to limit drops with seq > after (and at most
	// about maxBytes of blobs, always at least one row); more reports a
	// truncated page. Key-checked.
	ContactDropsAfter(ctx context.Context, cid, keyHash string, after uint64, limit, maxBytes int, now int64) (items []ContactDrop, more bool, err error)
	// AckContactDrops deletes the drops with seq <= through (key-checked).
	AckContactDrops(ctx context.Context, cid, keyHash string, through uint64, now int64) (int64, error)
	// PruneContacts empties expired records (record and drop box; the write
	// key hash is kept until reclaimHold after expiry, then the row goes) and
	// deletes drops older than dropMaxAge (0 = no age bound). Returns rows
	// removed.
	PruneContacts(ctx context.Context, now int64, dropMaxAge, reclaimHold time.Duration) (int64, error)
}

// ── Memory (and snapshot) implementation ─────────────────────────────────────

// memContacts is the in-process ContactStore. memoryStore embeds it (its state
// rides the JSON snapshot); the server uses a standalone one only in tests.
type memContacts struct {
	cmu      sync.Mutex
	contacts map[string]*memContact
	held     int // stripped (reclaim hold) entries in contacts
	// reserve charges bytes against the owning store's byte budget (nil = no
	// budget). Called with cmu held; must not take cmu.
	reserve func(n int64) bool
}

type memContact struct {
	keyHash   string
	record    json.RawMessage
	updatedAt int64
	expiresAt int64
	drops     []ContactDrop
	dropBytes int
	nextSeq   uint64
	stripped  bool // expired and emptied: a reclaim hold tombstone
}

// contactSeqBase is the drop-box seq a box created at now starts after. A box
// that is deleted, or expires, and is created again for the same cid must not
// reuse seqs: each device of the account keeps its own after= cursor and acks
// with through=, so a restart at 1 would hide new drops from a device whose
// cursor is past them and let its stale ack delete drops nobody has read. The
// creation second times 2^20 grows faster than any box can be filled (drops are
// rate-limited per cid), stays below 2^53 until the year 2242 so JavaScript
// clients read it exactly, and needs no per-cid state after a delete.
func contactSeqBase(now int64) uint64 {
	if now <= 0 {
		return 0
	}
	return uint64(now) << 20
}

// contactOverheadBytes is what the byte budget charges per record beyond its
// payload (map entry, cid, key hash).
const contactOverheadBytes = 256

func newMemContacts() *memContacts {
	return &memContacts{contacts: map[string]*memContact{}}
}

func (m *memContacts) charge(n int64) bool {
	if m.reserve == nil {
		return true
	}
	return m.reserve(n)
}

func (c *memContact) bytes() int64 {
	return int64(contactOverheadBytes + len(c.record) + c.dropBytes)
}

func (m *memContacts) liveLocked(cid string, now int64) *memContact {
	c := m.contacts[cid]
	if c == nil {
		return nil
	}
	if c.expiresAt <= now {
		m.stripLocked(c)
		return nil
	}
	return c
}

// stripLocked empties an expired record down to its key hash and seq counter
// (a reclaim hold tombstone; PruneContacts removes it after the hold).
func (m *memContacts) stripLocked(c *memContact) {
	if c.stripped {
		return
	}
	freed := len(c.record) + c.dropBytes
	c.record, c.drops, c.dropBytes, c.stripped = nil, nil, 0, true
	m.held++
	m.charge(-int64(freed))
}

func (m *memContacts) removeLocked(cid string) {
	if c := m.contacts[cid]; c != nil {
		m.charge(-c.bytes())
		if c.stripped {
			m.held--
		}
		delete(m.contacts, cid)
	}
}

func (m *memContacts) PutContact(_ context.Context, cid, keyHash string, record json.RawMessage, now, expiresAt int64, lim ContactLimits) (bool, error) {
	m.cmu.Lock()
	defer m.cmu.Unlock()
	rec := append(json.RawMessage(nil), record...)
	if c := m.liveLocked(cid, now); c != nil {
		if !constantTimeEq(c.keyHash, keyHash) {
			return false, ErrContactKey
		}
		if !m.charge(int64(len(rec) - len(c.record))) {
			return false, ErrStoreFull
		}
		c.record, c.updatedAt, c.expiresAt = rec, now, expiresAt
		return false, nil
	}
	if old := m.contacts[cid]; old != nil { // expired (stripped by liveLocked)
		held := now < old.expiresAt+int64(lim.ReclaimHold/time.Second)
		if !constantTimeEq(old.keyHash, keyHash) {
			if held {
				return false, ErrContactKey
			}
			m.removeLocked(cid)
		} else {
			if !m.charge(int64(len(rec))) {
				return false, ErrStoreFull
			}
			old.record, old.updatedAt, old.expiresAt, old.stripped = rec, now, expiresAt, false
			m.held--
			old.nextSeq = max(old.nextSeq, contactSeqBase(now))
			return true, nil
		}
	}
	// Tombstones hold no record or drops (their bytes still count toward the
	// store budget), so the cap counts the other entries only.
	if lim.MaxContacts > 0 && len(m.contacts)-m.held >= lim.MaxContacts {
		return false, ErrContactsFull
	}
	c := &memContact{keyHash: keyHash, record: rec, updatedAt: now, expiresAt: expiresAt, nextSeq: contactSeqBase(now)}
	if !m.charge(c.bytes()) {
		return false, ErrStoreFull
	}
	m.contacts[cid] = c
	return true, nil
}

func (m *memContacts) GetContact(_ context.Context, cid string, now int64) (*ContactRecord, error) {
	m.cmu.Lock()
	defer m.cmu.Unlock()
	c := m.liveLocked(cid, now)
	if c == nil {
		return nil, nil
	}
	return &ContactRecord{CID: cid, WriteKeyHash: c.keyHash, Record: c.record, UpdatedAt: c.updatedAt, ExpiresAt: c.expiresAt}, nil
}

// ownedLocked returns the live record if keyHash matches.
func (m *memContacts) ownedLocked(cid, keyHash string, now int64) (*memContact, error) {
	c := m.liveLocked(cid, now)
	if c == nil {
		return nil, ErrContactNotFound
	}
	if !constantTimeEq(c.keyHash, keyHash) {
		return nil, ErrContactKey
	}
	return c, nil
}

func (m *memContacts) DeleteContact(_ context.Context, cid, keyHash string, now int64) error {
	m.cmu.Lock()
	defer m.cmu.Unlock()
	if _, err := m.ownedLocked(cid, keyHash, now); err != nil {
		return err
	}
	m.removeLocked(cid)
	return nil
}

func (m *memContacts) AppendContactDrop(_ context.Context, cid string, blob json.RawMessage, now int64, lim ContactLimits) (uint64, error) {
	m.cmu.Lock()
	defer m.cmu.Unlock()
	c := m.liveLocked(cid, now)
	if c == nil {
		return 0, ErrContactNotFound
	}
	if !m.charge(int64(len(blob))) {
		return 0, ErrStoreFull
	}
	c.nextSeq++
	c.drops = append(c.drops, ContactDrop{Seq: c.nextSeq, Blob: append(json.RawMessage(nil), blob...), CreatedAt: now})
	c.dropBytes += len(blob)
	// Evict the oldest past the row and byte caps (the newest always stays).
	drop := 0
	bytes := c.dropBytes
	for drop < len(c.drops)-1 &&
		((lim.DropMaxRows > 0 && len(c.drops)-drop > lim.DropMaxRows) ||
			(lim.DropMaxBoxBytes > 0 && bytes > lim.DropMaxBoxBytes)) {
		bytes -= len(c.drops[drop].Blob)
		drop++
	}
	if drop > 0 {
		m.charge(-int64(c.dropBytes - bytes))
		c.dropBytes = bytes
		c.drops = append([]ContactDrop(nil), c.drops[drop:]...)
	}
	return c.nextSeq, nil
}

func (m *memContacts) ContactDropsAfter(_ context.Context, cid, keyHash string, after uint64, limit, maxBytes int, now int64) ([]ContactDrop, bool, error) {
	m.cmu.Lock()
	defer m.cmu.Unlock()
	c, err := m.ownedLocked(cid, keyHash, now)
	if err != nil {
		return nil, false, err
	}
	i := sort.Search(len(c.drops), func(i int) bool { return c.drops[i].Seq > after })
	out := []ContactDrop{}
	size := 0
	for ; i < len(c.drops); i++ {
		d := c.drops[i]
		if len(out) > 0 && ((limit > 0 && len(out) >= limit) || (maxBytes > 0 && size+len(d.Blob) > maxBytes)) {
			return out, true, nil
		}
		out = append(out, d)
		size += len(d.Blob)
	}
	return out, false, nil
}

func (m *memContacts) AckContactDrops(_ context.Context, cid, keyHash string, through uint64, now int64) (int64, error) {
	m.cmu.Lock()
	defer m.cmu.Unlock()
	c, err := m.ownedLocked(cid, keyHash, now)
	if err != nil {
		return 0, err
	}
	i := sort.Search(len(c.drops), func(i int) bool { return c.drops[i].Seq > through })
	if i == 0 {
		return 0, nil
	}
	freed := 0
	for _, d := range c.drops[:i] {
		freed += len(d.Blob)
	}
	c.drops = append([]ContactDrop(nil), c.drops[i:]...)
	c.dropBytes -= freed
	m.charge(-int64(freed))
	return int64(i), nil
}

func (m *memContacts) PruneContacts(_ context.Context, now int64, dropMaxAge, reclaimHold time.Duration) (int64, error) {
	m.cmu.Lock()
	defer m.cmu.Unlock()
	var n int64
	cutoff := now - int64(dropMaxAge/time.Second)
	for cid, c := range m.contacts {
		if c.expiresAt <= now {
			n += int64(len(c.drops))
			if now >= c.expiresAt+int64(reclaimHold/time.Second) {
				n++
				m.removeLocked(cid)
			} else {
				m.stripLocked(c)
			}
			continue
		}
		if dropMaxAge <= 0 {
			continue
		}
		i := 0
		freed := 0
		for i < len(c.drops) && c.drops[i].CreatedAt < cutoff {
			freed += len(c.drops[i].Blob)
			i++
		}
		if i > 0 {
			c.drops = append([]ContactDrop(nil), c.drops[i:]...)
			c.dropBytes -= freed
			m.charge(-int64(freed))
			n += int64(i)
		}
	}
	return n, nil
}

// contactCount reports live and expired-unswept records (tests).
func (m *memContacts) contactCount() int {
	m.cmu.Lock()
	defer m.cmu.Unlock()
	return len(m.contacts)
}

// ── Snapshot ─────────────────────────────────────────────────────────────────

// Records and blobs are snapshotted as JSON strings, not raw JSON, so the
// exact bytes survive a restart (encoding/json compacts raw values).
type contactSnap struct {
	KeyHash   string            `json:"keyHash"`
	Record    string            `json:"record"`
	UpdatedAt int64             `json:"updatedAt"`
	ExpiresAt int64             `json:"expiresAt"`
	Drops     []contactDropSnap `json:"drops,omitempty"`
	NextSeq   uint64            `json:"nextSeq"`
	Stripped  bool              `json:"stripped,omitempty"`
}

type contactDropSnap struct {
	Seq       uint64 `json:"seq"`
	Blob      string `json:"blob"`
	CreatedAt int64  `json:"createdAt"`
}

func (m *memContacts) toContactSnap() map[string]contactSnap {
	m.cmu.Lock()
	defer m.cmu.Unlock()
	if len(m.contacts) == 0 {
		return nil
	}
	out := make(map[string]contactSnap, len(m.contacts))
	for cid, c := range m.contacts {
		cs := contactSnap{KeyHash: c.keyHash, Record: string(c.record), UpdatedAt: c.updatedAt,
			ExpiresAt: c.expiresAt, NextSeq: c.nextSeq, Stripped: c.stripped}
		for _, d := range c.drops {
			cs.Drops = append(cs.Drops, contactDropSnap{Seq: d.Seq, Blob: string(d.Blob), CreatedAt: d.CreatedAt})
		}
		out[cid] = cs
	}
	return out
}

// restoreContacts replaces the contacts from a snapshot and returns the bytes
// they hold (the caller adds them to the store budget).
func (m *memContacts) restoreContacts(snap map[string]contactSnap) int64 {
	m.cmu.Lock()
	defer m.cmu.Unlock()
	m.contacts = map[string]*memContact{}
	m.held = 0
	var total int64
	for cid, s := range snap {
		c := &memContact{keyHash: s.KeyHash, record: json.RawMessage(s.Record), updatedAt: s.UpdatedAt,
			expiresAt: s.ExpiresAt, nextSeq: s.NextSeq}
		for _, d := range s.Drops {
			c.drops = append(c.drops, ContactDrop{Seq: d.Seq, Blob: json.RawMessage(d.Blob), CreatedAt: d.CreatedAt})
			c.dropBytes += len(d.Blob)
		}
		if s.Stripped {
			c.stripped = true
			m.held++
		}
		m.contacts[cid] = c
		total += c.bytes()
	}
	return total
}
