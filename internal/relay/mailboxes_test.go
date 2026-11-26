package relay

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Tests for /v1/mailboxes/* (mailboxes.go): the wire contract's auth and
// limits, and the preimages against ligus-core's golden vectors.

// ---- golden preimages (copied byte for byte from
// ligus-core/tests/golden/match_code_v1.jsonl, the "register" and "member" lines) ----

func TestMailboxPreimagesMatchLigusCoreGolden(t *testing.T) {
	f, err := os.Open("testdata/mailbox_preimages_v1.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var v struct {
			T          string `json:"t"`
			Label      string `json:"label"`
			Mailbox    string `json:"mailbox"`
			PutCapHash string `json:"putCapHash"`
			Counter    uint32 `json:"counter"`
			Method     string `json:"method"`
			Path       string `json:"path"`
			TS         int64  `json:"ts"`
			Body       string `json:"body"`
			Preimage   string `json:"preimage"`
		}
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		var got []byte
		switch v.T {
		case "register":
			var mb [16]byte
			var ch [32]byte
			mbHex(t, mb[:], v.Mailbox)
			mbHex(t, ch[:], v.PutCapHash)
			got = mailboxRegisterPreimage(mb, ch, v.Counter)
		case "member":
			got = mailboxMemberPreimage(v.Method, v.Path, v.TS, []byte(v.Body))
		default:
			t.Fatalf("unexpected vector type %q", v.T)
		}
		if hex.EncodeToString(got) != v.Preimage {
			t.Errorf("%s %s:\n got %x\nwant %s", v.T, v.Label, got, v.Preimage)
		}
		seen[v.T] = true
	}
	if !seen["register"] || !seen["member"] {
		t.Fatalf("vectors missing: %v", seen)
	}
}

func mbHex(t *testing.T, dst []byte, s string) {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(dst) {
		t.Fatalf("bad hex %q", s)
	}
	copy(dst, b)
}

// ---- harness ----

type mbClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *mbClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *mbClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type mbEnv struct {
	t     *testing.T
	srv   *Server
	url   string
	clock *mbClock
	priv  ed25519.PrivateKey
	id    string
	token string // sent as the bearer on member calls when set
}

var b64 = base64.RawURLEncoding

// newMBEnv starts a relay with ent (nil: open) and a fixed mailbox auth key
// derived from seed byte k. Put rates are lifted unless keepPutRate.
func newMBEnv(t *testing.T, ent EntitlementVerifier, k byte, keepPutRate bool) *mbEnv {
	t.Helper()
	if ent == nil {
		ent = entitlementPolicy{kind: entOpen}
	}
	srv := New(Options{Store: newMemoryStore(), Entitlement: ent})
	if srv.mbox == nil {
		t.Fatal("mailboxes are off")
	}
	clock := &mbClock{t: time.UnixMilli(1764000000000)}
	srv.mbox.now = clock.now
	if !keepPutRate {
		srv.mbox.putLimiter = newIPRateLimiter(0, 0)
		srv.mbox.putLimiter48 = newIPRateLimiter(0, 0)
		srv.mbox.putLimitBox = newIPRateLimiter(0, 0)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{k}, 32))
	return &mbEnv{t: t, srv: srv, url: ts.URL, clock: clock, priv: priv, id: mailboxIDOf(priv.Public().(ed25519.PublicKey))}
}

func putCap(n byte) []byte { return bytes.Repeat([]byte{n}, 16) }

func capHash(c []byte) []byte { s := sha256.Sum256(c); return s[:] }

type mbResp struct {
	status int
	hdr    http.Header
	body   map[string]any
	raw    string
}

func (r mbResp) code() string { s, _ := r.body["code"].(string); return s }

func (e *mbEnv) do(method, path string, body []byte, hdr map[string]string) mbResp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, e.url+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := mbResp{status: resp.StatusCode, hdr: resp.Header, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

// registerBody signs a register for e's key (or signer, when given).
func (e *mbEnv) registerBody(signer ed25519.PrivateKey, cap []byte, counter uint32) []byte {
	if signer == nil {
		signer = e.priv
	}
	var mb [16]byte
	mbHex(e.t, mb[:], e.id)
	var ch [32]byte
	copy(ch[:], capHash(cap))
	sig := ed25519.Sign(signer, mailboxRegisterPreimage(mb, ch, counter))
	b, _ := json.Marshal(map[string]any{
		"authPub": b64.EncodeToString(e.priv.Public().(ed25519.PublicKey)), "putCapHash": b64.EncodeToString(ch[:]),
		"counter": counter, "sig": b64.EncodeToString(sig),
	})
	return b
}

func (e *mbEnv) register(cap []byte, counter uint32) mbResp {
	return e.do("PUT", "/v1/mailboxes/"+e.id, e.registerBody(nil, cap, counter), e.tokenHdr(nil))
}

func (e *mbEnv) tokenHdr(h map[string]string) map[string]string {
	if h == nil {
		h = map[string]string{}
	}
	if e.token != "" {
		h["Authorization"] = "Bearer " + e.token
	}
	return h
}

// memberHdr signs a member call at the relay's clock plus skew.
func (e *mbEnv) memberHdr(signer ed25519.PrivateKey, method, path string, body []byte, skew time.Duration) map[string]string {
	if signer == nil {
		signer = e.priv
	}
	ts := e.clock.now().Add(skew).UnixMilli()
	sig := ed25519.Sign(signer, mailboxMemberPreimage(method, path, ts, body))
	return e.tokenHdr(map[string]string{mailboxAuthHeader: strconv.FormatInt(ts, 10) + "." + b64.EncodeToString(sig)})
}

// member calls path as a member; the signature covers the path without its query.
func (e *mbEnv) member(method, path string, body []byte) mbResp {
	signed, _, _ := strings.Cut(path, "?")
	return e.do(method, path, body, e.memberHdr(nil, method, signed, body, 0))
}

func rid(n int) string { return fmt.Sprintf("%032x", n) }

func readKey(n int) []byte { return bytes.Repeat([]byte{byte(n), 0xA5}, 8) }

func (e *mbEnv) reqPath(r string) string { return "/v1/mailboxes/" + e.id + "/requests/" + r }

func (e *mbEnv) put(cap []byte, r string, rk []byte, extra map[string]string) mbResp {
	body, _ := json.Marshal(map[string]string{"blob": "c2VhbGVk", "readKeyHash": b64.EncodeToString(capHash(rk))})
	h := map[string]string{}
	if cap != nil {
		h[mailboxPutCapHeader] = b64.EncodeToString(cap)
	}
	for k, v := range extra {
		h[k] = v
	}
	return e.do("PUT", e.reqPath(r), body, h)
}

func readHdr(rk []byte) map[string]string {
	return map[string]string{mailboxReadKeyHeader: b64.EncodeToString(rk)}
}

var msgSeq atomic.Int64

// msgBody is a message with a blob of its own: honest blobs are sealed to a
// fresh key each time, and the relay refuses the same blob twice on a rid.
func msgBody(to string) []byte {
	b, _ := json.Marshal(map[string]string{"blob": "bXNn" + strconv.FormatInt(msgSeq.Add(1), 10), "to": to})
	return b
}

func want(t *testing.T, r mbResp, status int, code string) {
	t.Helper()
	if r.status != status || (code != "" && r.code() != code) {
		t.Fatalf("got %d %q, want %d %q: %s", r.status, r.code(), status, code, r.raw)
	}
}

func gatedPolicy(tok string) EntitlementVerifier {
	return entitlementPolicy{kind: entTokens, tokenHashes: []string{sha256hex(tok)}}
}

// ---- register and reset ----

func TestMailboxRegisterCounterRules(t *testing.T) {
	e := newMBEnv(t, nil, 1, false)
	want(t, e.do("GET", "/v1/mailboxes/"+e.id, nil, nil), 404, mbCodeUnknown)

	r := e.register(putCap(1), 3)
	want(t, r, 201, "")
	if r.body["mailbox"] != e.id || r.body["counter"] != float64(3) {
		t.Fatalf("register body: %s", r.raw)
	}
	want(t, e.register(putCap(1), 3), 200, "") // same counter, same hash: idempotent
	r = e.register(putCap(2), 3)               // same counter, another hash
	want(t, r, 409, mbCodeStale)
	if r.body["counter"] != float64(3) {
		t.Fatalf("stale_counter should carry the current counter: %s", r.raw)
	}
	want(t, e.register(putCap(0), 2), 409, mbCodeStale) // lower
	want(t, e.register(putCap(5), 5), 409, mbCodeStale) // a jump past the next counter
	want(t, e.register(putCap(4), 4), 200, "")          // the next counter: reset
	r = e.do("GET", "/v1/mailboxes/"+e.id, nil, nil)
	want(t, r, 200, "")
	if r.body["counter"] != float64(4) || r.body["mailbox"] != e.id {
		t.Fatalf("GET mailbox: %s", r.raw)
	}
}

func TestMailboxRegisterAndResetNeedTheMailboxAuthKey(t *testing.T) {
	e := newMBEnv(t, nil, 1, false)
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32))

	// Signed by another key, for this mailbox's authPub: refused.
	want(t, e.do("PUT", "/v1/mailboxes/"+e.id, e.registerBody(other, putCap(1), 0), nil), 403, mbCodeBadSignature)
	// Another key's authPub, signed by it, claiming this mailbox id: refused.
	o := &mbEnv{t: t, priv: other, id: e.id}
	want(t, e.do("PUT", "/v1/mailboxes/"+e.id, o.registerBody(nil, putCap(1), 0), nil), 400, mbCodeMismatch)
	// A tampered counter: the signature no longer verifies.
	var body map[string]any
	_ = json.Unmarshal(e.registerBody(nil, putCap(1), 0), &body)
	body["counter"] = 1
	tampered, _ := json.Marshal(body)
	want(t, e.do("PUT", "/v1/mailboxes/"+e.id, tampered, nil), 403, mbCodeBadSignature)

	want(t, e.register(putCap(1), 0), 201, "")
	// A reset signed by another key is refused, and the cap is unchanged.
	want(t, e.do("PUT", "/v1/mailboxes/"+e.id, e.registerBody(other, putCap(2), 1), nil), 403, mbCodeBadSignature)
	want(t, e.put(putCap(1), rid(1), readKey(1), nil), 201, "")
	want(t, e.put(putCap(2), rid(2), readKey(2), nil), 403, mbCodeBadPutCap)

	// Malformed bodies.
	want(t, e.do("PUT", "/v1/mailboxes/"+e.id, []byte(`{"authPub":"x"}`), nil), 400, mbCodeInvalid)
	want(t, e.do("PUT", "/v1/mailboxes/"+e.id, []byte(`not json`), nil), 400, mbCodeInvalid)
	_ = json.Unmarshal(e.registerBody(nil, putCap(1), 0), &body)
	body["counter"] = 4294967296
	big, _ := json.Marshal(body)
	want(t, e.do("PUT", "/v1/mailboxes/"+e.id, big, nil), 400, mbCodeInvalid)
	want(t, e.do("PUT", "/v1/mailboxes/"+strings.ToUpper(e.id), e.registerBody(nil, putCap(1), 0), nil), 400, mbCodeInvalid)
}

// The relay's register check uses the same key derivation as ligus-core: an
// Ed25519 seed from the golden "mailbox" vector registers its own mailbox.
func TestMailboxRegisterWithGoldenAuthSeed(t *testing.T) {
	seed, _ := hex.DecodeString("a39c0d57dd0730d437be7a5168e67910ce2e50a0e403412835f54593ef004761")
	e := newMBEnv(t, nil, 1, false)
	e.priv = ed25519.NewKeyFromSeed(seed)
	e.id = mailboxIDOf(e.priv.Public().(ed25519.PublicKey))
	cap0, _ := hex.DecodeString("f6c54f7808b06f1e109997f63684b7f7")
	want(t, e.register(cap0, 0), 201, "")
	want(t, e.put(cap0, rid(1), readKey(1), nil), 201, "")
}

// ---- puts ----

func TestMailboxPutsNeedPutCap(t *testing.T) {
	e := newMBEnv(t, nil, 1, false)
	want(t, e.put(putCap(1), rid(1), readKey(1), nil), 404, mbCodeUnknown)
	want(t, e.register(putCap(1), 0), 201, "")
	want(t, e.put(nil, rid(1), readKey(1), nil), 403, mbCodeBadPutCap)
	want(t, e.put(putCap(7), rid(1), readKey(1), nil), 403, mbCodeBadPutCap)
	want(t, e.put([]byte("short"), rid(1), readKey(1), nil), 403, mbCodeBadPutCap)
	r := e.put(putCap(1), rid(1), readKey(1), nil)
	want(t, r, 201, "")
	if r.body["rid"] != rid(1) || r.body["putAt"] != float64(1764000000000) || r.body["expiresAt"] != float64(1764000000000+15*60*1000) {
		t.Fatalf("put body: %s", r.raw)
	}
}

func TestMailboxResetAddressInvalidatesTheOldCap(t *testing.T) {
	e := newMBEnv(t, nil, 1, false)
	want(t, e.register(putCap(1), 0), 201, "")
	want(t, e.put(putCap(1), rid(1), readKey(1), nil), 201, "")
	want(t, e.register(putCap(2), 1), 200, "") // Reset address
	want(t, e.put(putCap(1), rid(2), readKey(2), nil), 403, mbCodeBadPutCap)
	want(t, e.put(putCap(2), rid(3), readKey(3), nil), 201, "")
	// An old signed register can't roll the reset back.
	want(t, e.register(putCap(1), 0), 409, mbCodeStale)
}

func TestMailboxNoOverwriteAndNoReput(t *testing.T) {
	e := newMBEnv(t, nil, 1, false)
	want(t, e.register(putCap(1), 0), 201, "")
	want(t, e.put(putCap(1), rid(1), readKey(1), nil), 201, "")
	want(t, e.put(putCap(1), rid(1), readKey(9), nil), 409, mbCodeRidExists) // no overwrite

	// Deleted: can't be put again until after its expiry would have passed.
	want(t, e.do("DELETE", e.reqPath(rid(1)), nil, readHdr(readKey(1))), 204, "")
	want(t, e.put(putCap(1), rid(1), readKey(1), nil), 409, mbCodeRidExists)
	e.clock.add(15 * time.Minute)
	want(t, e.put(putCap(1), rid(1), readKey(1), nil), 409, mbCodeRidExists)
	e.clock.add(15*time.Minute + time.Second)
	want(t, e.put(putCap(1), rid(1), readKey(1), nil), 201, "")

	// Expired: the same.
	want(t, e.put(putCap(1), rid(2), readKey(2), nil), 201, "")
	e.clock.add(15 * time.Minute)
	want(t, e.put(putCap(1), rid(2), readKey(2), nil), 409, mbCodeRidExists)
}

func TestMailbox65thRequestRefused(t *testing.T) {
	e := newMBEnv(t, nil, 1, false)
	want(t, e.register(putCap(1), 0), 201, "")
	for i := 1; i <= 64; i++ {
		want(t, e.put(putCap(1), rid(i), readKey(i), nil), 201, "")
		e.clock.add(time.Second)
	}
	r := e.put(putCap(1), rid(65), readKey(65), nil)
	want(t, r, 429, mbCodeFull)
	ra, _ := strconv.Atoi(r.hdr.Get("Retry-After"))
	if ra < 1 || ra > 15*60 || r.body["error"] != mbCodeFull {
		t.Fatalf("mailbox_full refusal: Retry-After %q, %s", r.hdr.Get("Retry-After"), r.raw)
	}
	// The oldest expires: a slot opens.
	e.clock.add(15*time.Minute - 64*time.Second)
	want(t, e.put(putCap(1), rid(65), readKey(65), nil), 201, "")
	want(t, e.put(putCap(1), rid(66), readKey(66), nil), 429, mbCodeFull)
}

func TestMailboxPutRatePerClient(t *testing.T) {
	e := newMBEnv(t, nil, 1, true)
	want(t, e.register(putCap(1), 0), 201, "")
	for i := 1; i <= 10; i++ {
		want(t, e.put(putCap(1), rid(i), readKey(i), nil), 201, "")
	}
	r := e.put(putCap(1), rid(11), readKey(11), nil)
	want(t, r, 429, mbCodePutRate)
	if r.hdr.Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	// A put with no cap counts too, so guessing is rate limited.
	want(t, e.put(nil, rid(12), readKey(12), nil), 429, mbCodePutRate)
}

func TestMailboxBlobSizeLimits(t *testing.T) {
	e := newMBEnv(t, nil, 1, false)
	want(t, e.register(putCap(1), 0), 201, "")
	body, _ := json.Marshal(map[string]string{"blob": strings.Repeat("A", 8<<10+1), "readKeyHash": b64.EncodeToString(capHash(readKey(1)))})
	want(t, e.do("PUT", e.reqPath(rid(1)), body, map[string]string{mailboxPutCapHeader: b64.EncodeToString(putCap(1))}), 413, mbCodeTooLarge)
	body, _ = json.Marshal(map[string]string{"blob": strings.Repeat("A", 8<<10), "readKeyHash": b64.EncodeToString(capHash(readKey(1)))})
	want(t, e.do("PUT", e.reqPath(rid(1)), body, map[string]string{mailboxPutCapHeader: b64.EncodeToString(putCap(1))}), 201, "")
	body, _ = json.Marshal(map[string]string{"blob": "not base64url!", "readKeyHash": b64.EncodeToString(capHash(readKey(2)))})
	want(t, e.do("PUT", e.reqPath(rid(2)), body, map[string]string{mailboxPutCapHeader: b64.EncodeToString(putCap(1))}), 400, mbCodeInvalid)

	big, _ := json.Marshal(map[string]string{"blob": strings.Repeat("A", 32<<10+1), "to": "members"})
	want(t, e.do("POST", e.reqPath(rid(1))+"/msgs", big, readHdr(readKey(1))), 413, mbCodeTooLarge)
	ok, _ := json.Marshal(map[string]string{"blob": strings.Repeat("A", 32<<10), "to": "members"})
	want(t, e.do("POST", e.reqPath(rid(1))+"/msgs", ok, readHdr(readKey(1))), 201, "")
	want(t, e.do("POST", e.reqPath(rid(1))+"/msgs", msgBody("everyone"), readHdr(readKey(1))), 400, mbCodeInvalid)
}

// ---- messages, requester and member auth ----

func TestMailbox11thMessageRefused(t *testing.T) {
	e := newMBEnv(t, nil, 1, false)
	want(t, e.register(putCap(1), 0), 201, "")
	want(t, e.put(putCap(1), rid(1), readKey(1), nil), 201, "")
	p := e.reqPath(rid(1)) + "/msgs"
	for i := 1; i <= 10; i++ {
		var r mbResp
		if i%2 == 0 {
			r = e.member("POST", p, msgBody("requester"))
		} else {
			r = e.do("POST", p, msgBody("members"), readHdr(readKey(1)))
		}
		want(t, r, 201, "")
		if r.body["seq"] != float64(i) {
			t.Fatalf("seq %v, want %d", r.body["seq"], i)
		}
	}
	want(t, e.member("POST", p, msgBody("requester")), 429, mbCodeTooManyMsgs)
	want(t, e.do("POST", p, msgBody("members"), readHdr(readKey(1))), 429, mbCodeTooManyMsgs)

	r := e.do("GET", p+"?after=8", nil, readHdr(readKey(1)))
	want(t, r, 200, "")
	msgs := r.body["msgs"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("after=8: %s", r.raw)
	}
	m := msgs[0].(map[string]any)
	if m["seq"] != float64(9) || m["from"] != "requester" || m["to"] != "members" || !strings.HasPrefix(m["blob"].(string), "bXNn") || m["at"] != float64(1764000000000) {
		t.Fatalf("message: %v", m)
	}
	if msgs[1].(map[string]any)["from"] != "member" {
		t.Fatalf("member message: %v", msgs[1])
	}
	lr := e.member("GET", "/v1/mailboxes/"+e.id+"/requests", nil)
	want(t, lr, 200, "")
	if q := lr.body["requests"].([]any)[0].(map[string]any); q["msgCount"] != float64(10) || q["rid"] != rid(1) || q["blob"] != "c2VhbGVk" {
		t.Fatalf("list: %s", lr.raw)
	}
}

// A requester with its read key and no token reads only its own rid, on a
// token-gated relay too; members there need their token as well.
func TestMailboxRequesterReadsOnlyItsOwnRid(t *testing.T) {
	e := newMBEnv(t, gatedPolicy("tok"), 1, false)
	e.token = "tok"
	want(t, e.register(putCap(1), 0), 201, "")
	// Puts and requester calls carry no token.
	want(t, e.put(putCap(1), rid(1), readKey(1), nil), 201, "")
	want(t, e.put(putCap(1), rid(2), readKey(2), nil), 201, "")
	want(t, e.member("POST", e.reqPath(rid(2))+"/msgs", msgBody("requester")), 201, "")

	want(t, e.do("GET", e.reqPath(rid(1))+"/msgs", nil, readHdr(readKey(1))), 200, "")
	want(t, e.do("GET", e.reqPath(rid(2))+"/msgs", nil, readHdr(readKey(1))), 403, mbCodeBadReadKey)
	want(t, e.do("POST", e.reqPath(rid(2))+"/msgs", msgBody("members"), readHdr(readKey(1))), 403, mbCodeBadReadKey)
	want(t, e.do("DELETE", e.reqPath(rid(2)), nil, readHdr(readKey(1))), 403, mbCodeBadReadKey)
	want(t, e.do("GET", e.reqPath(rid(2))+"/msgs", nil, nil), 401, mbCodeAuthRequired)
	// A read key doesn't list the mailbox.
	want(t, e.do("GET", "/v1/mailboxes/"+e.id+"/requests", nil, readHdr(readKey(1))), 401, mbCodeAuthRequired)
	// The mailbox GET needs no auth.
	want(t, e.do("GET", "/v1/mailboxes/"+e.id, nil, nil), 200, "")

	// Members: signature and token.
	want(t, e.member("GET", "/v1/mailboxes/"+e.id+"/requests", nil), 200, "")
	e.token = ""
	want(t, e.member("GET", "/v1/mailboxes/"+e.id+"/requests", nil), 401, mbCodeTokenRequired)
	want(t, e.register(putCap(1), 0), 401, mbCodeTokenRequired)
	e.token = "wrong"
	want(t, e.member("GET", e.reqPath(rid(2))+"/msgs", nil), 401, mbCodeTokenRequired)
}

func TestMailboxMemberAuthWindowAndSignature(t *testing.T) {
	e := newMBEnv(t, nil, 1, false)
	want(t, e.register(putCap(1), 0), 201, "")
	want(t, e.put(putCap(1), rid(1), readKey(1), nil), 201, "")
	list := "/v1/mailboxes/" + e.id + "/requests"
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32))

	want(t, e.do("GET", list, nil, e.memberHdr(nil, "GET", list, nil, -299*time.Second)), 200, "")
	want(t, e.do("GET", list, nil, e.memberHdr(nil, "GET", list, nil, 299*time.Second)), 200, "")
	want(t, e.do("GET", list, nil, e.memberHdr(nil, "GET", list, nil, -301*time.Second)), 401, mbCodeBadMemberAuth)
	want(t, e.do("GET", list, nil, e.memberHdr(nil, "GET", list, nil, 301*time.Second)), 401, mbCodeBadMemberAuth)
	want(t, e.do("GET", list, nil, e.memberHdr(other, "GET", list, nil, 0)), 401, mbCodeBadMemberAuth)
	want(t, e.do("GET", list, nil, nil), 401, mbCodeAuthRequired)
	want(t, e.do("GET", list, nil, map[string]string{mailboxAuthHeader: "garbage"}), 401, mbCodeBadMemberAuth)
	// Signed for another path, method or body.
	want(t, e.do("GET", list, nil, e.memberHdr(nil, "GET", e.reqPath(rid(1))+"/msgs", nil, 0)), 401, mbCodeBadMemberAuth)
	p := e.reqPath(rid(1)) + "/msgs"
	want(t, e.do("POST", p, msgBody("requester"), e.memberHdr(nil, "POST", p, msgBody("approver"), 0)), 401, mbCodeBadMemberAuth)
	want(t, e.do("POST", p, msgBody("requester"), e.memberHdr(nil, "PUT", p, msgBody("requester"), 0)), 401, mbCodeBadMemberAuth)
	// The query string isn't signed.
	want(t, e.do("GET", p+"?after=0", nil, e.memberHdr(nil, "GET", p, nil, 0)), 200, "")
	// A member can't delete a request: that is the requester's.
	want(t, e.do("DELETE", e.reqPath(rid(1)), nil, e.memberHdr(nil, "DELETE", e.reqPath(rid(1)), nil, 0)), 401, mbCodeAuthRequired)
	// Unknown mailbox.
	o := &mbEnv{t: t, priv: other, id: mailboxIDOf(other.Public().(ed25519.PublicKey)), clock: e.clock}
	ol := "/v1/mailboxes/" + o.id + "/requests"
	want(t, e.do("GET", ol, nil, o.memberHdr(nil, "GET", ol, nil, 0)), 404, mbCodeUnknown)
}

func TestMailboxDeleteOnSettle(t *testing.T) {
	e := newMBEnv(t, nil, 1, false)
	want(t, e.register(putCap(1), 0), 201, "")
	want(t, e.put(putCap(1), rid(1), readKey(1), nil), 201, "")
	p := e.reqPath(rid(1)) + "/msgs"
	want(t, e.member("POST", p, msgBody("requester")), 201, "") // the result
	want(t, e.do("DELETE", e.reqPath(rid(1)), nil, nil), 401, mbCodeAuthRequired)
	want(t, e.do("DELETE", e.reqPath(rid(1)), nil, readHdr(readKey(2))), 403, mbCodeBadReadKey)
	r := e.do("DELETE", e.reqPath(rid(1)), nil, readHdr(readKey(1)))
	want(t, r, 204, "")
	want(t, e.do("GET", p, nil, readHdr(readKey(1))), 404, mbCodeRequestUnknown)
	want(t, e.member("GET", p, nil), 404, mbCodeRequestUnknown)
	want(t, e.member("POST", p, msgBody("requester")), 404, mbCodeRequestUnknown)
	want(t, e.do("DELETE", e.reqPath(rid(1)), nil, readHdr(readKey(1))), 404, mbCodeRequestUnknown)
	lr := e.member("GET", "/v1/mailboxes/"+e.id+"/requests", nil)
	if n := len(lr.body["requests"].([]any)); n != 0 {
		t.Fatalf("deleted request still listed: %s", lr.raw)
	}
	e.srv.mbox.mu.Lock()
	bytesLeft, reqs := e.srv.mbox.bytes, e.srv.mbox.reqCount
	e.srv.mbox.mu.Unlock()
	if bytesLeft != 0 || reqs != 0 {
		t.Fatalf("delete left %d bytes, %d requests", bytesLeft, reqs)
	}
}

func TestMailboxExpiry(t *testing.T) {
	e := newMBEnv(t, nil, 1, false)
	want(t, e.register(putCap(1), 0), 201, "")
	want(t, e.put(putCap(1), rid(1), readKey(1), nil), 201, "")
	e.clock.add(10 * time.Minute)
	want(t, e.put(putCap(1), rid(2), readKey(2), nil), 201, "")
	p := e.reqPath(rid(1)) + "/msgs"
	want(t, e.do("POST", p, msgBody("members"), readHdr(readKey(1))), 201, "")
	e.clock.add(5*time.Minute - time.Millisecond)
	want(t, e.do("GET", p, nil, readHdr(readKey(1))), 200, "")
	e.clock.add(time.Millisecond) // 15 minutes after the put: gone, messages too
	want(t, e.do("GET", p, nil, readHdr(readKey(1))), 404, mbCodeRequestUnknown)
	want(t, e.do("POST", p, msgBody("members"), readHdr(readKey(1))), 404, mbCodeRequestUnknown)
	lr := e.member("GET", "/v1/mailboxes/"+e.id+"/requests", nil)
	reqs := lr.body["requests"].([]any)
	if len(reqs) != 1 || reqs[0].(map[string]any)["rid"] != rid(2) {
		t.Fatalf("after expiry: %s", lr.raw)
	}
	e.clock.add(10 * time.Minute)
	e.srv.mbox.sweep() // the background sweep frees the bytes without any request
	e.srv.mbox.mu.Lock()
	defer e.srv.mbox.mu.Unlock()
	if e.srv.mbox.bytes != 0 || e.srv.mbox.reqCount != 0 {
		t.Fatalf("sweep left %d bytes, %d requests", e.srv.mbox.bytes, e.srv.mbox.reqCount)
	}
}

func TestMailboxLongPollWakesOnMessageAndDelete(t *testing.T) {
	e := newMBEnv(t, nil, 1, false)
	want(t, e.register(putCap(1), 0), 201, "")
	want(t, e.put(putCap(1), rid(1), readKey(1), nil), 201, "")
	p := e.reqPath(rid(1)) + "/msgs"

	done := make(chan mbResp, 1)
	start := time.Now()
	go func() { done <- e.do("GET", p+"?after=0&wait=20", nil, readHdr(readKey(1))) }()
	time.Sleep(200 * time.Millisecond)
	want(t, e.member("POST", p, msgBody("requester")), 201, "")
	select {
	case r := <-done:
		want(t, r, 200, "")
		if len(r.body["msgs"].([]any)) != 1 || time.Since(start) > 10*time.Second {
			t.Fatalf("long poll: %s after %s", r.raw, time.Since(start))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("long poll did not wake on a message")
	}

	// Nothing after 1: a short wait answers empty.
	r := e.do("GET", p+"?after=1&wait=1", nil, readHdr(readKey(1)))
	want(t, r, 200, "")
	if len(r.body["msgs"].([]any)) != 0 {
		t.Fatalf("want no messages: %s", r.raw)
	}

	go func() { done <- e.member("GET", p+"?after=1&wait=20", nil) }()
	time.Sleep(200 * time.Millisecond)
	want(t, e.do("DELETE", e.reqPath(rid(1)), nil, readHdr(readKey(1))), 204, "")
	select {
	case r := <-done:
		want(t, r, 404, mbCodeRequestUnknown)
	case <-time.After(15 * time.Second):
		t.Fatal("long poll did not wake on delete")
	}
	want(t, e.do("GET", p+"?wait=x", nil, readHdr(readKey(1))), 400, mbCodeInvalid)
}

// ---- token mode, off mode, config ----

func TestMailboxTokenModePutsNeedAToken(t *testing.T) {
	t.Setenv("LANGSTROTH_RELAY_MAILBOXES", "token")
	e := newMBEnv(t, gatedPolicy("tok"), 1, false)
	e.token = "tok"
	want(t, e.register(putCap(1), 0), 201, "")
	want(t, e.put(putCap(1), rid(1), readKey(1), nil), 401, mbCodeTokenRequired)
	want(t, e.put(putCap(1), rid(1), readKey(1), map[string]string{"Authorization": "Bearer wrong"}), 401, mbCodeTokenRequired)
	want(t, e.put(nil, rid(1), readKey(1), map[string]string{"Authorization": "Bearer tok"}), 403, mbCodeBadPutCap)
	want(t, e.put(putCap(1), rid(1), readKey(1), map[string]string{"Authorization": "Bearer tok"}), 201, "")
	// The requester still reads with its read key alone.
	want(t, e.do("GET", e.reqPath(rid(1))+"/msgs", nil, readHdr(readKey(1))), 200, "")
}

func TestMailboxTokenModeOnAnOpenRelayRefusesPuts(t *testing.T) {
	t.Setenv("LANGSTROTH_RELAY_MAILBOXES", "token")
	e := newMBEnv(t, nil, 1, false)
	want(t, e.register(putCap(1), 0), 201, "")
	want(t, e.put(putCap(1), rid(1), readKey(1), map[string]string{"Authorization": "Bearer anything"}), 401, mbCodeTokenRequired)
}

func TestMailboxModeOffAnswers404Everywhere(t *testing.T) {
	t.Setenv("LANGSTROTH_RELAY_MAILBOXES", "off")
	for _, ent := range []EntitlementVerifier{entitlementPolicy{kind: entOpen}, gatedPolicy("tok")} {
		srv := New(Options{Store: newMemoryStore(), Entitlement: ent})
		if srv.mbox != nil {
			t.Fatal("mailboxes on with LANGSTROTH_RELAY_MAILBOXES=off")
		}
		ts := httptest.NewServer(srv.Handler())
		id := rid(1)
		for _, c := range []struct{ m, p string }{
			{"GET", "/v1/mailboxes/" + id}, {"PUT", "/v1/mailboxes/" + id},
			{"GET", "/v1/mailboxes/" + id + "/requests"}, {"PUT", "/v1/mailboxes/" + id + "/requests/" + id},
			{"DELETE", "/v1/mailboxes/" + id + "/requests/" + id},
			{"POST", "/v1/mailboxes/" + id + "/requests/" + id + "/msgs"}, {"GET", "/v1/mailboxes/" + id + "/requests/" + id + "/msgs"},
		} {
			req, _ := http.NewRequest(c.m, ts.URL+c.p, strings.NewReader("{}"))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 404 || strings.Contains(string(b), mbCodeUnknown) {
				t.Errorf("%s %s with mailboxes off: %d %q", c.m, c.p, resp.StatusCode, b)
			}
		}
		ts.Close()
	}
}

func TestMailboxesConfig(t *testing.T) {
	for _, c := range []struct {
		mode, pubkey string
		ok           bool
	}{
		{"", "", true}, {"on", "", true}, {"OFF", "", true}, {"token", "", false}, {"yes", "", false}, {"1", "", false},
		{"token", strings.Repeat("ab", 32), true},
	} {
		t.Run(c.mode+"/"+strconv.FormatBool(c.pubkey != ""), func(t *testing.T) {
			t.Setenv("LANGSTROTH_RELAY_MAILBOXES", c.mode)
			t.Setenv("LANGSTROTH_RELAY_TOKEN_PUBKEY", c.pubkey)
			if err := mailboxesConfigError(); (err == nil) != c.ok {
				t.Fatalf("mailboxesConfigError() = %v, want ok=%v", err, c.ok)
			}
		})
	}
	// An unknown value in library use: the routes are off, not on.
	t.Setenv("LANGSTROTH_RELAY_MAILBOXES", "maybe")
	if srv := New(Options{Store: newMemoryStore()}); srv.mbox != nil {
		t.Fatal("an unparseable mode turned mailboxes on")
	}
}

// ---- registration caps ----

func TestMailboxRegistrationCaps(t *testing.T) {
	t.Setenv("LANGSTROTH_RELAY_MAX_MAILBOXES_PER_CLIENT", "2")
	e := newMBEnv(t, nil, 1, false)
	regAs := func(k byte) mbResp {
		o := &mbEnv{t: t, url: e.url, priv: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{k}, 32))}
		o.id = mailboxIDOf(o.priv.Public().(ed25519.PublicKey))
		return o.register(putCap(1), 0)
	}
	want(t, regAs(1), 201, "")
	want(t, regAs(2), 201, "")
	want(t, regAs(3), 429, mbCodeRegister)
	want(t, regAs(1), 200, "") // re-registering an existing one is fine

	t.Setenv("LANGSTROTH_RELAY_MAX_MAILBOXES_PER_CLIENT", "0")
	t.Setenv("LANGSTROTH_RELAY_MAX_MAILBOXES", "1")
	e2 := newMBEnv(t, nil, 1, false)
	want(t, e2.register(putCap(1), 0), 201, "")
	o := &mbEnv{t: t, url: e2.url, priv: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{5}, 32))}
	o.id = mailboxIDOf(o.priv.Public().(ed25519.PublicKey))
	want(t, o.register(putCap(1), 0), 507, mbCodeMailboxesFull)

	// An idle mailbox (no member call for 30 days, no live request) is forgotten.
	e2.clock.add(mailboxIdleTTL + time.Minute)
	e2.srv.mbox.sweep()
	want(t, o.register(putCap(1), 0), 201, "")
}

func TestMailboxStorageCap(t *testing.T) {
	t.Setenv("LANGSTROTH_RELAY_MAILBOX_MAX_BYTES", "20")
	e := newMBEnv(t, nil, 1, false)
	want(t, e.register(putCap(1), 0), 201, "")
	want(t, e.put(putCap(1), rid(1), readKey(1), nil), 201, "")                                                              // 8 bytes
	want(t, e.do("POST", e.reqPath(rid(1))+"/msgs", []byte(`{"blob":"bXNn","to":"members"}`), readHdr(readKey(1))), 201, "") // 4 bytes
	want(t, e.put(putCap(1), rid(2), readKey(2), nil), 201, "")
	want(t, e.put(putCap(1), rid(3), readKey(3), nil), 507, mbCodeStorage)
}

// ---- wiring ----

func TestMailboxMetricsAndLogTemplates(t *testing.T) {
	e := newMBEnv(t, nil, 1, false)
	want(t, e.register(putCap(1), 0), 201, "")
	want(t, e.put(putCap(1), rid(1), readKey(1), nil), 201, "")
	resp, err := http.Get(e.url + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, s := range []string{"hive_relay_mailboxes 1\n", "hive_relay_mailbox_requests 1\n", "hive_relay_mailbox_bytes 8\n", `hive_relay_mailbox_refusals_total{code="mailbox_full"} 0`} {
		if !strings.Contains(string(b), s) {
			t.Errorf("/metrics lacks %q", s)
		}
	}
	for p, tmpl := range map[string]string{
		"/v1/mailboxes/" + e.id:                                   "/v1/mailboxes/{}",
		"/v1/mailboxes/" + e.id + "/requests":                     "/v1/mailboxes/{}/requests",
		"/v1/mailboxes/" + e.id + "/requests/" + rid(1):           "/v1/mailboxes/{}/requests/{}",
		"/v1/mailboxes/" + e.id + "/requests/" + rid(1) + "/msgs": "/v1/mailboxes/{}/requests/{}/msgs",
	} {
		if got := routeTemplate(p); got != tmpl {
			t.Errorf("routeTemplate(%s) = %s", p, got)
		}
	}
	if inflightPath("/v1/mailboxes/" + e.id + "/requests/" + rid(1) + "/msgs") {
		t.Error("long polls count against the in-flight cap")
	}
}

// The tunnel-only pod doesn't serve mailboxes.
func TestMailboxesNotOnTheTunnelPod(t *testing.T) {
	srv := New(Options{Store: newMemoryStore(), Entitlement: entitlementPolicy{kind: entOpen}})
	get := func(k string) string {
		if k == "LANGSTROTH_RELAY_ROUTES" {
			return "tunnel"
		}
		return ""
	}
	ts := httptest.NewServer(restrictRoutes(srv.Handler(), get))
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/v1/mailboxes/" + rid(1))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("tunnel pod answered %d", resp.StatusCode)
	}
}

// Mailboxes are additive: with them off every existing route answers exactly
// as recorded (with them on, TestExistingRoutesMatchGolden covers it).
func TestExistingRoutesMatchGoldenWithMailboxesOff(t *testing.T) {
	t.Setenv("LANGSTROTH_RELAY_GITHUB_ROUTES", "on")
	t.Setenv("LANGSTROTH_RELAY_MAILBOXES", "off")
	checkGolden(t, runGoldenRequests(t))
}

// ---- the client's reading of a 404 (Ligus 0.4 code-match wire note, 4) ----

// mbClass404 classifies a 404 the way ligus-runtime's approval_mailbox client
// does: a mailbox problem document's code ("error" first, then "code"), and
// "no_routes" for any 404 without one.
func mbClass404(t *testing.T, status int, raw string) string {
	t.Helper()
	if status != 404 {
		t.Fatalf("got %d, want 404: %s", status, raw)
	}
	var doc map[string]any
	if json.Unmarshal([]byte(raw), &doc) == nil {
		e, _ := doc["error"].(string)
		c, _ := doc["code"].(string)
		if e != c {
			t.Fatalf("problem document's error %q and code %q differ: %s", e, c, raw)
		}
		if e == mbCodeUnknown || e == mbCodeRequestUnknown {
			return e
		}
	}
	return "no_routes"
}

func TestMailbox404sReadAsTheLigusClientReadsThem(t *testing.T) {
	e := newMBEnv(t, gatedPolicy("tok"), 1, false)
	e.token = "tok"
	class := func(r mbResp) string { return mbClass404(t, r.status, r.raw) }

	// No mailbox yet: every route that names one answers mailbox_unknown.
	if c := class(e.do("GET", "/v1/mailboxes/"+e.id, nil, nil)); c != mbCodeUnknown {
		t.Fatalf("GET mailbox: %s", c)
	}
	if c := class(e.member("GET", "/v1/mailboxes/"+e.id+"/requests", nil)); c != mbCodeUnknown {
		t.Fatalf("list: %s", c)
	}
	if c := class(e.put(putCap(1), rid(1), readKey(1), nil)); c != mbCodeUnknown {
		t.Fatalf("put: %s", c)
	}

	// Registered: an unknown, deleted or expired rid is request_unknown,
	// for a member and for a requester.
	want(t, e.register(putCap(1), 0), 201, "")
	for _, r := range []mbResp{
		e.member("POST", e.reqPath(rid(9))+"/msgs", msgBody("members")),
		e.member("GET", e.reqPath(rid(9))+"/msgs", nil),
		e.do("GET", e.reqPath(rid(9))+"/msgs", nil, readHdr(readKey(9))),
		e.do("DELETE", e.reqPath(rid(9)), nil, readHdr(readKey(9))),
	} {
		if c := class(r); c != mbCodeRequestUnknown {
			t.Fatalf("unknown rid: %s (%s)", c, r.raw)
		}
	}
	want(t, e.put(putCap(1), rid(1), readKey(1), nil), 201, "")
	want(t, e.do("DELETE", e.reqPath(rid(1)), nil, readHdr(readKey(1))), 204, "")
	if c := class(e.do("GET", e.reqPath(rid(1))+"/msgs", nil, readHdr(readKey(1)))); c != mbCodeRequestUnknown {
		t.Fatalf("deleted rid: %s", c)
	}

	// Register needs the access token on a gated relay (401, not a 404).
	e.token = ""
	want(t, e.register(putCap(1), 0), 401, mbCodeTokenRequired)

	// Mailboxes off on a gated relay: plain 404s, no token asked for.
	t.Setenv("LANGSTROTH_RELAY_MAILBOXES", "off")
	off := httptest.NewServer(New(Options{Store: newMemoryStore(), Entitlement: gatedPolicy("tok")}).Handler())
	defer off.Close()
	for _, p := range []string{"/v1/mailboxes/" + e.id, "/v1/mailboxes/" + e.id + "/requests/" + rid(1) + "/msgs"} {
		resp, err := http.Get(off.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if c := mbClass404(t, resp.StatusCode, string(b)); c != "no_routes" {
			t.Fatalf("off %s: %s", p, c)
		}
	}
}
