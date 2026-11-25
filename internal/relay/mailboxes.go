package relay

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// /v1/mailboxes/* (Ligus 0.4): approval mailboxes for code-match approval.
// A workspace's members register one mailbox per key epoch; a new box,
// computer or phone drops a sealed request into it, and the members and the
// requester exchange a few sealed messages under that request until the
// requester has its answer. The relay never sees what is inside: every blob
// is sealed client-side, and the relay checks only signatures, hashes and
// limits. docs/reference.md, "Approval mailboxes", is the wire contract.
//
// Auth (no route goes through the access gate; each checks its own):
//
//   - register/reset (PUT /v1/mailboxes/{mailbox}) is self-certifying:
//     mailbox == hex(SHA256(authPub)[:16]) and an Ed25519 signature by
//     authPub over registerPreimage. A token-gated relay also wants a valid
//     access token, as for any member call.
//   - a member (list, post and read messages) signs X-Ligus-Mailbox-Auth
//     with the registered authPub over memberPreimage, ts within 300 s of
//     the relay's clock; a token-gated relay also wants a valid access token.
//   - a requester presents X-Ligus-Read-Key, whose SHA-256 is the
//     readKeyHash it put with its rid. It needs no token.
//   - a put presents X-Ligus-Put-Cap, whose SHA-256 is the mailbox's current
//     putCapHash. LANGSTROTH_RELAY_MAILBOXES=token also wants a token.
//
// Storage is process-local (like pairing codes): mailboxes are not in the
// snapshot or in Postgres. A restart forgets every registration and request;
// members re-register when a call answers mailbox_unknown, and a request
// lives 15 minutes at most anyway. The Helm chart runs exactly one sync
// process, and the tunnel-only pod answers 404 here (LANGSTROTH_RELAY_ROUTES).

const (
	mailboxAuthHeader    = "X-Ligus-Mailbox-Auth"
	mailboxReadKeyHeader = "X-Ligus-Read-Key"
	mailboxPutCapHeader  = "X-Ligus-Put-Cap"

	// Domain separation (ligus_core::match_code); protocol identifiers.
	mailboxRegisterCtx = "ligus-mailbox-register-v1\x00"
	mailboxMemberCtx   = "ligus-mailbox-member-v1\x00"

	// The wire contract's limits (fixed: clients rely on them).
	mailboxMaxLive        = 64               // live requests per mailbox
	mailboxMaxMsgs        = 10               // messages per request
	mailboxRequestTTL     = 15 * time.Minute // a request and its messages, from the put
	mailboxMaxRequestBlob = 8 << 10          // one request blob
	mailboxMaxMsgBlob     = 32 << 10         // one message blob
	mailboxPutsPerMinute  = 10               // puts per client address
	mailboxAuthWindow     = 300 * time.Second
	mailboxMaxWait        = 25 * time.Second

	// The relay's own bounds.
	mailboxMaxReserved       = 4 * mailboxMaxLive  // deleted/expired rids held per mailbox
	mailboxReserveFor        = mailboxRequestTTL   // a dead rid stays taken this long past its expiry
	mailboxIdleTTL           = 30 * 24 * time.Hour // a mailbox no member touched for this long is forgotten
	mailboxMaxPolls          = 1000                // concurrent long polls per process
	mailboxMaxPollsPerClient = 16                  // concurrent long polls per client
	mailboxSweepEvery        = 10 * time.Second    // lazy sweep of every mailbox, at most
	mailboxRegisterBodyCap   = 4 << 10             // register body
	mailboxSlash48Factor     = createSlash48Factor // an IPv6 /48 holds this many clients' mailboxes
	mailboxRetryStale        = 15 * time.Second    // Retry-After when only time frees a slot
	mailboxRetryStorage      = retryStoreFull      // 507: the operator or expiry frees it
	mailboxRetryRegister     = 24 * time.Hour      // a client's mailbox cap: idle expiry frees it
	defaultMaxMailboxes      = 10000               // LANGSTROTH_RELAY_MAX_MAILBOXES
	defaultMaxMailboxesPerCl = 20                  // LANGSTROTH_RELAY_MAX_MAILBOXES_PER_CLIENT
	defaultMailboxMaxBytes   = 64 << 20            // LANGSTROTH_RELAY_MAILBOX_MAX_BYTES
	mailboxSweepLoopInterval = 30 * time.Second    // background sweep (Serve)
	mailboxPollRetry         = retrySSE            // Retry-After for mailbox_polls
	mailboxMsgRetryMin       = time.Second         // floor for too_many_messages
	mailboxB64Alphabet       = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	mailboxMaxCounter        = math.MaxUint32
)

// LANGSTROTH_RELAY_MAILBOXES values.
const (
	mailboxesOn    = "on"
	mailboxesOff   = "off"
	mailboxesToken = "token"
)

// Problem and refusal codes on these routes (docs/reference.md).
const (
	mbCodeUnknown        = "mailbox_unknown"
	mbCodeRequestUnknown = "request_unknown"
	mbCodeStale          = "stale_counter"
	mbCodeFull           = "mailbox_full"
	mbCodeTooManyMsgs    = "too_many_messages"
	mbCodePutRate        = "mailbox_put_rate"
	mbCodeRegister       = "mailbox_register"
	mbCodeMailboxesFull  = "mailboxes_full"
	mbCodeStorage        = "mailbox_storage"
	mbCodePolls          = "mailbox_polls"
	mbCodeRidExists      = "rid_exists"
	mbCodeBadPutCap      = "bad_put_cap"
	mbCodeBadReadKey     = "bad_read_key"
	mbCodeBadMemberAuth  = "bad_member_auth"
	mbCodeAuthRequired   = "auth_required"
	mbCodeTokenRequired  = "token_required"
	mbCodeBadSignature   = "bad_signature"
	mbCodeMismatch       = "mailbox_mismatch"
	mbCodeInvalid        = "invalid_request"
	mbCodeTooLarge       = "too_large"
)

