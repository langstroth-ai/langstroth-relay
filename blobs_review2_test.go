package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
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
