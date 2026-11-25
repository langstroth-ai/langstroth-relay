package relay

// Regression tests from the security review of /v1/mailboxes. Each one was
// first written to show the flaw; the assertions are now the fixed behaviour.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// rvDo calls the handler directly as a client at remote (so tests can be many clients).
func rvDo(t *testing.T, h http.Handler, ctx context.Context, remote, method, path string, body []byte, hdr map[string]string) mbResp {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	req.RemoteAddr = remote
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := mbResp{status: rec.Code, hdr: rec.Header(), raw: rec.Body.String()}
	_ = json.Unmarshal(rec.Body.Bytes(), &out.body)
	return out
}

func rvV6(slash64 int) string { return fmt.Sprintf("[2001:db8:1:%x::1]:4000", slash64) }

func rvV4(n int) string { return fmt.Sprintf("10.%d.%d.1:4000", n/256, n%256) }

func rvBox(t *testing.T, e *mbEnv, seed byte) *mbEnv {
	o := &mbEnv{t: t, url: e.url, srv: e.srv, clock: e.clock, priv: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, 32))}
	o.id = mailboxIDOf(o.priv.Public().(ed25519.PublicKey))
	return o
}

// RL-1: a stranger registers its own mailboxes from one IPv6 /48 and fills
// them through its own read keys. Its blob bytes are charged to its /48
// (and capped per mailbox), so the relay-wide pool never fills and another
// workspace's put still succeeds. Puts are also rate limited per /48.
func TestReviewRL1_OneNetworkCannotFillTheBytePool(t *testing.T) {
	victim := newMBEnv(t, nil, 1, true) // real put rate limits
	h := victim.srv.Handler()
	want(t, rvDo(t, h, nil, "198.51.100.7:1", "PUT", "/v1/mailboxes/"+victim.id, victim.registerBody(nil, putCap(1), 0), nil), 201, "")

	reqBlob := strings.Repeat("A", mailboxMaxRequestBlob)
	client, refused := 0, map[int]int{}
	for box := byte(10); box < 20; box++ {
		a := rvBox(t, victim, box)
		want(t, rvDo(t, h, nil, rvV6(0), "PUT", "/v1/mailboxes/"+a.id, a.registerBody(nil, putCap(9), 0), nil), 201, "")
		for i := 0; i < mailboxMaxLive; i++ {
			client++ // one fresh /64 per request, all in one /48
			rk := readKey(i)
			body, _ := json.Marshal(map[string]string{"blob": reqBlob, "readKeyHash": b64.EncodeToString(capHash(rk))})
			r := rvDo(t, h, nil, rvV6(client), "PUT", a.reqPath(rid(i)), body, map[string]string{mailboxPutCapHeader: b64.EncodeToString(putCap(9))})
			if r.status != 201 {
				refused[r.status]++
				continue
			}
			for m := 0; m < mailboxMaxMsgs; m++ {
				mb, _ := json.Marshal(map[string]string{"blob": strings.Repeat("A", mailboxMaxMsgBlob-1) + string(rune('A'+m)), "to": "members"})
				if r := rvDo(t, h, nil, rvV6(client), "POST", a.reqPath(rid(i))+"/msgs", mb, readHdr(rk)); r.status != 201 {
					refused[r.status]++
					break
				}
			}
		}
	}
	if refused[429] == 0 && refused[507] == 0 {
		t.Fatal("one /48 was never refused")
	}
	victim.srv.mbox.mu.Lock()
	total := victim.srv.mbox.bytes
	victim.srv.mbox.mu.Unlock()
	if total > defaultMailboxClientBytes {
		t.Fatalf("one /48 holds %d bytes, over its %d share", total, defaultMailboxClientBytes)
	}
	r := rvDo(t, h, nil, "203.0.113.9:1", "PUT", victim.reqPath(rid(1)),
		[]byte(`{"blob":"c2VhbGVk","readKeyHash":"`+b64.EncodeToString(capHash(readKey(1)))+`"}`),
		map[string]string{mailboxPutCapHeader: b64.EncodeToString(putCap(1))})
	want(t, r, 201, "")
}

