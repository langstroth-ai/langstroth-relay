package relay

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Tests for the short link code routes (linkcodes.go).

func lcID(n int) string { return fmt.Sprintf("%032x", 0xc0de0000+n) }

func (e *mbEnv) codePath(code string) string { return "/v1/mailboxes/" + e.id + "/codes/" + code }

func lcBody(blob string, ttl int) []byte {
	m := map[string]any{"blob": blob}
	if ttl > 0 {
		m["ttlSecs"] = ttl
	}
	b, _ := json.Marshal(m)
	return b
}

func (e *mbEnv) postCode(code, blob string) mbResp {
	return e.member("PUT", e.codePath(code), lcBody(blob, 0))
}

func (e *mbEnv) resolve(code string) mbResp {
	return e.do("POST", "/v1/link-codes/"+code, nil, nil)
}

// newLCEnv is a registered mailbox with the resolve limits lifted.
func newLCEnv(t *testing.T) *mbEnv {
	e := newMBEnv(t, nil, 1, false)
	e.srv.codes.resolves = newIPRateLimiter(0, 0)
	e.srv.codes.resolve48 = newIPRateLimiter(0, 0)
	want(t, e.register(putCap(1), 0), 201, "")
	return e
}

func TestLinkCodePostResolveIsSingleUse(t *testing.T) {
	e := newLCEnv(t)
	r := e.postCode(lcID(1), "c2VhbGVkLWFkZHJlc3M")
	want(t, r, 201, "")
	if got := int64(r.body["expiresAt"].(float64)); got != e.clock.now().Add(10*time.Minute).UnixMilli() {
		t.Fatalf("expiresAt %d", got)
	}
	st := e.member("GET", e.codePath(lcID(1)), nil)
	want(t, st, 200, "")
	if st.body["used"] != false {
		t.Fatalf("fresh code reads used: %s", st.raw)
	}

	got := e.resolve(lcID(1))
	want(t, got, 200, "")
	// Content-blind: the relay hands back what it was given, nothing more.
	if got.body["mailbox"] != e.id || got.body["blob"] != "c2VhbGVkLWFkZHJlc3M" || len(got.body) != 3 {
		t.Fatalf("resolve: %s", got.raw)
	}
	if got.hdr.Get("Cache-Control") != "no-store" {
		t.Fatal("a resolve may be cached")
	}
	// Used: a second resolve is a plain miss, the shower sees "used", and
	// the same code can't be posted again.
	want(t, e.resolve(lcID(1)), 404, lcCodeUnknown)
	st = e.member("GET", e.codePath(lcID(1)), nil)
	want(t, st, 200, "")
	if st.body["used"] != true {
		t.Fatalf("used code reads unused: %s", st.raw)
	}
	want(t, e.postCode(lcID(1), "YWdhaW4"), 409, lcCodeUsed)
	// The used entry keeps no blob.
	e.srv.codes.mu.Lock()
	blob := e.srv.codes.codes[lcID(1)].blob
	e.srv.codes.mu.Unlock()
	if blob != "" {
		t.Fatal("a used code still holds its blob")
	}
}

func TestLinkCodeRepostReplacesAndKeepsExpiry(t *testing.T) {
	e := newLCEnv(t)
	want(t, e.postCode(lcID(1), "b25l"), 201, "")
	first := e.member("GET", e.codePath(lcID(1)), nil).body["expiresAt"]
	e.clock.add(3 * time.Minute)
	r := e.postCode(lcID(1), "dHdv")
	want(t, r, 200, "")
	if r.body["expiresAt"] != first {
		t.Fatalf("a re-post moved the expiry: %v -> %v", first, r.body["expiresAt"])
	}
	if got := e.resolve(lcID(1)); got.body["blob"] != "dHdv" {
		t.Fatalf("re-post didn't replace the blob: %s", got.raw)
	}
}

func TestLinkCodeNeedsMemberAuth(t *testing.T) {
	e := newLCEnv(t)
	body := lcBody("YWJj", 0)
	want(t, e.do("PUT", e.codePath(lcID(1)), body, nil), 401, mbCodeAuthRequired)
	stranger := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32))
	want(t, e.do("PUT", e.codePath(lcID(1)), body, e.memberHdr(stranger, "PUT", e.codePath(lcID(1)), body, 0)), 401, mbCodeBadMemberAuth)
	// The signature covers the body: a captured header can't post another blob.
	h := e.memberHdr(nil, "PUT", e.codePath(lcID(1)), body, 0)
	want(t, e.do("PUT", e.codePath(lcID(1)), lcBody("ZXZpbA", 0), h), 401, mbCodeBadMemberAuth)
	want(t, e.do("GET", e.codePath(lcID(1)), nil, nil), 401, mbCodeAuthRequired)
	want(t, e.do("DELETE", e.codePath(lcID(1)), nil, nil), 401, mbCodeAuthRequired)
	// An unregistered mailbox.
	o := rvBox(t, e, 7)
	want(t, o.postCode(lcID(1), "YWJj"), 404, mbCodeUnknown)
}