// mailboxRefusalCodes are the 429/507 codes counted on /metrics (fixed label set).
var mailboxRefusalCodes = []string{
	mbCodeFull, mbCodeTooManyMsgs, mbCodePutRate, mbCodeRegister, mbCodeMailboxesFull, mbCodeStorage, mbCodePolls,
}

// mailboxModeFromEnv reads LANGSTROTH_RELAY_MAILBOXES (default on).
func mailboxModeFromEnv() (string, error) {
	v := strings.ToLower(strings.TrimSpace(getenv("LANGSTROTH_RELAY_MAILBOXES")))
	switch v {
	case "", mailboxesOn:
		return mailboxesOn, nil
	case mailboxesOff, mailboxesToken:
		return v, nil
	}
	return "", fmt.Errorf("LANGSTROTH_RELAY_MAILBOXES=%q: use on, off or token", v)
}

// mailboxesConfigError refuses a mode that doesn't parse, and token mode on a
// relay with no access gate (puts would need a token any client gets for
// free, which reads as protected but isn't).
func mailboxesConfigError() error {
	mode, err := mailboxModeFromEnv()
	if err != nil {
		return err
	}
	if mode == mailboxesToken && !gatedFromEnv() {
		return errors.New("LANGSTROTH_RELAY_MAILBOXES=token needs an access gate (LANGSTROTH_RELAY_TOKEN_PUBKEY, LANGSTROTH_RELAY_ACCESS_TOKENS or LANGSTROTH_RELAY_TENANCY); on an open relay use on or off")
	}
	return nil
}

// gatedFromEnv reports whether the environment configures an access gate.
func gatedFromEnv() bool {
	return entitlementFromEnv().kind != entOpen || strings.TrimSpace(getenv("LANGSTROTH_RELAY_TENANCY")) != ""
}

// mailboxHub is the process-local mailbox store.
type mailboxHub struct {
	mode string
	// gated: the access gate refuses a request without a token. putsOpen
	// is false when token mode meets an open gate (library use; Serve
	// refuses to start): every put is then refused.
	gated    bool
	putsOpen bool

	now func() time.Time // tests move the clock

	maxBoxes     int
	maxPerClient int
	maxBytes     int64

	putLimiter  *ipRateLimiter
	polls       *concurrencyLimiter
	pollsClient *keyedConcurrencyLimiter

	refusals map[string]*atomic.Int64

	mu        sync.Mutex
	boxes     map[string]*mailbox
	perClient map[string]int
	per48     map[string]int
	bytes     int64
	reqCount  int
	sweptAt   time.Time
}

type mailbox struct {
	id         string
	authPub    ed25519.PublicKey
	putCapHash [32]byte
	counter    uint32
	client     string // clientKey of the registering client
	slash48    string
	active     time.Time // last register or member call
	reqs       map[string]*mbRequest
	reserved   map[string]time.Time // dead rid -> until when it stays taken
}

type mbRequest struct {
	rid         string
	blob        string
	readKeyHash [32]byte
	putAt       time.Time
	expiresAt   time.Time
	msgs        []mbMsg
	wake        chan struct{} // closed (and replaced) on a new message, closed when gone
	gone        bool
}

type mbMsg struct {
	Seq  int    `json:"seq"`
	Blob string `json:"blob"`
	To   string `json:"to"`
	From string `json:"from"`
	At   int64  `json:"at"`
}

func newMailboxHub(mode string, gated bool) *mailboxHub {
	h := &mailboxHub{
		mode:         mode,
		gated:        gated,
		putsOpen:     mode != mailboxesToken || gated,
		now:          time.Now,
		maxBoxes:     int(max(envInt64("LANGSTROTH_RELAY_MAX_MAILBOXES", defaultMaxMailboxes), 0)),
		maxPerClient: int(max(envInt64("LANGSTROTH_RELAY_MAX_MAILBOXES_PER_CLIENT", defaultMaxMailboxesPerCl), 0)),
		maxBytes:     max(envInt64("LANGSTROTH_RELAY_MAILBOX_MAX_BYTES", defaultMailboxMaxBytes), 0),
		putLimiter:   newIPRateLimiter(mailboxPutsPerMinute/60.0, mailboxPutsPerMinute),
		polls:        newConcurrencyLimiter(mailboxMaxPolls),
		pollsClient:  newKeyedConcurrencyLimiter(mailboxMaxPollsPerClient),
		refusals:     map[string]*atomic.Int64{},
		boxes:        map[string]*mailbox{},
		perClient:    map[string]int{},
		per48:        map[string]int{},
	}
	for _, c := range mailboxRefusalCodes {
		h.refusals[c] = new(atomic.Int64)
	}
	if !h.putsOpen {
		slog.Error("LANGSTROTH_RELAY_MAILBOXES=token on a relay with no access gate: refusing every mailbox put")
	}
	return h
}