// RL-1: one mailbox's live requests hold at most mailboxMaxBytesPerBox.
func TestReviewRL1_PerMailboxByteCap(t *testing.T) {
	t.Setenv("LANGSTROTH_RELAY_MAILBOX_MAX_BYTES_PER_CLIENT", "0")
	e := newMBEnv(t, nil, 1, false)
	want(t, e.register(putCap(1), 0), 201, "")
	n, last := 0, mbResp{}
	for i := 0; i < mailboxMaxLive; i++ {
		body, _ := json.Marshal(map[string]string{"blob": strings.Repeat("A", mailboxMaxRequestBlob), "readKeyHash": b64.EncodeToString(capHash(readKey(i)))})
		want(t, e.do("PUT", e.reqPath(rid(i)), body, map[string]string{mailboxPutCapHeader: b64.EncodeToString(putCap(1))}), 201, "")
		for m := 0; m < mailboxMaxMsgs && last.status != 507; m++ {
			mb, _ := json.Marshal(map[string]string{"blob": strings.Repeat("A", mailboxMaxMsgBlob-1) + string(rune('A'+m)), "to": "members"})
			last = e.do("POST", e.reqPath(rid(i))+"/msgs", mb, readHdr(readKey(i)))
			n++
		}
		if last.status == 507 {
			break
		}
	}
	want(t, last, 507, mbCodeStorage)
	if e.srv.mbox.bytes > mailboxMaxBytesPerBox {
		t.Fatalf("mailbox holds %d bytes", e.srv.mbox.bytes)
	}
}

// RL-2: a key holder can't pin the counter at 2^32-1 with a junk cap: only
// the next counter is a reset, so an honest reset still works.
func TestReviewRL2_CounterCanOnlyStepByOne(t *testing.T) {
	e := newMBEnv(t, nil, 1, false)
	want(t, e.register(putCap(1), 0), 201, "")
	want(t, e.do("PUT", "/v1/mailboxes/"+e.id, e.registerBody(nil, []byte("junk-not-derived"), math.MaxUint32), nil), 409, mbCodeStale)
	want(t, e.do("PUT", "/v1/mailboxes/"+e.id, e.registerBody(nil, []byte("junk-not-derived"), 2), nil), 409, mbCodeStale)
	want(t, e.put(putCap(1), rid(1), readKey(1), nil), 201, "")
	want(t, e.register(putCap(2), 1), 200, "")
	want(t, e.put(putCap(2), rid(2), readKey(2), nil), 201, "")
}

// RL-3: one /48 holds at most mailboxMaxPollsPer48 long polls, so an honest
// requester elsewhere still gets a real long poll.
func TestReviewRL3_OneSlash48CannotHoldEveryLongPoll(t *testing.T) {
	e := newMBEnv(t, nil, 1, false)
	h := e.srv.Handler()
	want(t, e.register(putCap(1), 0), 201, "")
	want(t, e.put(putCap(1), rid(1), readKey(1), nil), 201, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	refused := 0
	for i := 0; i < mailboxMaxPolls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := rvDo(t, h, ctx, rvV6(i/mailboxMaxPollsPerClient), "GET", e.reqPath(rid(1))+"/msgs?after=0&wait=25", nil, readHdr(readKey(1)))
			if r.status == 429 {
				mu.Lock()
				refused++
				mu.Unlock()
			}
		}(i)
	}
	deadline := time.Now().Add(5 * time.Second)
	for e.srv.mbox.polls.cur.Load() < mailboxMaxPollsPer48 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if n := e.srv.mbox.polls.cur.Load(); n > mailboxMaxPollsPer48 {
		t.Fatalf("one /48 holds %d long polls", n)
	}
	start := time.Now()
	done := make(chan mbResp, 1)
	go func() {
		done <- rvDo(t, h, nil, "203.0.113.9:1", "GET", e.reqPath(rid(1))+"/msgs?after=0&wait=1", nil, readHdr(readKey(1)))
	}()
	r := <-done
	want(t, r, 200, "")
	if time.Since(start) < 900*time.Millisecond {
		t.Fatalf("honest poll did not wait (%s)", time.Since(start))
	}
	cancel()
	wg.Wait()
	if refused < mailboxMaxPolls-mailboxMaxPollsPer48 {
		t.Fatalf("only %d of the /48's polls refused", refused)
	}
}

