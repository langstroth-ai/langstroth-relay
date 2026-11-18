package langstrothauth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func adminJSON(t *testing.T, base, tok, method, path string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, base+path, bytes.NewReader(nil))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

// Revoking one device's sessions ends exactly that device's refresh tokens
// (its sign-in session), leaves the person's other devices signed in, is
// idempotent, and needs the admin token.
func TestAdminRevokeDeviceEndsOnlyThatDevicesSession(t *testing.T) {
	h, _ := homeHarness(t)
	phone, laptop := newDevice("4d1b6c1e-0000-4000-8000-000000000001"), newDevice("dev-mac-1")
	h.loginTokens("home", phone) // jit-create the user
	grantJIT(t, h, "home")
	pt := h.loginTokens("home", phone)
	lt := h.loginTokens("home", laptop)
	if pt.status != 200 || lt.status != 200 {
		t.Fatalf("logins: %s / %s", fmtStatus(pt), fmtStatus(lt))
	}
	admin := httptestAdmin(t, h)
	path := "/admin/v1/tenants/home/devices/" + phone.id + "/revoke"

	if code, _ := adminJSON(t, admin, "", "POST", path); code != http.StatusUnauthorized {
		t.Fatalf("no admin token: %d", code)
	}
	if code, _ := adminJSON(t, admin, "wrong-token-0123456789abcdef0123456789", "POST", path); code != http.StatusUnauthorized {
		t.Fatalf("wrong admin token: %d", code)
	}
	if code, _ := adminJSON(t, admin, "admin-token-for-tests-0123456789abcdef", "POST", "/admin/v1/tenants/nope/devices/"+phone.id+"/revoke"); code != http.StatusNotFound {
		t.Fatalf("unknown tenant: %d", code)
	}
	if code, _ := adminJSON(t, admin, "admin-token-for-tests-0123456789abcdef", "POST", "/admin/v1/tenants/home/devices/bad%20id/revoke"); code != http.StatusBadRequest {
		t.Fatalf("bad device id: %d", code)
	}
	code, m := adminJSON(t, admin, "admin-token-for-tests-0123456789abcdef", "POST", path)
	if code != http.StatusOK || m["revoked"] != float64(1) {
		t.Fatalf("revoke device: %d %v", code, m)
	}
	// The phone's refresh is refused; the laptop's still works.
	if tr := h.refresh("home", pt.Refresh, pt.Nonce, phone); tr.status == 200 {
		t.Fatalf("revoked device refreshed: %s", fmtStatus(tr))
	}
	if tr := h.refresh("home", lt.Refresh, lt.Nonce, laptop); tr.status != 200 {
		t.Fatalf("other device lost its session: %s", fmtStatus(tr))
	}
	// Idempotent.
	code, m = adminJSON(t, admin, "admin-token-for-tests-0123456789abcdef", "POST", path)
	if code != http.StatusOK || m["revoked"] != float64(0) {
		t.Fatalf("second revoke: %d %v", code, m)
	}
}
