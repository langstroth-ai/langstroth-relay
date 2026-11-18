package relay

import (
	"strings"
	"testing"
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