func (s *Server) registerMailboxes(mux *http.ServeMux) {
	if s.mbox == nil {
		return // off: every route answers 404
	}
	mux.HandleFunc("GET /v1/mailboxes/{mailbox}", s.mailboxGet)
	mux.HandleFunc("PUT /v1/mailboxes/{mailbox}", s.mailboxRegister)
	mux.HandleFunc("GET /v1/mailboxes/{mailbox}/requests", s.mailboxList)
	mux.HandleFunc("PUT /v1/mailboxes/{mailbox}/requests/{rid}", s.mailboxPut)
	mux.HandleFunc("DELETE /v1/mailboxes/{mailbox}/requests/{rid}", s.mailboxDelete)
	mux.HandleFunc("POST /v1/mailboxes/{mailbox}/requests/{rid}/msgs", s.mailboxPostMsg)
	mux.HandleFunc("GET /v1/mailboxes/{mailbox}/requests/{rid}/msgs", s.mailboxGetMsgs)
}

// ---- preimages (byte-identical to ligus_core::match_code) ----

// mailboxRegisterPreimage is what a register or reset signs:
// ctx || mailbox(16) || putCapHash(32) || u32le(counter).
func mailboxRegisterPreimage(mailbox [16]byte, putCapHash [32]byte, counter uint32) []byte {
	out := make([]byte, 0, len(mailboxRegisterCtx)+16+32+4)
	out = append(out, mailboxRegisterCtx...)
	out = append(out, mailbox[:]...)
	out = append(out, putCapHash[:]...)
	return binary.LittleEndian.AppendUint32(out, counter)
}

// mailboxMemberPreimage is what a member call signs:
// ctx || method || 0 || path || 0 || i64le(ts ms) || SHA256(body).
func mailboxMemberPreimage(method, path string, tsMs int64, body []byte) []byte {
	sum := sha256.Sum256(body)
	out := make([]byte, 0, len(mailboxMemberCtx)+len(method)+len(path)+2+8+32)
	out = append(out, mailboxMemberCtx...)
	out = append(out, method...)
	out = append(out, 0)
	out = append(out, path...)
	out = append(out, 0)
	out = binary.LittleEndian.AppendUint64(out, uint64(tsMs))
	return append(out, sum[:]...)
}

// mailboxIDOf is hex(SHA256(authPub)[:16]).
func mailboxIDOf(authPub []byte) string {
	sum := sha256.Sum256(authPub)
	return hex.EncodeToString(sum[:16])
}

// ---- small helpers ----

// validHex16 reports whether s is 32 lowercase hex characters (16 bytes).
func validHex16(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// mbB64u decodes strict base64url without padding of exactly n bytes.
func mbB64u(s string, n int) ([]byte, bool) {
	b, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != n {
		return nil, false
	}
	return b, true
}

// validBlob reports whether s is a non-empty base64url string of at most limit bytes.
func validBlob(s string, limit int) bool {
	if s == "" || len(s) > limit {
		return false
	}
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(mailboxB64Alphabet, s[i]) < 0 {
			return false
		}
	}
	return true
}

func ms(t time.Time) int64 { return t.UnixMilli() }

type mbProblemBody struct {
	Error      string  `json:"error"`
	Code       string  `json:"code"`
	Message    string  `json:"message"`
	RetryAfter int64   `json:"retryAfter,omitempty"`
	Counter    *uint32 `json:"counter,omitempty"`
}

// mbProblem writes a mailbox problem document: {"error","code","message"}
// (error and code hold the same value, so a client that reads the relay's
// refusal shape and one that reads the code both work). 429 and 507 also
// carry Retry-After and retryAfter, as every refusal does.
func (h *mailboxHub) problem(w http.ResponseWriter, status int, code, msg string, wait time.Duration, counter *uint32) {
	body := mbProblemBody{Error: code, Code: code, Message: msg, Counter: counter}
	hd := w.Header()
	if status == http.StatusTooManyRequests || status == http.StatusInsufficientStorage {
		secs := int64(math.Ceil(wait.Seconds()))
		if secs < 1 {
			secs = 1
		}
		body.RetryAfter = secs
		hd.Set("Retry-After", strconv.FormatInt(secs, 10))
		if n := h.refusals[code]; n != nil {
			n.Add(1)
		}
	}
	b, _ := json.Marshal(body)
	hd.Set("Content-Type", "application/json")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

func (h *mailboxHub) bad(w http.ResponseWriter, msg string) {
	h.problem(w, http.StatusBadRequest, mbCodeInvalid, msg, 0, nil)
}

func mbWriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, v)
}

// readBody reads at most limit bytes of the body (413 past it).
func (h *mailboxHub) readBody(w http.ResponseWriter, r *http.Request, limit int) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, int64(limit))
	b, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.problem(w, http.StatusRequestEntityTooLarge, mbCodeTooLarge, "request body too large", 0, nil)
		} else {
			h.bad(w, "could not read the request body")
		}
		return nil, false
	}
	return b, true
}

// pathIDs validates {mailbox} (and {rid} when the route has one).
func (s *Server) mailboxPathIDs(w http.ResponseWriter, r *http.Request, withRID bool) (string, string, bool) {
	mb := r.PathValue("mailbox")
	if !validHex16(mb) {
		s.mbox.bad(w, "mailbox must be 32 lowercase hex characters")
		return "", "", false
	}
	if !withRID {
		return mb, "", true
	}
	rid := r.PathValue("rid")
	if !validHex16(rid) {
		s.mbox.bad(w, "rid must be 32 lowercase hex characters")
		return "", "", false
	}
	return mb, rid, true
}