func TestLinkCodeBelongsToOneMailbox(t *testing.T) {
	e := newLCEnv(t)
	o := rvBox(t, e, 2)
	want(t, o.register(putCap(2), 0), 201, "")
	want(t, e.postCode(lcID(1), "bWluZQ"), 201, "")
	want(t, o.postCode(lcID(1), "dGhlaXJz"), 409, lcCodeTaken)
	want(t, o.member("GET", o.codePath(lcID(1)), nil), 404, lcCodeUnknown)
	want(t, o.member("DELETE", o.codePath(lcID(1)), nil), 204, "")
	if got := e.resolve(lcID(1)); got.body["blob"] != "bWluZQ" || got.body["mailbox"] != e.id {
		t.Fatalf("another mailbox changed the code: %s", got.raw)
	}
}

func TestLinkCodeDelete(t *testing.T) {
	e := newLCEnv(t)
	want(t, e.postCode(lcID(1), "YWJj"), 201, "")
	want(t, e.member("DELETE", e.codePath(lcID(1)), nil), 204, "")
	want(t, e.member("DELETE", e.codePath(lcID(1)), nil), 204, "")
	want(t, e.resolve(lcID(1)), 404, lcCodeUnknown)
	want(t, e.member("GET", e.codePath(lcID(1)), nil), 404, lcCodeUnknown)
}

func TestLinkCodeTTL(t *testing.T) {
	e := newLCEnv(t)
	for _, ttl := range []int{59, 601} {
		want(t, e.member("PUT", e.codePath(lcID(1)), lcBody("YWJj", ttl)), 400, mbCodeInvalid)
	}
	r := e.member("PUT", e.codePath(lcID(1)), lcBody("YWJj", 60))
	want(t, r, 201, "")
	want(t, e.postCode(lcID(2), "YWJj"), 201, "")
	e.clock.add(61 * time.Second)
	want(t, e.resolve(lcID(1)), 404, lcCodeUnknown)
	want(t, e.member("GET", e.codePath(lcID(1)), nil), 404, lcCodeUnknown)
	// An expired code is free again.
	want(t, e.postCode(lcID(1), "bmV3"), 201, "")
	e.clock.add(10 * time.Minute)
	want(t, e.resolve(lcID(2)), 404, lcCodeUnknown)
	// Used codes expire too.
	want(t, e.postCode(lcID(3), "YWJj"), 201, "")
	want(t, e.resolve(lcID(3)), 200, "")
	e.clock.add(10*time.Minute + time.Second)
	want(t, e.member("GET", e.codePath(lcID(3)), nil), 404, lcCodeUnknown)
	if n := e.srv.codes.count(e.clock.now()); n != 0 {
		t.Fatalf("%d codes held after expiry", n)
	}
}

func TestLinkCodeFourLivePerMailbox(t *testing.T) {
	e := newLCEnv(t)
	for i := 1; i <= linkCodeMaxPerBox; i++ {
		want(t, e.postCode(lcID(i), "YWJj"), 201, "")
	}
	r := e.postCode(lcID(9), "YWJj")
	want(t, r, 429, lcCodeFull)
	if r.hdr.Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	// A used code doesn't count; a deleted one doesn't either.
	want(t, e.resolve(lcID(1)), 200, "")
	want(t, e.postCode(lcID(9), "YWJj"), 201, "")
	want(t, e.member("DELETE", e.codePath(lcID(2)), nil), 204, "")
	want(t, e.postCode(lcID(10), "YWJj"), 201, "")
}

func TestLinkCodePostRatePerMailbox(t *testing.T) {
	e := newLCEnv(t)
	for i := 0; i < linkCodePostsPerMinute; i++ {
		want(t, e.postCode(lcID(1), "YWJj"), map[bool]int{true: 201, false: 200}[i == 0], "")
	}
	want(t, e.postCode(lcID(1), "YWJj"), 429, lcCodeRate)
}

