package relay

import (
	"fmt"
	"net/http"
	"os"
	"strings"
)

// Per-workspace token scope (opt-in).
//
// The entitlement gate is coarse: any admitted token reads and writes every
// workspace. LIGUS_RELAY_WORKSPACE_SCOPE narrows that with an optional
// "workspaces" claim in signed hrt1 tokens:
//
//	off (default)  the claim is ignored; behavior is unchanged.
//	claims         a token that carries "workspaces" may only use those ids;
//	               a token without the claim keeps coarse access.
//	strict         every workspace route requires a token whose "workspaces"
//	               claim lists the id (or "*"). Callers without signed claims
//	               (open policy, static allowlist) get 403 on workspace routes.
//
// It applies to every /v1/workspaces/{id}/... route (envelopes, events,
// keyring, presence, candidates and, in membership mode, members, invites and
// join), before the handler runs. The claim travels inside the signed claims
// JSON, so the hrt1 format is unchanged; a relay without this setting (or an
// older relay) ignores the claim, so a scoped token is only confined where the
// operator turns this on.
const (
	scopeOff    = "off"
	scopeClaims = "claims"
	scopeStrict = "strict"
)

func workspaceScopeFromEnv() string {
	switch v := strings.ToLower(strings.TrimSpace(getenv("HIVE_RELAY_WORKSPACE_SCOPE"))); v {
	case "", scopeOff:
		return scopeOff
	case scopeClaims, scopeStrict:
		return v
	default:
		// Fail closed on a typo: the operator asked for some scoping.
		fmt.Fprintf(os.Stderr, "ligus-relay: LIGUS_RELAY_WORKSPACE_SCOPE=%q is not off|claims|strict; using strict\n", v)
		return scopeStrict
	}
}

// scopeAllows reports whether claims may use workspace under mode.
func scopeAllows(mode string, claims *TokenClaims, workspace string) bool {
	switch mode {
	case scopeClaims, scopeStrict:
	default:
		return true
	}
	if claims == nil || claims.Workspaces == nil {
		return mode == scopeClaims
	}
	for _, w := range claims.Workspaces {
		if w == "*" || w == workspace {
			return true
		}
	}
	return false
}

// scoped wraps a /v1/workspaces/{id}/... handler with the scope check. It runs
// after the entitlement gate, which put the verified claims in the context.
func (s *Server) scoped(h http.HandlerFunc) http.HandlerFunc {
	if s.wsScope == scopeOff {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !scopeAllows(s.wsScope, claimsFrom(r.Context()), r.PathValue("id")) {
			http.Error(w, "this access token is not scoped to this workspace", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}
