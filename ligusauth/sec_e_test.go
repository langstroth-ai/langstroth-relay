package ligusauth

import (
	"net/http"
	"net/url"
	"testing"
)

// E-1: in jit mode a first sign-in with an UNVERIFIED email must not claim
// that address as the new user's userName. Otherwise a person who can set
// any unverified email at the IdP squats the real owner's name: the real
// owner's later sign-in is refused (bound_to_other_subject), and an IdP's
// SCIM client that looks users up by userName before adding them to groups
// links the squatter's record and grants it the owner's workspaces.
func TestJITUnverifiedEmailCannotSquatUserName(t *testing.T) {
	h, idp := homeHarness(t)
	idp.set(func(m *mockIdP) {
		m.user = idpUser{sub: "mallory-sub", email: "alice@example.com", emailVerified: boolp(false)}
	})
	if got := loginOutcome(t, h, "home"); got != "" {
		t.Fatalf("jit sign-in (no grant needed for the code) failed: %q", got)
	}
	code, out := h.scim("home", testSCIMToken, "GET", "/Users?filter="+url.QueryEscape(`userName eq "alice@example.com"`), nil)
	if code != http.StatusOK {
		t.Fatalf("scim filter: %d", code)
	}
	if n, _ := out["totalResults"].(float64); n != 0 {
		t.Fatalf("an unverified email became a userName: the SCIM lookup for alice finds the squatter (%v)", out)
	}
	// The real Alice (verified) can still sign in.
	idp.set(func(m *mockIdP) {
		m.user = idpUser{sub: "alice-sub", email: "alice@example.com", emailVerified: boolp(true)}
	})
	if got := loginOutcome(t, h, "home"); got != "" {
		t.Fatalf("the real owner of the address is locked out: %q", got)
	}
}