func TestLinkCodeBodyLimits(t *testing.T) {
	e := newLCEnv(t)
	want(t, e.postCode(lcID(1), strings.Repeat("A", linkCodeMaxBlob+1)), 413, mbCodeTooLarge)
	want(t, e.postCode(lcID(1), "not base64!"), 400, mbCodeInvalid)
	want(t, e.postCode(lcID(1), ""), 400, mbCodeInvalid)
	want(t, e.postCode(lcID(1), strings.Repeat("A", linkCodeMaxBlob)), 201, "")
	// The path takes a lookup id, never a typed short code.
	for _, c := range []string{"K7Q4MX", "K7Q-4MX", strings.ToUpper(lcID(1)), lcID(1)[:31]} {
		want(t, e.member("PUT", "/v1/mailboxes/"+e.id+"/codes/"+c, lcBody("YWJj", 0)), 400, mbCodeInvalid)
		want(t, e.resolve(c), 400, mbCodeInvalid)
	}
}

// Every lookup spends the client's budget: 10 a minute, so guessing a live
// code out of 2^30 short codes takes years from one address.
func TestLinkCodeResolveBudgetPerClient(t *testing.T) {
	e := newMBEnv(t, nil, 1, false)
	want(t, e.register(putCap(1), 0), 201, "")
	want(t, e.postCode(lcID(1), "YWJj"), 201, "")
	h := e.srv.Handler()
	for i := 0; i < linkCodeResolvesPerMin; i++ {
		want(t, rvDo(t, h, nil, rvV4(1), "POST", "/v1/link-codes/"+lcID(100+i), nil, nil), 404, lcCodeUnknown)
	}
	r := rvDo(t, h, nil, rvV4(1), "POST", "/v1/link-codes/"+lcID(1), nil, nil)
	want(t, r, 429, lcCodeAttempts)
	if r.hdr.Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	// The refusal didn't use the code; another client still resolves it.
	want(t, rvDo(t, h, nil, rvV4(2), "POST", "/v1/link-codes/"+lcID(1), nil, nil), 200, "")
	// One IPv6 /48 gets 16 clients' worth, then stops.
	refused := false
	for i := 0; i < 20*linkCodeResolvesPerMin && !refused; i++ {
		r := rvDo(t, h, nil, rvV6(i), "POST", "/v1/link-codes/"+lcID(200+i), nil, nil)
		refused = r.status == 429
	}
	if !refused {
		t.Fatal("one /48 resolved without a limit")
	}
}

// A restart forgets codes (they live in memory): the status call answers
// code_unknown, the shower posts the same code again, and it resolves.
func TestLinkCodeRepostAfterRestart(t *testing.T) {
	e := newLCEnv(t)
	want(t, e.postCode(lcID(1), "YWJj"), 201, "")
	fresh := newLCEnv(t) // the same mailbox key on a relay that just started
	fresh.priv, fresh.id = e.priv, e.id
	want(t, fresh.member("GET", fresh.codePath(lcID(1)), nil), 404, lcCodeUnknown)
	want(t, fresh.postCode(lcID(1), "YWJj"), 201, "")
	want(t, fresh.resolve(lcID(1)), 200, "")
}

func TestLinkCodeTokenMode(t *testing.T) {
	t.Setenv("LANGSTROTH_RELAY_MAILBOXES", "token")
	e := newMBEnv(t, gatedPolicy("tok"), 1, false)
	e.token = "tok"
	want(t, e.register(putCap(1), 0), 201, "")
	want(t, e.postCode(lcID(1), "YWJj"), 201, "")
	want(t, e.resolve(lcID(1)), 401, mbCodeTokenRequired)
	want(t, e.do("POST", "/v1/link-codes/"+lcID(1), nil, map[string]string{"Authorization": "Bearer tok"}), 200, "")
}

// On a gated relay (not token mode) a resolve needs no token, like a put:
// the new device has none yet. Posting a code is a member call and does.
func TestLinkCodeGatedRelay(t *testing.T) {
	e := newMBEnv(t, gatedPolicy("tok"), 1, false)
	e.token = "tok"
	want(t, e.register(putCap(1), 0), 201, "")
	want(t, e.postCode(lcID(1), "YWJj"), 201, "")
	e.token = ""
	want(t, e.postCode(lcID(2), "YWJj"), 401, mbCodeTokenRequired)
	want(t, e.resolve(lcID(1)), 200, "")
}

