package relay

import (
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Second adversarial review of the blob channel (prove-then-fix).

// An upload that takes longer than the server's write timeout must still get
// its answer: otherwise the blob is stored, the client never learns its id,
// retries, and each retry burns quota on an orphan.
func TestHTTPBlobSlowUploadOutlivesWriteTimeout(t *testing.T) {
	s := newMemoryStore()
	srv := New(Options{Store: s, Entitlement: entitlementPolicy{kind: entOpen}})
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.Config.ReadTimeout = 300 * time.Millisecond
	ts.Config.WriteTimeout = 300 * time.Millisecond
	ts.Start()
	t.Cleanup(ts.Close)
	seedHTTP(t, ts.URL, "w", "")

	pr, pw := io.Pipe()
	go func() {
		for i := 0; i < 8; i++ {
			_, _ = pw.Write([]byte("chunk-of-ciphertext"))
			time.Sleep(100 * time.Millisecond)
		}
		_ = pw.Close()
	}()
	r, _ := http.NewRequest("POST", ts.URL+"/v1/workspaces/w/blobs", pr)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("slow upload lost its answer: %v (stored blobs: %d)", err, s.blobTotal.count)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("slow upload: %d %s", resp.StatusCode, b)
	}
}

// stalledUpload starts an upload that sends some bytes and then waits for
// release; it returns once the server holds the request.
func stalledUpload(t *testing.T, url, token string) (release func() (int, []byte)) {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() }) // never leave the server holding it
	r, _ := http.NewRequest("POST", url, pr)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	type res struct {
		code int
		body []byte
	}
	done := make(chan res, 1)
	go func() {
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			done <- res{code: -1, body: []byte(err.Error())}
			return
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		done <- res{resp.StatusCode, b}
	}()
	if _, err := pw.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // the handler is now reading the body
	return func() (int, []byte) {
		_ = pw.Close()
		r := <-done
		return r.code, r.body
	}
}

// Every upload in flight holds up to the blob size cap in memory for up to
// the transfer window. Without a bound on concurrent uploads, a client that
// opens many uploads and never finishes them (slowloris) exhausts the heap.
func TestHTTPBlobUploadsInFlightCapped(t *testing.T) {
	sk := testKey(t)
	t.Setenv("LANGSTROTH_RELAY_BLOB_MAX_INFLIGHT", "2")
	t.Setenv("LANGSTROTH_RELAY_BLOB_WRITER_INFLIGHT", "1")
	srv := New(Options{Store: newMemoryStore(), Entitlement: entitlementPolicy{kind: entSigned, pubkey: sk.Public().(ed25519.PublicKey)}})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	tk := func(sub string) string {
		return issueToken(sk, TokenClaims{Sub: sub, Exp: uint64(time.Now().Add(time.Hour).Unix())})
	}
	alice, bob, carol := tk("alice"), tk("bob"), tk("carol")
	seedHTTP(t, ts.URL, "w", alice)
	url := ts.URL + "/v1/workspaces/w/blobs"

	a1 := stalledUpload(t, url, alice)
	// alice's second concurrent upload: over her own in-flight cap.
	resp, b := blobHTTP(t, "POST", url, alice, []byte("x"))
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(b), limBlobInflight) || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("second upload in flight for one writer: %d %s", resp.StatusCode, b)
	}
	b1 := stalledUpload(t, url, bob)
	// The relay-wide cap (2) is now full: carol is refused too.
	resp, b = blobHTTP(t, "POST", url, carol, []byte("x"))
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(b), limBlobInflight) {
		t.Fatalf("upload past the relay-wide in-flight cap: %d %s", resp.StatusCode, b)
	}
	if code, b := a1(); code != http.StatusCreated {
		t.Fatalf("stalled upload a1: %d %s", code, b)
	}
	if code, b := b1(); code != http.StatusCreated {
		t.Fatalf("stalled upload b1: %d %s", code, b)
	}
	// Slots are given back.
	if resp, b := blobHTTP(t, "POST", url, carol, []byte("x")); resp.StatusCode != http.StatusCreated {
		t.Fatalf("after the uploads finished: %d %s", resp.StatusCode, b)
	}
	if resp, b := blobHTTP(t, "POST", url, alice, []byte("x")); resp.StatusCode != http.StatusCreated {
		t.Fatalf("alice after hers finished: %d %s", resp.StatusCode, b)
	}
}