// tokenOK checks the access token as the gate would (any valid token; a
// mailbox isn't bound to a room). Open relay: always true.
func (s *Server) mailboxTokenOK(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := s.entitlement.Allow(bearerToken(r), nowUnix()); !ok {
		s.mbox.problem(w, http.StatusUnauthorized, mbCodeTokenRequired, "relay requires a valid access token", 0, nil)
		return false
	}
	return true
}

// ---- hub internals (h.mu held) ----

// sweepBoxLocked expires box's requests and forgets dead rids past their hold.
func (h *mailboxHub) sweepBoxLocked(b *mailbox, now time.Time) {
	for rid, q := range b.reqs {
		if !now.Before(q.expiresAt) {
			h.dropRequestLocked(b, q)
			delete(b.reqs, rid)
		}
	}
	for rid, until := range b.reserved {
		if !now.Before(until) {
			delete(b.reserved, rid)
		}
	}
}

// dropRequestLocked frees q's bytes, wakes its pollers and holds its rid.
func (h *mailboxHub) dropRequestLocked(b *mailbox, q *mbRequest) {
	n := int64(len(q.blob))
	for _, m := range q.msgs {
		n += int64(len(m.Blob))
	}
	h.bytes -= n
	h.reqCount--
	q.gone = true
	q.msgs = nil
	close(q.wake)
	b.reserved[q.rid] = q.expiresAt.Add(mailboxReserveFor)
}

// sweepAllLocked sweeps every mailbox and forgets idle, empty ones.
func (h *mailboxHub) sweepAllLocked(now time.Time) {
	h.sweptAt = now
	for id, b := range h.boxes {
		h.sweepBoxLocked(b, now)
		if len(b.reqs) == 0 && now.Sub(b.active) > mailboxIdleTTL {
			h.forgetLocked(id, b)
		}
	}
}

func (h *mailboxHub) forgetLocked(id string, b *mailbox) {
	delete(h.boxes, id)
	if h.perClient[b.client]--; h.perClient[b.client] <= 0 {
		delete(h.perClient, b.client)
	}
	if b.slash48 != "" {
		if h.per48[b.slash48]--; h.per48[b.slash48] <= 0 {
			delete(h.per48, b.slash48)
		}
	}
}

// boxLocked returns the live mailbox id (swept), or nil.
func (h *mailboxHub) boxLocked(id string, now time.Time) *mailbox {
	if now.Sub(h.sweptAt) >= mailboxSweepEvery {
		h.sweepAllLocked(now)
	}
	b := h.boxes[id]
	if b != nil {
		h.sweepBoxLocked(b, now)
	}
	return b
}

func (h *mailboxHub) sweep() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sweepAllLocked(h.now())
}

// authPub returns a copy of the registered key of mailbox id.
func (h *mailboxHub) authPub(id string) (ed25519.PublicKey, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.boxLocked(id, h.now())
	if b == nil {
		return nil, false
	}
	return append(ed25519.PublicKey(nil), b.authPub...), true
}

// mailboxSweepLoop expires requests in the background (Serve), so a sealed
// result nobody fetched doesn't sit in memory past its 15 minutes.
func (s *Server) mailboxSweepLoop(stop <-chan struct{}) {
	if s.mbox == nil {
		return
	}
	t := time.NewTicker(mailboxSweepLoopInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.mbox.sweep()
		}
	}
}

// ---- auth ----

type mbCaller struct {
	member  bool
	keyHash [32]byte // requester: SHA-256 of the read key
}

func (c mbCaller) from() string {
	if c.member {
		return "member"
	}
	return "requester"
}

// memberAuth verifies X-Ligus-Mailbox-Auth for mailbox mb over body (and the
// access token on a gated relay). It writes the problem and returns false on
// failure. A known mailbox's member call also refreshes its idle timer.
func (s *Server) mailboxMemberAuth(w http.ResponseWriter, r *http.Request, mb string, body []byte) bool {
	h := s.mbox
	hdr := strings.TrimSpace(r.Header.Get(mailboxAuthHeader))
	if hdr == "" {
		h.problem(w, http.StatusUnauthorized, mbCodeAuthRequired, "member auth required ("+mailboxAuthHeader+")", 0, nil)
		return false
	}
	if !s.mailboxTokenOK(w, r) {
		return false
	}
	tsText, sigText, ok := strings.Cut(hdr, ".")
	ts, err := strconv.ParseInt(tsText, 10, 64)
	sig, sigOK := mbB64u(sigText, ed25519.SignatureSize)
	if !ok || err != nil || !sigOK {
		h.problem(w, http.StatusUnauthorized, mbCodeBadMemberAuth, "malformed "+mailboxAuthHeader+" (want <ts ms>.<b64u signature>)", 0, nil)
		return false
	}
	now := h.now()
	if d := time.Duration(now.UnixMilli()-ts) * time.Millisecond; d > mailboxAuthWindow || d < -mailboxAuthWindow {
		h.problem(w, http.StatusUnauthorized, mbCodeBadMemberAuth, "member auth timestamp is more than 300 s from the relay's clock", 0, nil)
		return false
	}
	pub, ok := h.authPub(mb)
	if !ok {
		h.problem(w, http.StatusNotFound, mbCodeUnknown, "no such mailbox", 0, nil)
		return false
	}
	if !ed25519.Verify(pub, mailboxMemberPreimage(r.Method, r.URL.Path, ts, body), sig) {
		h.problem(w, http.StatusUnauthorized, mbCodeBadMemberAuth, "member auth signature does not verify", 0, nil)
		return false
	}
	h.mu.Lock()
	if b := h.boxes[mb]; b != nil {
		b.active = h.now()
	}
	h.mu.Unlock()
	return true
}