// RL-3: near the caps (this mailbox's, or the relay's), long polls answer at
// once instead of being refused.
func TestReviewRL3_NearTheCapPollsDegradeToWaitZero(t *testing.T) {
	e := newMBEnv(t, nil, 1, false)
	h := e.srv.Handler()
	boxes := []*mbEnv{e}
	for k := byte(2); k <= 7; k++ {
		boxes = append(boxes, rvBox(t, e, k))
	}
	for _, b := range boxes {
		want(t, b.register(putCap(1), 0), 201, "")
		want(t, b.put(putCap(1), rid(1), readKey(1), nil), 201, "")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	poll := func(i int, b *mbEnv) {
		defer wg.Done()
		rvDo(t, h, ctx, rvV4(i/mailboxMaxPollsPerClient), "GET", b.reqPath(rid(1))+"/msgs?after=0&wait=25", nil, readHdr(readKey(1)))
	}
	// Fill box 0 to its own cap from IPv4 clients.
	for i := 0; i < mailboxMaxPollsPerBox; i++ {
		wg.Add(1)
		go poll(i, boxes[0])
	}
	waitFor := func(n int64) {
		deadline := time.Now().Add(5 * time.Second)
		for e.srv.mbox.polls.cur.Load() < n && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor(mailboxMaxPollsPerBox)
	quick := func(b *mbEnv) time.Duration {
		start := time.Now()
		r := rvDo(t, h, nil, "203.0.113.9:1", "GET", b.reqPath(rid(1))+"/msgs?after=0&wait=20", nil, readHdr(readKey(1)))
		want(t, r, 200, "")
		return time.Since(start)
	}
	if d := quick(boxes[0]); d > 2*time.Second {
		t.Fatalf("a poll on a mailbox at its cap waited %s", d)
	}
	// Then the relay to its soft cap across the other mailboxes.
	for i := mailboxMaxPollsPerBox; i < mailboxPollsSoft; i++ {
		wg.Add(1)
		go poll(i, boxes[1+i%6])
	}
	waitFor(mailboxPollsSoft)
	if d := quick(boxes[6]); d > 2*time.Second {
		t.Fatalf("a poll with the relay near its cap waited %s", d)
	}
	cancel()
	wg.Wait()
}

// RL-4: a captured member POST replayed inside the 300 s window is refused
// (the same sealed blob twice), so replays can't fill a request's 10 slots.
func TestReviewRL4_MemberAuthReplayRefused(t *testing.T) {
	e := newMBEnv(t, nil, 1, false)
	want(t, e.register(putCap(1), 0), 201, "")
	want(t, e.put(putCap(1), rid(1), readKey(1), nil), 201, "")
	p := e.reqPath(rid(1)) + "/msgs"
	body := msgBody("members")
	hdr := e.memberHdr(nil, "POST", p, body, 0)
	want(t, e.do("POST", p, body, hdr), 201, "")
	for i := 0; i < mailboxMaxMsgs; i++ {
		e.clock.add(20 * time.Second) // still inside the window
		want(t, e.do("POST", p, body, hdr), 409, mbCodeDuplicate)
	}
	r := e.member("POST", p, msgBody("requester"))
	want(t, r, 201, "")
	if r.body["seq"] != float64(2) {
		t.Fatalf("seq %v after replays", r.body["seq"])
	}
}

// RL-5 (accepted, documented): the read key is inside the request sealed to
// sealKey, so every key holder can act as the requester, including delete.
func TestReviewRL5_AnyKeyHolderCanActAsRequester(t *testing.T) {
	e := newMBEnv(t, nil, 1, false)
	want(t, e.register(putCap(1), 0), 201, "")
	want(t, e.put(putCap(1), rid(1), readKey(1), nil), 201, "")
	want(t, e.do("DELETE", e.reqPath(rid(1)), nil, readHdr(readKey(1))), 204, "")
}

// RL-6: on a tenancy relay a mailbox belongs to the tenant that registered
// it: another tenant's token sees it as unknown, and each tenant has its
// own mailbox quota.
func TestReviewRL6_MailboxesAreTenantScoped(t *testing.T) {
	t.Setenv("LANGSTROTH_RELAY_MAX_MAILBOXES_PER_TENANT", "1")
	f := newTenancyFixture(t, newMemoryStore(), "")
	clock := &mbClock{t: time.UnixMilli(1764000000000)}
	f.srv.mbox.now = clock.now
	f.srv.mbox.putLimiter = newIPRateLimiter(0, 0)
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	e := &mbEnv{t: t, srv: f.srv, url: f.ts.URL, clock: clock, priv: priv, id: mailboxIDOf(priv.Public().(ed25519.PublicKey)), token: f.acme}
	want(t, e.register(putCap(1), 0), 201, "")
	want(t, e.put(putCap(1), rid(1), readKey(1), nil), 201, "")
	list := "/v1/mailboxes/" + e.id + "/requests"
	want(t, e.member("GET", list, nil), 200, "")

	e.token = f.home
	want(t, e.member("GET", list, nil), 404, mbCodeUnknown)
	want(t, e.member("POST", e.reqPath(rid(1))+"/msgs", msgBody("requester")), 404, mbCodeUnknown)
	want(t, e.register(putCap(2), 1), 404, mbCodeUnknown) // no reset across tenants
	e.token = ""
	want(t, e.register(putCap(1), 0), 401, mbCodeTokenRequired)

	// acme's quota is 1 mailbox; home has its own.
	o := rvBox(t, e, 2)
	o.token = f.acme
	want(t, o.register(putCap(1), 0), 429, mbCodeRegister)
	o.token = f.home
	want(t, o.register(putCap(1), 0), 201, "")
}
