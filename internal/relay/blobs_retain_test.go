package relay

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Retain route (blobs.go retainBlob, docs/blobs.md "Renewing"): clients
// renew the blobs their workspace still refers to; the operator's maximum
// age caps every renewal.

const day = int64(24 * 60 * 60)

// setBlobTimes rewrites a stored blob's creation and expiry times.
func setBlobTimes(t *testing.T, s Store, room, id string, created, expires int64) {
	t.Helper()
	switch st := s.(type) {
	case *memoryStore:
		st.mu.Lock()
		b := st.blobs[room][id]
		b.meta.CreatedAt, b.meta.ExpiresAt = created, expires
		st.mu.Unlock()
	case *postgresStore:
		if _, err := st.pool.Exec(bg, `UPDATE relay_blobs SET created_at = $3, expires_at = $4 WHERE workspace = $1 AND id = $2`,
			room, id, created, expires); err != nil {
			t.Fatal(err)
		}
	}
}

func blobExpiry(t *testing.T, s Store, room, id string) int64 {
	t.Helper()
	m, _, ok, err := s.(blobStore).GetBlob(bg, room, id, 0)
	if err != nil || !ok {
		t.Fatalf("blob %s gone: %v", id, err)
	}
	return m.ExpiresAt
}

func TestRetainedExpiry(t *testing.T) {
	for _, c := range []struct{ cur, created, until, maxAge, want int64 }{
		{100, 0, 300, 0, 300},        // no cap
		{100, 0, 300, 1000, 300},     // under the cap
		{100, 0, 300, 200, 200},      // capped at created + maxAge
		{250, 0, 300, 200, 250},      // never earlier than it is
		{500, 0, 300, 0, 500},        // a shorter renewal doesn't shorten
		{0, 0, 300, 200, 0},          // kept until deleted stays so
		{100, 0, 0, 0, 100},          // retention off: unchanged
		{100, 1000, 3000, 365, 1365}, // created later than the old expiry
	} {
		if got := retainedExpiry(c.cur, c.created, c.until, c.maxAge); got != c.want {
			t.Errorf("retainedExpiry(%d, %d, %d, %d) = %d, want %d", c.cur, c.created, c.until, c.maxAge, got, c.want)
		}
	}
}

func TestBlobRetainStore(t *testing.T) {
	blobStores(t, func(t *testing.T, s Store) {
		rt, ok := s.(blobRetainer)
		if !ok {
			t.Fatal("built-in store without RetainBlob")
		}
		seedRoom(t, s, "w")
		seedRoom(t, s, "other")
		m, err := putTestBlob(s, "w", "alice", 10, blobQuota{})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().Unix()
		setBlobTimes(t, s, "w", m.ID, now-20*day, now+10*day)
		// Renewal: now plus the retention.
		exp, found, err := rt.RetainBlob(bg, "w", m.ID, now, now+30*day, 365*day)
		if err != nil || !found || exp != now+30*day || blobExpiry(t, s, "w", m.ID) != now+30*day {
			t.Fatalf("renew: %d %v %v", exp, found, err)
		}
		// Idempotent, and a shorter ask never shortens.
		for _, until := range []int64{now + 30*day, now + day} {
			if exp, found, _ := rt.RetainBlob(bg, "w", m.ID, now, until, 365*day); !found || exp != now+30*day {
				t.Fatalf("repeat with until %d: %d %v", until, exp, found)
			}
		}
		// The cap: never past created plus the maximum age.
		setBlobTimes(t, s, "w", m.ID, now-360*day, now+day)
		if exp, _, _ := rt.RetainBlob(bg, "w", m.ID, now, now+30*day, 365*day); exp != now+5*day {
			t.Fatalf("capped: %d, want %d", exp, now+5*day)
		}
		// No cap (0): the full retention.
		if exp, _, _ := rt.RetainBlob(bg, "w", m.ID, now, now+30*day, 0); exp != now+30*day {
			t.Fatalf("uncapped: %d", exp)
		}
		// Kept until deleted stays so.
		setBlobTimes(t, s, "w", m.ID, now, 0)
		if exp, found, _ := rt.RetainBlob(bg, "w", m.ID, now, now+30*day, 365*day); !found || exp != 0 {
			t.Fatalf("never-expiring blob: %d %v", exp, found)
		}
		// Unknown, other workspace, expired: not found, nothing created.
		setBlobTimes(t, s, "w", m.ID, now-40*day, now-1)
		for _, c := range []struct{ room, id string }{{"w", newBlobID()}, {"other", m.ID}, {"w", m.ID}} {
			if _, found, err := rt.RetainBlob(bg, c.room, c.id, now, now+30*day, 0); found || err != nil {
				t.Fatalf("retain %s/%s: %v %v", c.room, c.id, found, err)
			}
		}
		checkBlobUsage(t, s)
	})
}

