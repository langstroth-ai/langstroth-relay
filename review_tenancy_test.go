package relay

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Adversarial review of tenancy and community limits (prove, then fix).

// F3: /metrics is public when LANGSTROTH_RELAY_METRICS_TOKEN is unset, and
// with tenancy it lists every tenant's id, request volume, rooms and bytes:
// one tenant (or anyone) learns who else shares the relay and how much they
// use it.
func TestTenancyMetricsNeedAToken(t *testing.T) {
	t.Setenv("LANGSTROTH_RELAY_METRICS_TOKEN", "")
	f := newTenancyFixtureNoCheck(t, newMemoryStore(), "")
	if err := tenancyConfigError(); err == nil || !strings.Contains(err.Error(), "LANGSTROTH_RELAY_METRICS_TOKEN") {
		t.Fatalf("tenancy started without a metrics token: %v", err)
	}
	// A library caller that skips the startup check still gets no tenant
	// series on an open /metrics.
	req(t, "POST", f.ts.URL+"/v1/workspaces/w/envelopes", f.acme, 1)
	m := metricsBody(t, f.ts.URL)
	if strings.Contains(m, `tenant="acme"`) || strings.Contains(m, "hive_relay_tenant_") {
		t.Fatalf("open /metrics names tenants:\n%s", m)
	}
}

// barrierHosts holds every TunnelHostsOfTenant caller until n have arrived,
// so concurrent registrations all read the count before any writes.
type barrierHosts struct {
	TunnelStore
	n       int
	arrived chan struct{}
	release chan struct{}
}

func (b *barrierHosts) TunnelHostsOfTenant(ctx context.Context, tenant string) (int64, error) {
	n, err := b.TunnelStore.(tenantHostCounter).TunnelHostsOfTenant(ctx, tenant)
	b.arrived <- struct{}{}
	select {
	case <-b.release:
	case <-time.After(300 * time.Millisecond): // a serialized caller arrives alone
	}
	return n, err
}

// F4: max_hosts is a count-then-insert: hosts registering at the same time
// all see the count below the cap and all register.
func TestTenancyMaxHostsRace(t *testing.T) {
	acmeKey := randKey(t)
	t.Setenv("LANGSTROTH_RELAY_METRICS_TOKEN", "ops-metrics-token")
	t.Setenv("LANGSTROTH_RELAY_TOKEN_PUBKEY", "")
	t.Setenv("LANGSTROTH_RELAY_TENANCY", fmt.Sprintf(`{"tenants": [{"id": "acme", "issuer_pubkeys": [%q], "limits": {"max_hosts": 1}}]}`, pubHex(acmeKey)))
	env := newTunnelEnv(t, nil, nil, nil)
	const n = 4
	b := &barrierHosts{TunnelStore: env.hub.store, n: n, arrived: make(chan struct{}, n), release: make(chan struct{})}
	env.hub.store = b
	go func() {
		for i := 0; i < n; i++ {
			<-b.arrived
		}
		close(b.release)
	}()
	reg := issueToken(acmeKey, TokenClaims{Sub: "box", Tid: "acme", Exp: uint64(time.Now().Add(time.Hour).Unix())})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, sk, _ := ed25519.GenerateKey(rand.Reader)
			pub := sk.Public().(ed25519.PublicKey)
			id := TunnelHostID(pub)
			nonce := []byte("0123456789abcdef0123456789abcdef")
			v := tunnelVersion
			m := &relayMsg{T: "hello", V: &v, HostID: id, HostPub: b64u(pub), Reg: reg,
				Sig: b64u(ed25519.Sign(sk, tunnelCtx("host-auth", []byte(id), nonce)))}
			_, _ = env.hub.admitHost(t.Context(), m, nonce)
		}()
	}
	wg.Wait()
	got, _ := b.TunnelStore.(tenantHostCounter).TunnelHostsOfTenant(t.Context(), "acme")
	if got > 1 {
		t.Fatalf("max_hosts 1, but %d hosts registered concurrently", got)
	}
}

// F5: the memory store created a room on a presence or candidate write
// without a last-write time (0), so the next hourly retention pass expired a
// room created a minute earlier (Postgres stamps it with the creation time).
func TestDeviceBlobRoomNotExpiredAtOnce(t *testing.T) {
	roomStores(t, func(t *testing.T, s Store) {
		if err := s.PutPresence(qctx(writeQuota{}), "fresh", "d1", raw(`{"online":true}`)); err != nil {
			t.Fatal(err)
		}
		n, err := s.(roomStore).PruneIdleRooms(bg, time.Now().Add(-24*time.Hour).Unix())
		if err != nil || n != 0 {
			t.Fatalf("retention expired %d rooms written just now (err %v)", n, err)
		}
		if m, _ := s.PresenceBlobs(bg, "fresh"); len(m) != 1 {
			t.Fatalf("presence of a fresh room wiped: %v", m)
		}
	})
}

// F6: the memory store kept every expired room's struct forever, even one
// that never held an envelope (no sequence to keep) and has no roster, so
// creating and abandoning rooms grew the heap and the byte budget without
// bound. Such a room is now dropped whole when it expires or is purged.
func TestExpiredEmptyRoomFreesMemory(t *testing.T) {
	s := newMemoryStore()
	for i := 0; i < 100; i++ {
		room := fmt.Sprintf("spray-%d", i)
		if err := s.PutPresence(qctx(writeQuota{}), room, "d", raw(`1`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AppendEnvelope(qctx(writeQuota{}), "kept", raw(`1`), ""); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneIdleRooms(bg, time.Now().Add(time.Hour).Unix()); err != nil || n != 101 {
		t.Fatalf("expired %d (%v)", n, err)
	}
	s.mu.RLock()
	left, budget := len(s.workspaces), s.storeBytes
	s.mu.RUnlock()
	if left != 1 {
		t.Fatalf("%d expired room structs kept; only the one with a sequence should stay", left)
	}
	if want := int64(wsOverheadBytes + len("kept")); budget != want {
		t.Fatalf("store budget %d after expiry, want %d", budget, want)
	}
	// The kept room continues its sequence.
	if seq, _ := s.AppendEnvelope(qctx(writeQuota{}), "kept", raw(`2`), ""); seq != 2 {
		t.Fatalf("seq %d", seq)
	}
	// Purging a room without a sequence drops it too.
	_ = s.PutPresence(qctx(writeQuota{}), "p", "d", raw(`1`))
	if _, err := s.PurgeRoom(bg, "p"); err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	_, still := s.workspaces["p"]
	s.mu.RUnlock()
	if still {
		t.Fatal("a purged room with no sequence stayed in memory")
	}
}
