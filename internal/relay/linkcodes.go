package relay

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Short link codes (Ligus 0.5): a person's existing device shows a short
// code ("K7Q-4MX"), and the new device types it instead of pasting the
// approval address. The client derives two things from the short code with
// Argon2id: a 16-byte lookup id, the {code} in these paths, and a key that
// seals the approval address. The relay stores the lookup id, the mailbox it
// belongs to and the sealed address, and never sees the short code or the
// address: what it learns is that a member of a mailbox it already knows
// asked for a code. docs/reference.md, "Short link codes", is the contract.
//
//   - PUT    /v1/mailboxes/{mailbox}/codes/{code}  member: post or re-post
//   - GET    /v1/mailboxes/{mailbox}/codes/{code}  member: is it live, used?
//   - DELETE /v1/mailboxes/{mailbox}/codes/{code}  member: withdraw it
//   - POST   /v1/link-codes/{code}                 anyone: resolve, single-use
//
// Codes live in memory with the mailboxes (off with them, gone after a
// restart); the device that shows a code posts it again when a status call
// answers code_unknown.

const (
	linkCodeTTLDefault      = 10 * time.Minute
	linkCodeTTLMin          = time.Minute
	linkCodeTTLMax          = 10 * time.Minute
	linkCodeMaxBlob         = 2 << 10 // one sealed address
	linkCodeMaxPerBox       = 4       // live, unused codes per mailbox
	linkCodeMaxPerOwner     = 32      // codes held (used ones too) posted from one IPv4 address or IPv6 /48
	linkCodeMaxLive         = 10000   // codes held per process (used ones included until they expire)
	linkCodePostsPerMinute  = 10      // posts per mailbox
	linkCodeResolvesPerMin  = 10      // resolves per client (/64)
	linkCodeResolves48Scale = mailboxSlash48Factor
	linkCodeBodyCap         = linkCodeMaxBlob + 512
	linkCodeRetryFull       = time.Minute
)

// Problem codes on these routes.
const (
	lcCodeUnknown  = "code_unknown"
	lcCodeTaken    = "code_taken"
	lcCodeUsed     = "code_used"
	lcCodeFull     = "link_codes_full"
	lcCodeRate     = "link_code_rate"
	lcCodeAttempts = "link_code_attempts"
)

type linkCode struct {
	mailbox   string
	owner     string // the posting client's IPv4 address or IPv6 /48
	blob      string
	expiresAt time.Time
	used      bool
}

// linkCodes is the process-local code store. It has its own lock, taken
// after member auth (which takes the mailbox hub's).
type linkCodes struct {
	posts     *ipRateLimiter // per mailbox
	resolves  *ipRateLimiter // per client (/64)
	resolve48 *ipRateLimiter // per IPv6 /48

	mu      sync.Mutex
	codes   map[string]*linkCode
	sweptAt time.Time
}

func newLinkCodes() *linkCodes {
	return &linkCodes{
		posts:     newIPRateLimiter(linkCodePostsPerMinute/60.0, linkCodePostsPerMinute),
		resolves:  newIPRateLimiter(linkCodeResolvesPerMin/60.0, linkCodeResolvesPerMin),
		resolve48: newIPRateLimiter(linkCodeResolves48Scale*linkCodeResolvesPerMin/60.0, linkCodeResolves48Scale*linkCodeResolvesPerMin),
		codes:     map[string]*linkCode{},
	}
}

func (s *Server) registerLinkCodes(mux *http.ServeMux) {
	if s.mbox == nil {
		return // off with the mailboxes: every route answers 404
	}
	mux.HandleFunc("PUT /v1/mailboxes/{mailbox}/codes/{code}", s.linkCodePut)
	mux.HandleFunc("GET /v1/mailboxes/{mailbox}/codes/{code}", s.linkCodeStatus)
	mux.HandleFunc("DELETE /v1/mailboxes/{mailbox}/codes/{code}", s.linkCodeDelete)
	mux.HandleFunc("POST /v1/link-codes/{code}", s.linkCodeResolve)
}

// sweepLocked drops expired codes, at most once a second (lookups check
// expiry themselves). Caller holds c.mu.
func (c *linkCodes) sweepLocked(now time.Time) {
	if !c.sweptAt.IsZero() && now.Sub(c.sweptAt) < time.Second {
		return
	}
	c.sweptAt = now
	for k, v := range c.codes {
		if !now.Before(v.expiresAt) {
			delete(c.codes, k)
		}
	}
}