// mailboxCaller authenticates a member or a requester (read key).
func (s *Server) mailboxCaller(w http.ResponseWriter, r *http.Request, mb string, body []byte) (mbCaller, bool) {
	if strings.TrimSpace(r.Header.Get(mailboxAuthHeader)) != "" {
		return mbCaller{member: true}, s.mailboxMemberAuth(w, r, mb, body)
	}
	k := strings.TrimSpace(r.Header.Get(mailboxReadKeyHeader))
	if k == "" {
		s.mbox.problem(w, http.StatusUnauthorized, mbCodeAuthRequired, "member auth ("+mailboxAuthHeader+") or a read key ("+mailboxReadKeyHeader+") required", 0, nil)
		return mbCaller{}, false
	}
	raw, ok := mbB64u(k, 16)
	if !ok {
		s.mbox.problem(w, http.StatusForbidden, mbCodeBadReadKey, "read key must be 16 bytes, base64url without padding", 0, nil)
		return mbCaller{}, false
	}
	return mbCaller{keyHash: sha256.Sum256(raw)}, true
}

// requestFor finds rid in mb for caller c (h.mu held). It writes the problem
// and returns nil on failure.
func (h *mailboxHub) requestForLocked(w http.ResponseWriter, mb, rid string, c mbCaller) (*mailbox, *mbRequest) {
	b := h.boxLocked(mb, h.now())
	if b == nil {
		h.problem(w, http.StatusNotFound, mbCodeUnknown, "no such mailbox", 0, nil)
		return nil, nil
	}
	q := b.reqs[rid]
	if q == nil {
		h.problem(w, http.StatusNotFound, mbCodeRequestUnknown, "no such request (unknown, expired or deleted)", 0, nil)
		return nil, nil
	}
	if !c.member && subtle.ConstantTimeCompare(c.keyHash[:], q.readKeyHash[:]) != 1 {
		h.problem(w, http.StatusForbidden, mbCodeBadReadKey, "read key does not match this request", 0, nil)
		return nil, nil
	}
	return b, q
}

// ---- handlers ----

type mailboxInfo struct {
	Mailbox string `json:"mailbox"`
	Counter uint32 `json:"counter"`
}

// mailboxGet: GET /v1/mailboxes/{mailbox} (no auth) -> {mailbox, counter}.
func (s *Server) mailboxGet(w http.ResponseWriter, r *http.Request) {
	mb, _, ok := s.mailboxPathIDs(w, r, false)
	if !ok || rateRefused(w, s.readLimiter, clientKey(r), refReadRate, "rate limit exceeded") {
		return
	}
	h := s.mbox
	h.mu.Lock()
	b := h.boxLocked(mb, h.now())
	var info mailboxInfo
	if b != nil {
		info = mailboxInfo{Mailbox: b.id, Counter: b.counter}
	}
	h.mu.Unlock()
	if b == nil {
		h.problem(w, http.StatusNotFound, mbCodeUnknown, "no such mailbox", 0, nil)
		return
	}
	mbWriteJSON(w, http.StatusOK, info)
}

type mailboxRegisterReq struct {
	AuthPub    string          `json:"authPub"`
	PutCapHash string          `json:"putCapHash"`
	Counter    json.RawMessage `json:"counter"`
	Sig        string          `json:"sig"`
}

