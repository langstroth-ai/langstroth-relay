package relay

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// The memory store's JSON snapshot is rewritten every few seconds. Blob
// bytes inside it (base64) make every flush rewrite every attachment: a
// workspace's 512 MiB quota becomes ~700 MB of JSON every 5 s. The snapshot
// must hold only metadata; the bytes live in their own files.
func TestBlobSnapshotKeepsBytesOutOfJSON(t *testing.T) {
	dir := t.TempDir()
	s := newMemoryStoreWithPersistence(dir)
	seedRoom(t, s, "w")
	m, d := testBlob("w", "ip:a", 256<<10)
	if err := s.PutBlob(bg, m, d, blobQuota{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(bg); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, snapshotFile))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 32<<10 {
		t.Fatalf("snapshot is %d bytes for one 256 KiB blob: blob bytes ride the JSON", fi.Size())
	}
	r := newMemoryStoreWithPersistence(dir)
	if _, data, ok, _ := r.GetBlob(bg, "w", m.ID, time.Now().Unix()); !ok || !bytes.Equal(data, d) {
		t.Fatalf("blob not restored: ok=%v", ok)
	}
	// Deleting removes its file; a restore then doesn't bring it back even
	// from a snapshot written before the delete.
	if ok, _ := r.DeleteBlob(bg, "w", m.ID); !ok {
		t.Fatal("delete")
	}
	r2 := newMemoryStoreWithPersistence(dir) // the snapshot still lists it
	if _, _, ok, _ := r2.GetBlob(bg, "w", m.ID, time.Now().Unix()); ok {
		t.Fatal("a deleted blob came back from the snapshot")
	}
	if r2.blobTotal.count != 0 {
		t.Fatalf("totals after restore: %+v", r2.blobTotal)
	}
}

// A blob file on disk that no snapshot lists (an upload that landed just
// before a crash) is removed at boot, and a corrupted file isn't served.
func TestBlobSnapshotOrphanAndCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	s := newMemoryStoreWithPersistence(dir)
	seedRoom(t, s, "w")
	keep, kd := testBlob("w", "ip:a", 100)
	if err := s.PutBlob(bg, keep, kd, blobQuota{}); err != nil {
		t.Fatal(err)
	}
	bad, bd := testBlob("w", "ip:a", 100)
	if err := s.PutBlob(bg, bad, bd, blobQuota{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(bg); err != nil {
		t.Fatal(err)
	}
	orphan, od := testBlob("w", "ip:a", 100) // written after the last flush
	if err := s.PutBlob(bg, orphan, od, blobQuota{}); err != nil {
		t.Fatal(err)
	}
	// Corrupt one stored file (same length, other bytes).
	if err := os.WriteFile(s.blobFile("w", bad.ID), bytes.Repeat([]byte{1}, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newMemoryStoreWithPersistence(dir)
	if _, _, ok, _ := r.GetBlob(bg, "w", keep.ID, time.Now().Unix()); !ok {
		t.Fatal("intact blob lost")
	}
	if _, _, ok, _ := r.GetBlob(bg, "w", bad.ID, time.Now().Unix()); ok {
		t.Fatal("a corrupted blob file was served")
	}
	if _, err := os.Stat(r.blobFile("w", orphan.ID)); !os.IsNotExist(err) {
		t.Fatalf("orphan blob file left on disk: %v", err)
	}
	if r.blobTotal.count != 1 {
		t.Fatalf("totals %+v", r.blobTotal)
	}
}

// A snapshot written before blob files (bytes inline) still restores, and
// its bytes move out to files.
func TestBlobSnapshotInlineBytesMigrate(t *testing.T) {
	dir := t.TempDir()
	s := newMemoryStoreWithPersistence(dir)
	seedRoom(t, s, "w")
	m, d := testBlob("w", "ip:a", 1000)
	snap := s.toSnapshot()
	snap.Blobs = []blobSnap{{Room: "w", ID: m.ID, Uploader: m.Uploader, Size: m.Size, SHA256: m.SHA256, CreatedAt: m.CreatedAt, Data: d}}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, snapshotFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	r := newMemoryStoreWithPersistence(dir)
	if _, data, ok, _ := r.GetBlob(bg, "w", m.ID, time.Now().Unix()); !ok || !bytes.Equal(data, d) {
		t.Fatal("inline blob not restored")
	}
	if err := r.Flush(bg); err != nil {
		t.Fatal(err)
	}
	r2 := newMemoryStoreWithPersistence(dir)
	if _, data, ok, _ := r2.GetBlob(bg, "w", m.ID, time.Now().Unix()); !ok || !bytes.Equal(data, d) {
		t.Fatal("blob lost after its bytes moved to a file")
	}
}