func retainHTTP(t *testing.T, url, ws, id, token string) (int, blobResp) {
	t.Helper()
	resp, b := blobHTTP(t, "POST", url+"/v1/workspaces/"+ws+"/blobs/"+id+"/retain", token, nil)
	var br blobResp
	_ = json.Unmarshal(b, &br)
	return resp.StatusCode, br
}

func TestHTTPBlobRetain(t *testing.T) {
	blobStores(t, func(t *testing.T, s Store) {
		ts := blobServer(t, s, map[string]string{
			"LANGSTROTH_RELAY_BLOB_MAX_AGE_DAYS":   "31",
			"LANGSTROTH_RELAY_BLOB_ROOM_MAX_COUNT": "1",
		})
		seedHTTP(t, ts.URL, "w", "")
		seedHTTP(t, ts.URL, "w2", "")
		resp, b := blobHTTP(t, "POST", ts.URL+"/v1/workspaces/w/blobs", "", []byte("sealed"))
		var up blobResp
		_ = json.Unmarshal(b, &up)
		if resp.StatusCode != 201 {
			t.Fatalf("upload: %d %s", resp.StatusCode, b)
		}
		now := time.Now().Unix()
		// Ten days in, a day left: renewed to now plus 30 days.
		setBlobTimes(t, s, "w", up.ID, now-day, now+day)
		code, br := retainHTTP(t, ts.URL, "w", up.ID, "")
		if code != 200 || br.ID != up.ID || br.ExpiresAt < now+30*day-60 || br.ExpiresAt > now+30*day+60 {
			t.Fatalf("retain: %d %+v", code, br)
		}
		// Idempotent.
		if code, again := retainHTTP(t, ts.URL, "w", up.ID, ""); code != 200 || again.ExpiresAt < br.ExpiresAt {
			t.Fatalf("repeat: %d %+v", code, again)
		}
		// Capped at created + 31 days.
		setBlobTimes(t, s, "w", up.ID, now-5*day, now+day)
		if code, br := retainHTTP(t, ts.URL, "w", up.ID, ""); code != 200 || br.ExpiresAt != now-5*day+31*day {
			t.Fatalf("capped: %d %+v, want %d", code, br, now+26*day)
		}
		// Unknown, malformed, other workspace: 404 blob_not_found.
		for _, c := range []struct{ ws, id string }{{"w", newBlobID()}, {"w", "short"}, {"w2", up.ID}} {
			if code, br := retainHTTP(t, ts.URL, c.ws, c.id, ""); code != 404 || br.Error != "blob_not_found" {
				t.Fatalf("retain %s/%s: %d %+v", c.ws, c.id, code, br)
			}
		}
		// Still counts against the quota: the room holds its one blob.
		resp, b = blobHTTP(t, "POST", ts.URL+"/v1/workspaces/w/blobs", "", []byte("more"))
		if resp.StatusCode != 507 || !strings.Contains(string(b), limBlobRoomCount) {
			t.Fatalf("quota after retain: %d %s", resp.StatusCode, b)
		}
		checkBlobUsage(t, s)
		// Expired: 404, and retain doesn't revive it.
		setBlobTimes(t, s, "w", up.ID, now-40*day, now-1)
		if code, _ := retainHTTP(t, ts.URL, "w", up.ID, ""); code != 404 {
			t.Fatalf("expired: %d", code)
		}
	})
}