// mailboxRegister: PUT /v1/mailboxes/{mailbox} registers or resets a mailbox.
func (s *Server) mailboxRegister(w http.ResponseWriter, r *http.Request) {
	mb, _, ok := s.mailboxPathIDs(w, r, false)
	if !ok || !s.limitWrite(w, r) || !s.mailboxTokenOK(w, r) {
		return
	}
	h := s.mbox
	raw, ok := h.readBody(w, r, mailboxRegisterBodyCap)
	if !ok {
		return
	}
	var req mailboxRegisterReq
	if json.Unmarshal(raw, &req) != nil {
		h.bad(w, "invalid JSON body")
		return
	}
	authPub, ok1 := mbB64u(req.AuthPub, ed25519.PublicKeySize)
	capHash, ok2 := mbB64u(req.PutCapHash, sha256.Size)
	sig, ok3 := mbB64u(req.Sig, ed25519.SignatureSize)
	counter, err := strconv.ParseUint(strings.TrimSpace(string(req.Counter)), 10, 32)
	switch {
	case !ok1:
		h.bad(w, "authPub must be 32 bytes, base64url without padding")
		return
	case !ok2:
		h.bad(w, "putCapHash must be 32 bytes, base64url without padding")
		return
	case !ok3:
		h.bad(w, "sig must be 64 bytes, base64url without padding")
		return
	case err != nil || counter > mailboxMaxCounter:
		h.bad(w, "counter must be an integer from 0 to 4294967295")
		return
	}
	// Self-certifying: the cheap check first, then the signature.
	if mailboxIDOf(authPub) != mb {
		h.problem(w, http.StatusForbidden, mbCodeMismatch, "mailbox is not hex(SHA256(authPub)[:16])", 0, nil)
		return
	}
	var mbRaw [16]byte
	_, _ = hex.Decode(mbRaw[:], []byte(mb))
	var ch [32]byte
	copy(ch[:], capHash)
	if !ed25519.Verify(authPub, mailboxRegisterPreimage(mbRaw, ch, uint32(counter)), sig) {
		h.problem(w, http.StatusForbidden, mbCodeBadSignature, "sig does not verify under authPub", 0, nil)
		return
	}
	n := uint32(counter)
	now := h.now()
	h.mu.Lock()
	b := h.boxLocked(mb, now)
	switch {
	case b == nil:
		client, s48 := clientKey(r), slash48Key(r)
		if h.maxPerClient > 0 && (h.perClient[client] >= h.maxPerClient || s48 != "" && h.per48[s48] >= h.maxPerClient*mailboxSlash48Factor) {
			h.mu.Unlock()
			h.problem(w, http.StatusTooManyRequests, mbCodeRegister, "too many mailboxes registered from this client", mailboxRetryRegister, nil)
			return
		}
		if h.maxBoxes > 0 && len(h.boxes) >= h.maxBoxes {
			h.mu.Unlock()
			h.problem(w, http.StatusInsufficientStorage, mbCodeMailboxesFull, "relay mailbox capacity reached", mailboxRetryStorage, nil)
			return
		}
		h.boxes[mb] = &mailbox{
			id: mb, authPub: append(ed25519.PublicKey(nil), authPub...), putCapHash: ch, counter: n,
			client: client, slash48: s48, active: now,
			reqs: map[string]*mbRequest{}, reserved: map[string]time.Time{},
		}
		h.perClient[client]++
		if s48 != "" {
			h.per48[s48]++
		}
		h.mu.Unlock()
		mbWriteJSON(w, http.StatusCreated, mailboxInfo{Mailbox: mb, Counter: n})
	case n == b.counter && ch == b.putCapHash, n > b.counter:
		// Same registration again (idempotent), or a reset: the old cap
		// stops working at once. Live requests stay.
		b.counter, b.putCapHash, b.active = n, ch, now
		h.mu.Unlock()
		mbWriteJSON(w, http.StatusOK, mailboxInfo{Mailbox: mb, Counter: n})
	default:
		cur := b.counter
		h.mu.Unlock()
		h.problem(w, http.StatusConflict, mbCodeStale, "counter is below the registered one, or equal with another putCapHash", 0, &cur)
	}
}

type mailboxPutReq struct {
	Blob        string `json:"blob"`
	ReadKeyHash string `json:"readKeyHash"`
}

type mailboxPutResp struct {
	RID       string `json:"rid"`
	PutAt     int64  `json:"putAt"`
	ExpiresAt int64  `json:"expiresAt"`
}

// mailboxPut: PUT /v1/mailboxes/{mailbox}/requests/{rid} (X-Ligus-Put-Cap).
func (s *Server) mailboxPut(w http.ResponseWriter, r *http.Request) {
	mb, rid, ok := s.mailboxPathIDs(w, r, true)
	if !ok {
		return
	}
	h := s.mbox
	if !h.putsOpen {
		h.problem(w, http.StatusUnauthorized, mbCodeTokenRequired, "mailbox puts need an access token, and this relay has no access gate", 0, nil)
		return
	}
	if h.mode == mailboxesToken && !s.mailboxTokenOK(w, r) {
		return
	}
	if !s.limitWrite(w, r) {
		return
	}
	if ok, wait := h.putLimiter.allowWait(clientKey(r)); !ok {
		h.problem(w, http.StatusTooManyRequests, mbCodePutRate, "too many mailbox puts from this client (10 a minute)", wait, nil)
		return
	}
	raw, ok := h.readBody(w, r, mailboxMaxRequestBlob+4096)
	if !ok {
		return
	}
	var req mailboxPutReq
	if json.Unmarshal(raw, &req) != nil {
		h.bad(w, "invalid JSON body")
		return
	}
	if len(req.Blob) > mailboxMaxRequestBlob {
		h.problem(w, http.StatusRequestEntityTooLarge, mbCodeTooLarge, "blob is over 8 KiB", 0, nil)
		return
	}
	if !validBlob(req.Blob, mailboxMaxRequestBlob) {
		h.bad(w, "blob must be a non-empty base64url string")
		return
	}
	rkh, ok := mbB64u(req.ReadKeyHash, sha256.Size)
	if !ok {
		h.bad(w, "readKeyHash must be 32 bytes, base64url without padding")
		return
	}
	capRaw, capOK := mbB64u(r.Header.Get(mailboxPutCapHeader), 16)
	capHash := sha256.Sum256(capRaw)

	now := h.now()
	h.mu.Lock()
	b := h.boxLocked(mb, now)
	if b == nil {
		h.mu.Unlock()
		h.problem(w, http.StatusNotFound, mbCodeUnknown, "no such mailbox", 0, nil)
		return
	}
	if !capOK || subtle.ConstantTimeCompare(capHash[:], b.putCapHash[:]) != 1 {
		h.mu.Unlock()
		h.problem(w, http.StatusForbidden, mbCodeBadPutCap, "put cap missing or not the mailbox's current one", 0, nil)
		return
	}
	if _, live := b.reqs[rid]; live {
		h.mu.Unlock()
		h.problem(w, http.StatusConflict, mbCodeRidExists, "rid exists", 0, nil)
		return
	}
	if _, dead := b.reserved[rid]; dead {
		h.mu.Unlock()
		h.problem(w, http.StatusConflict, mbCodeRidExists, "rid was used and can't be put again yet", 0, nil)
		return
	}
	if len(b.reqs) >= mailboxMaxLive || len(b.reserved) >= mailboxMaxReserved {
		wait := mailboxRetryStale
		if len(b.reqs) >= mailboxMaxLive {
			oldest := now.Add(mailboxRequestTTL)
			for _, q := range b.reqs {
				if q.expiresAt.Before(oldest) {
					oldest = q.expiresAt
				}
			}
			wait = oldest.Sub(now)
		}
		h.mu.Unlock()
		h.problem(w, http.StatusTooManyRequests, mbCodeFull, "mailbox holds 64 live requests", wait, nil)
		return
	}
	if h.maxBytes > 0 && h.bytes+int64(len(req.Blob)) > h.maxBytes {
		h.mu.Unlock()
		h.problem(w, http.StatusInsufficientStorage, mbCodeStorage, "relay mailbox storage is full", mailboxRetryStorage, nil)
		return
	}
	q := &mbRequest{rid: rid, blob: req.Blob, putAt: now, expiresAt: now.Add(mailboxRequestTTL), wake: make(chan struct{})}
	copy(q.readKeyHash[:], rkh)
	b.reqs[rid] = q
	h.bytes += int64(len(req.Blob))
	h.reqCount++
	h.mu.Unlock()
	mbWriteJSON(w, http.StatusCreated, mailboxPutResp{RID: rid, PutAt: ms(q.putAt), ExpiresAt: ms(q.expiresAt)})
}