// liveLocked is the unexpired code k, or nil. Caller holds c.mu.
func (c *linkCodes) liveLocked(k string, now time.Time) *linkCode {
	c.sweepLocked(now)
	v := c.codes[k]
	if v == nil || !now.Before(v.expiresAt) {
		return nil
	}
	return v
}

func (s *Server) linkCodePathIDs(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	mb, _, ok := s.mailboxPathIDs(w, r, false)
	if !ok {
		return "", "", false
	}
	code := r.PathValue("code")
	if !validHex16(code) {
		s.mbox.bad(w, "code must be 32 lowercase hex characters (the lookup id, never the short code)")
		return "", "", false
	}
	return mb, code, true
}

type linkCodePutReq struct {
	Blob    string           `json:"blob"`
	TTLSecs *json.RawMessage `json:"ttlSecs"`
}

type linkCodeInfo struct {
	Code      string `json:"code"`
	ExpiresAt int64  `json:"expiresAt"`
	Used      bool   `json:"used"`
}

// linkCodePut: PUT /v1/mailboxes/{mailbox}/codes/{code} (member). A new code
// is 201; the same mailbox posting it again while it is live and unused
// replaces the blob and keeps the expiry (200).
func (s *Server) linkCodePut(w http.ResponseWriter, r *http.Request) {
	mb, code, ok := s.linkCodePathIDs(w, r)
	if !ok || !s.limitWrite(w, r) {
		return
	}
	h := s.mbox
	raw, ok := h.readBody(w, r, linkCodeBodyCap)
	if !ok || !s.mailboxMemberAuth(w, r, mb, raw) {
		return
	}
	var req linkCodePutReq
	if json.Unmarshal(raw, &req) != nil {
		h.bad(w, "invalid JSON body")
		return
	}
	if len(req.Blob) > linkCodeMaxBlob {
		h.problem(w, http.StatusRequestEntityTooLarge, mbCodeTooLarge, "blob is over 2 KiB", 0, nil)
		return
	}
	if !validBlob(req.Blob, linkCodeMaxBlob) {
		h.bad(w, "blob must be a non-empty base64url string")
		return
	}
	ttl := linkCodeTTLDefault
	if req.TTLSecs != nil {
		var n int64
		if json.Unmarshal(*req.TTLSecs, &n) != nil || n < int64(linkCodeTTLMin/time.Second) || n > int64(linkCodeTTLMax/time.Second) {
			h.bad(w, "ttlSecs must be an integer from 60 to 600")
			return
		}
		ttl = time.Duration(n) * time.Second
	}
	if ok, wait := s.codes.posts.allowWait(mb); !ok {
		h.problem(w, http.StatusTooManyRequests, lcCodeRate, "too many codes posted for this mailbox (10 a minute)", wait, nil)
		return
	}
	c := s.codes
	now := h.now()
	c.mu.Lock()
	cur := c.liveLocked(code, now)
	switch {
	case cur != nil && cur.mailbox != mb:
		c.mu.Unlock()
		h.problem(w, http.StatusConflict, lcCodeTaken, "another mailbox holds this code: pick another", 0, nil)
		return
	case cur != nil && cur.used:
		c.mu.Unlock()
		h.problem(w, http.StatusConflict, lcCodeUsed, "this code was used: pick another", 0, nil)
		return
	case cur != nil:
		cur.blob = req.Blob
		info := linkCodeInfo{Code: code, ExpiresAt: ms(cur.expiresAt)}
		c.mu.Unlock()
		mbWriteJSON(w, http.StatusOK, info)
		return
	}
	owner := mailboxOwner(r)
	n, byOwner := 0, 0
	for _, v := range c.codes {
		if !now.Before(v.expiresAt) {
			continue
		}
		if v.mailbox == mb && !v.used {
			n++
		}
		if v.owner == owner {
			byOwner++
		}
	}
	if n >= linkCodeMaxPerBox {
		c.mu.Unlock()
		h.problem(w, http.StatusTooManyRequests, lcCodeFull, "this mailbox holds 4 live codes", linkCodeRetryFull, nil)
		return
	}
	// Anyone can register a mailbox, so the relay-wide pool is also split
	// by where codes come from: one network can't fill it for everyone.
	if byOwner >= linkCodeMaxPerOwner {
		c.mu.Unlock()
		h.problem(w, http.StatusTooManyRequests, lcCodeFull, "too many codes from this client address", linkCodeRetryFull, nil)
		return
	}
	if len(c.codes) >= linkCodeMaxLive {
		c.mu.Unlock()
		h.problem(w, http.StatusInsufficientStorage, lcCodeFull, "relay link code capacity reached", linkCodeRetryFull, nil)
		return
	}
	v := &linkCode{mailbox: mb, owner: owner, blob: req.Blob, expiresAt: now.Add(ttl)}
	c.codes[code] = v
	c.mu.Unlock()
	mbWriteJSON(w, http.StatusCreated, linkCodeInfo{Code: code, ExpiresAt: ms(v.expiresAt)})
}