func TestHTTPBlobRetainOffAndRate(t *testing.T) {
	ts := blobServer(t, newMemoryStore(), map[string]string{"LANGSTROTH_RELAY_BLOBS": "off"})
	seedHTTP(t, ts.URL, "w", "")
	if code, _ := retainHTTP(t, ts.URL, "w", newBlobID(), ""); code != 404 {
		t.Fatalf("blobs off: %d", code)
	}
	ts = blobServer(t, newMemoryStore(), map[string]string{"LANGSTROTH_RELAY_BLOBS": "on",
		"LANGSTROTH_RELAY_PUSH_RATE": "0.001", "LANGSTROTH_RELAY_PUSH_BURST": "3"})
	seedHTTP(t, ts.URL, "w", "")
	resp, b := blobHTTP(t, "POST", ts.URL+"/v1/workspaces/w/blobs", "", []byte("x"))
	var up blobResp
	_ = json.Unmarshal(b, &up)
	if resp.StatusCode != 201 {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	if code, _ := retainHTTP(t, ts.URL, "w", up.ID, ""); code != 200 {
		t.Fatalf("retain: %d", code)
	}
	if code, _ := retainHTTP(t, ts.URL, "w", up.ID, ""); code != http.StatusTooManyRequests {
		t.Fatalf("retain past the write rate: %d", code)
	}
}

// On a gated relay the retain route needs what a download needs: a valid
// token and, with relay membership, a place on the roster.
func TestHTTPBlobRetainGated(t *testing.T) {
	ts, sk := membershipServer(t)
	tk := func(sub string) string {
		return issueToken(sk, TokenClaims{Sub: sub, Exp: uint64(time.Now().Add(time.Hour).Unix())})
	}
	owner, bob, eve := tk("owner"), tk("bob"), tk("eve")
	if code, b := req(t, "POST", ts.URL+"/v1/workspaces/team/members/claim", owner, map[string]string{"login": "o"}); code != 200 {
		t.Fatalf("claim: %d %s", code, b)
	}
	if code, b := req(t, "POST", ts.URL+"/v1/workspaces/team/members", owner, map[string]string{"account": "bob", "role": "viewer"}); code != 200 {
		t.Fatalf("add bob: %d %s", code, b)
	}
	seedHTTP(t, ts.URL, "team", owner)
	resp, b := blobHTTP(t, "POST", ts.URL+"/v1/workspaces/team/blobs", owner, []byte("bytes"))
	var up blobResp
	_ = json.Unmarshal(b, &up)
	if resp.StatusCode != 201 {
		t.Fatalf("upload: %d %s", resp.StatusCode, b)
	}
	forged := issueToken(testKeyOther(t), TokenClaims{Sub: "owner", Exp: uint64(time.Now().Add(time.Hour).Unix())})
	for _, c := range []struct {
		tok  string
		want int
	}{{"", 401}, {"garbage", 401}, {forged, 401}, {eve, 403}} {
		if code, _ := retainHTTP(t, ts.URL, "team", up.ID, c.tok); code != c.want {
			t.Fatalf("retain with %q: %d, want %d", c.tok, code, c.want)
		}
	}
	// A member who isn't the uploader (a Viewer, even) may renew.
	if code, br := retainHTTP(t, ts.URL, "team", up.ID, bob); code != 200 || br.ExpiresAt == 0 {
		t.Fatalf("member retain: %d %+v", code, br)
	}
	if code, _ := retainHTTP(t, ts.URL, "team", newBlobID(), bob); code != 404 {
		t.Fatalf("member retain of an unknown blob: %d", code)
	}
}

func TestBlobMaxAgeConfig(t *testing.T) {
	if c := blobConfigFromEnv(); c.maxAge != 365*24*time.Hour {
		t.Fatalf("default max age: %v", c.maxAge)
	}
	t.Setenv("LANGSTROTH_RELAY_BLOB_MAX_AGE_DAYS", "0")
	if c := blobConfigFromEnv(); c.maxAge != 0 {
		t.Fatalf("0 = no cap: %v", c.maxAge)
	}
	t.Setenv("LANGSTROTH_RELAY_BLOB_MAX_AGE_DAYS", "90")
	if c := blobConfigFromEnv(); c.maxAge != 90*24*time.Hour {
		t.Fatalf("90: %v", c.maxAge)
	}
}