type mailboxListItem struct {
	RID       string `json:"rid"`
	Blob      string `json:"blob"`
	PutAt     int64  `json:"putAt"`
	ExpiresAt int64  `json:"expiresAt"`
	MsgCount  int    `json:"msgCount"`
}

// mailboxList: GET /v1/mailboxes/{mailbox}/requests (member) -> live requests, oldest first.
func (s *Server) mailboxList(w http.ResponseWriter, r *http.Request) {
	mb, _, ok := s.mailboxPathIDs(w, r, false)
	if !ok || rateRefused(w, s.readLimiter, clientKey(r), refReadRate, "rate limit exceeded") || !s.mailboxMemberAuth(w, r, mb, nil) {
		return
	}
	h := s.mbox
	h.mu.Lock()
	b := h.boxLocked(mb, h.now())
	if b == nil {
		h.mu.Unlock()
		h.problem(w, http.StatusNotFound, mbCodeUnknown, "no such mailbox", 0, nil)
		return
	}
	items := make([]mailboxListItem, 0, len(b.reqs))
	for _, q := range b.reqs {
		items = append(items, mailboxListItem{RID: q.rid, Blob: q.blob, PutAt: ms(q.putAt), ExpiresAt: ms(q.expiresAt), MsgCount: len(q.msgs)})
	}
	h.mu.Unlock()
	sort.Slice(items, func(i, j int) bool {
		if items[i].PutAt != items[j].PutAt {
			return items[i].PutAt < items[j].PutAt
		}
		return items[i].RID < items[j].RID
	})
	mbWriteJSON(w, http.StatusOK, map[string]any{"requests": items})
}

type mailboxMsgReq struct {
	Blob string `json:"blob"`
	To   string `json:"to"`
}

// mailboxPostMsg: POST .../requests/{rid}/msgs (member or requester).
func (s *Server) mailboxPostMsg(w http.ResponseWriter, r *http.Request) {
	mb, rid, ok := s.mailboxPathIDs(w, r, true)
	if !ok || !s.limitWrite(w, r) {
		return
	}
	h := s.mbox
	raw, ok := h.readBody(w, r, mailboxMaxMsgBlob+4096)
	if !ok {
		return
	}
	c, ok := s.mailboxCaller(w, r, mb, raw)
	if !ok {
		return
	}
	var req mailboxMsgReq
	if json.Unmarshal(raw, &req) != nil {
		h.bad(w, "invalid JSON body")
		return
	}
	switch req.To {
	case "members", "requester", "approver":
	default:
		h.bad(w, `to must be "members", "requester" or "approver"`)
		return
	}
	if len(req.Blob) > mailboxMaxMsgBlob {
		h.problem(w, http.StatusRequestEntityTooLarge, mbCodeTooLarge, "blob is over 32 KiB", 0, nil)
		return
	}
	if !validBlob(req.Blob, mailboxMaxMsgBlob) {
		h.bad(w, "blob must be a non-empty base64url string")
		return
	}
	h.mu.Lock()
	_, q := h.requestForLocked(w, mb, rid, c)
	if q == nil {
		h.mu.Unlock()
		return
	}
	now := h.now()
	if len(q.msgs) >= mailboxMaxMsgs {
		wait := max(q.expiresAt.Sub(now), mailboxMsgRetryMin)
		h.mu.Unlock()
		h.problem(w, http.StatusTooManyRequests, mbCodeTooManyMsgs, "this request holds 10 messages", wait, nil)
		return
	}
	if h.maxBytes > 0 && h.bytes+int64(len(req.Blob)) > h.maxBytes {
		h.mu.Unlock()
		h.problem(w, http.StatusInsufficientStorage, mbCodeStorage, "relay mailbox storage is full", mailboxRetryStorage, nil)
		return
	}
	seq := len(q.msgs) + 1
	q.msgs = append(q.msgs, mbMsg{Seq: seq, Blob: req.Blob, To: req.To, From: c.from(), At: ms(now)})
	h.bytes += int64(len(req.Blob))
	close(q.wake)
	q.wake = make(chan struct{})
	h.mu.Unlock()
	mbWriteJSON(w, http.StatusCreated, map[string]int{"seq": seq})
}