// linkCodeStatus: GET /v1/mailboxes/{mailbox}/codes/{code} (member) ->
// {code, expiresAt, used}; 404 code_unknown when it isn't this mailbox's
// live code (a restart, its expiry or a delete): post it again.
func (s *Server) linkCodeStatus(w http.ResponseWriter, r *http.Request) {
	mb, code, ok := s.linkCodePathIDs(w, r)
	if !ok || rateRefused(w, s.readLimiter, clientKey(r), refReadRate, "rate limit exceeded") || !s.mailboxMemberAuth(w, r, mb, nil) {
		return
	}
	c := s.codes
	c.mu.Lock()
	v := c.liveLocked(code, s.mbox.now())
	var info linkCodeInfo
	if v != nil && v.mailbox == mb {
		info = linkCodeInfo{Code: code, ExpiresAt: ms(v.expiresAt), Used: v.used}
	}
	c.mu.Unlock()
	if info.Code == "" {
		s.mbox.problem(w, http.StatusNotFound, lcCodeUnknown, "no such code for this mailbox (expired, deleted, or the relay restarted)", 0, nil)
		return
	}
	mbWriteJSON(w, http.StatusOK, info)
}

// linkCodeDelete: DELETE /v1/mailboxes/{mailbox}/codes/{code} (member) ->
// 204, also when it is already gone.
func (s *Server) linkCodeDelete(w http.ResponseWriter, r *http.Request) {
	mb, code, ok := s.linkCodePathIDs(w, r)
	if !ok || !s.limitWrite(w, r) || !s.mailboxMemberAuth(w, r, mb, nil) {
		return
	}
	c := s.codes
	c.mu.Lock()
	if v := c.codes[code]; v != nil && v.mailbox == mb {
		delete(c.codes, code)
	}
	c.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

type linkCodeResolved struct {
	Mailbox   string `json:"mailbox"`
	Blob      string `json:"blob"`
	ExpiresAt int64  `json:"expiresAt"`
}

// linkCodeResolve: POST /v1/link-codes/{code} (no auth; a token in token
// mode, as for a put). The first resolve uses the code up; an unknown,
// expired or used code is 404 code_unknown. Every resolve, found or not,
// spends the client's budget (10 a minute, 160 per IPv6 /48).
func (s *Server) linkCodeResolve(w http.ResponseWriter, r *http.Request) {
	h := s.mbox
	code := r.PathValue("code")
	if !validHex16(code) {
		h.bad(w, "code must be 32 lowercase hex characters (the lookup id, never the short code)")
		return
	}
	if !h.putsOpen {
		h.problem(w, http.StatusUnauthorized, mbCodeTokenRequired, "mailbox puts need an access token, and this relay has no access gate", 0, nil)
		return
	}
	if h.mode == mailboxesToken && !s.mailboxTokenOK(w, r) {
		return
	}
	if ok, wait := s.codes.resolves.allowWait(clientKey(r)); !ok {
		h.problem(w, http.StatusTooManyRequests, lcCodeAttempts, "too many code lookups from this client (10 a minute)", wait, nil)
		return
	}
	if k := slash48Key(r); k != "" {
		if ok, wait := s.codes.resolve48.allowWait(k); !ok {
			h.problem(w, http.StatusTooManyRequests, lcCodeAttempts, "too many code lookups from this network (IPv6 /48)", wait, nil)
			return
		}
	}
	c := s.codes
	c.mu.Lock()
	v := c.liveLocked(code, h.now())
	var out linkCodeResolved
	if v != nil && !v.used {
		// Single-use: a code read over a shoulder can't be resolved again.
		// The entry stays, used, until it expires, so the device that shows
		// it can tell "used" from "the relay restarted".
		v.used = true
		out = linkCodeResolved{Mailbox: v.mailbox, Blob: v.blob, ExpiresAt: ms(v.expiresAt)}
		v.blob = ""
	}
	c.mu.Unlock()
	if out.Mailbox == "" {
		h.problem(w, http.StatusNotFound, lcCodeUnknown, "no such code (mistyped, expired or already used)", 0, nil)
		return
	}
	mbWriteJSON(w, http.StatusOK, out)
}

// linkCodeCount is the number of codes held (live and used, unexpired).
func (c *linkCodes) count(now time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.codes {
		if now.Before(v.expiresAt) {
			n++
		}
	}
	return n
}