// Off with the mailboxes: plain 404s, which the Ligus client reads as "this
// relay has no short codes" and falls back to the approval address.
func TestLinkCodeRoutesOffWithMailboxes(t *testing.T) {
	t.Setenv("LANGSTROTH_RELAY_MAILBOXES", "off")
	ts := httptest.NewServer(New(Options{Store: newMemoryStore(), Entitlement: entitlementPolicy{kind: entOpen}}).Handler())
	defer ts.Close()
	for _, c := range []struct{ m, p string }{
		{"PUT", "/v1/mailboxes/" + rid(1) + "/codes/" + lcID(1)}, {"GET", "/v1/mailboxes/" + rid(1) + "/codes/" + lcID(1)},
		{"DELETE", "/v1/mailboxes/" + rid(1) + "/codes/" + lcID(1)}, {"POST", "/v1/link-codes/" + lcID(1)},
	} {
		req, _ := http.NewRequest(c.m, ts.URL+c.p, strings.NewReader("{}"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 404 || strings.Contains(string(b), lcCodeUnknown) || strings.Contains(string(b), mbCodeUnknown) {
			t.Errorf("%s %s with mailboxes off: %d %q", c.m, c.p, resp.StatusCode, b)
		}
	}
}

func TestLinkCodeMetricsAndTemplates(t *testing.T) {
	e := newLCEnv(t)
	want(t, e.postCode(lcID(1), "YWJj"), 201, "")
	resp, err := http.Get(e.url + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, s := range []string{"hive_relay_link_codes 1\n", `hive_relay_mailbox_refusals_total{code="link_code_attempts"} 0`} {
		if !strings.Contains(string(b), s) {
			t.Errorf("/metrics lacks %q", s)
		}
	}
	for p, tmpl := range map[string]string{
		e.codePath(lcID(1)):         "/v1/mailboxes/{}/codes/{}",
		"/v1/link-codes/" + lcID(1): "/v1/link-codes/{}",
	} {
		if got := routeTemplate(p); got != tmpl {
			t.Errorf("routeTemplate(%s) = %s", p, got)
		}
	}
}

// The tunnel-only pod doesn't serve codes.
func TestLinkCodesNotOnTheTunnelPod(t *testing.T) {
	srv := New(Options{Store: newMemoryStore(), Entitlement: entitlementPolicy{kind: entOpen}})
	get := func(k string) string {
		if k == "LANGSTROTH_RELAY_ROUTES" {
			return "tunnel"
		}
		return ""
	}
	ts := httptest.NewServer(restrictRoutes(srv.Handler(), get))
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/link-codes/"+lcID(1), "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("tunnel pod answered %d", resp.StatusCode)
	}
}

// Anyone can register mailboxes, so one network can't fill the relay's
// code pool: codes posted from one IPv4 address or IPv6 /48 are capped, and
// another client still posts.
func TestLinkCodeOneNetworkCannotFillThePool(t *testing.T) {
	e := newMBEnv(t, nil, 1, false)
	h := e.srv.Handler()
	posted := 0
	for box := 0; box < 20 && posted <= linkCodeMaxPerOwner; box++ {
		o := rvBox(t, e, byte(10+box))
		want(t, rvDo(t, h, nil, rvV6(box), "PUT", "/v1/mailboxes/"+o.id, o.registerBody(nil, putCap(1), 0), nil), 201, "")
		for i := 0; i < linkCodeMaxPerBox; i++ {
			body := lcBody("YWJj", 0)
			p := o.codePath(lcID(box*10 + i))
			r := rvDo(t, h, nil, rvV6(box), "PUT", p, body, o.memberHdr(nil, "PUT", p, body, 0))
			if r.status == 429 {
				want(t, r, 429, lcCodeFull)
				if posted != linkCodeMaxPerOwner {
					t.Fatalf("refused after %d codes", posted)
				}
				goto other
			}
			want(t, r, 201, "")
			posted++
		}
	}
	t.Fatal("one /48 posted without a cap")
other:
	o := rvBox(t, e, 99)
	want(t, rvDo(t, h, nil, rvV4(1), "PUT", "/v1/mailboxes/"+o.id, o.registerBody(nil, putCap(1), 0), nil), 201, "")
	body := lcBody("YWJj", 0)
	p := o.codePath(lcID(999))
	want(t, rvDo(t, h, nil, rvV4(1), "PUT", p, body, o.memberHdr(nil, "PUT", p, body, 0)), 201, "")
}