// mailboxGetMsgs: GET .../requests/{rid}/msgs?after=N&wait=S (member or
// requester). With wait, it holds the request up to min(S, 25) s while
// nothing is after N.
func (s *Server) mailboxGetMsgs(w http.ResponseWriter, r *http.Request) {
	mb, rid, ok := s.mailboxPathIDs(w, r, true)
	if !ok || rateRefused(w, s.readLimiter, clientKey(r), refReadRate, "rate limit exceeded") {
		return
	}
	h := s.mbox
	c, ok := s.mailboxCaller(w, r, mb, nil)
	if !ok {
		return
	}
	qv := r.URL.Query()
	var after int
	if v := qv.Get("after"); v != "" {
		n, err := strconv.ParseUint(v, 10, 31)
		if err != nil {
			h.bad(w, "after must be a non-negative integer")
			return
		}
		after = int(n)
	}
	var wait time.Duration
	if v := qv.Get("wait"); v != "" {
		n, err := strconv.ParseUint(v, 10, 31)
		if err != nil {
			h.bad(w, "wait must be a non-negative integer (seconds)")
			return
		}
		wait = min(time.Duration(n)*time.Second, mailboxMaxWait)
	}
	if wait > 0 {
		rel, ok := h.polls.acquire()
		if !ok {
			h.problem(w, http.StatusTooManyRequests, mbCodePolls, "too many mailbox long polls on this relay", mailboxPollRetry, nil)
			return
		}
		defer rel()
		relC, ok := h.pollsClient.acquire(clientKey(r))
		if !ok {
			h.problem(w, http.StatusTooManyRequests, mbCodePolls, "too many mailbox long polls from this client", mailboxPollRetry, nil)
			return
		}
		defer relC()
	}
	deadline := time.Now().Add(wait)
	for {
		h.mu.Lock()
		_, q := h.requestForLocked(w, mb, rid, c)
		if q == nil {
			h.mu.Unlock()
			return
		}
		var out []mbMsg
		if after < len(q.msgs) {
			out = append(out, q.msgs[after:]...)
		}
		wake := q.wake
		h.mu.Unlock()
		left := time.Until(deadline)
		if len(out) > 0 || left <= 0 {
			if out == nil {
				out = []mbMsg{}
			}
			mbWriteJSON(w, http.StatusOK, map[string]any{"msgs": out})
			return
		}
		if !waitWake(r.Context(), wake, left) && r.Context().Err() != nil {
			return
		}
	}
}

// waitWake waits for wake to close, d to pass or ctx to end; true when woken.
func waitWake(ctx context.Context, wake <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-wake:
		return true
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// mailboxDelete: DELETE .../requests/{rid} (requester, read key) -> 204.
// The rid stays taken until after its expiry would have passed.
func (s *Server) mailboxDelete(w http.ResponseWriter, r *http.Request) {
	mb, rid, ok := s.mailboxPathIDs(w, r, true)
	if !ok || !s.limitWrite(w, r) {
		return
	}
	h := s.mbox
	if strings.TrimSpace(r.Header.Get(mailboxReadKeyHeader)) == "" {
		h.problem(w, http.StatusUnauthorized, mbCodeAuthRequired, "the requester's read key ("+mailboxReadKeyHeader+") is required", 0, nil)
		return
	}
	// A read key only: members can't delete a request (wire contract).
	r.Header.Del(mailboxAuthHeader)
	c, ok := s.mailboxCaller(w, r, mb, nil)
	if !ok {
		return
	}
	h.mu.Lock()
	b, q := h.requestForLocked(w, mb, rid, c)
	if q == nil {
		h.mu.Unlock()
		return
	}
	h.dropRequestLocked(b, q)
	delete(b.reqs, rid)
	h.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// writeMailboxMetrics appends the mailbox series to /metrics.
func (s *Server) writeMailboxMetrics(w io.Writer) {
	h := s.mbox
	if h == nil {
		return
	}
	h.mu.Lock()
	boxes, reqs, bytes := len(h.boxes), h.reqCount, h.bytes
	h.mu.Unlock()
	fmt.Fprintf(w, "# HELP hive_relay_mailboxes Registered approval mailboxes.\n# TYPE hive_relay_mailboxes gauge\nhive_relay_mailboxes %d\n", boxes)
	fmt.Fprintf(w, "# HELP hive_relay_mailbox_requests Live approval requests.\n# TYPE hive_relay_mailbox_requests gauge\nhive_relay_mailbox_requests %d\n", reqs)
	fmt.Fprintf(w, "# HELP hive_relay_mailbox_bytes Blob bytes held by live approval requests and their messages.\n# TYPE hive_relay_mailbox_bytes gauge\nhive_relay_mailbox_bytes %d\n", bytes)
	fmt.Fprintf(w, "# HELP hive_relay_mailbox_refusals_total Mailbox requests refused by a limit (429 or 507).\n# TYPE hive_relay_mailbox_refusals_total counter\n")
	for _, c := range mailboxRefusalCodes {
		fmt.Fprintf(w, "hive_relay_mailbox_refusals_total{code=%q} %d\n", c, h.refusals[c].Load())
	}
}
