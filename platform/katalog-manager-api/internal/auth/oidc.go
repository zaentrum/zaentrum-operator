// Package auth provides an OIDC bearer-token verifier used to gate the
// management plane. The issuer and audience come entirely from the
// operator's environment / discovery document — nothing here is pinned to
// a specific identity provider.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Verifier wraps an OIDC ID-token verifier plus an audience accept-list.
//
// A nil *Verifier (or one whose underlying verifier is nil) means OIDC is
// not configured / unreachable. In that state Middleware lets /healthz and
// /metrics through but rejects every protected request with 503 — the
// management plane is never exposed unauthenticated.
type Verifier struct {
	v         *oidc.IDTokenVerifier
	audiences []string
}

// NewVerifier sets up an OIDC verifier against the issuer's discovery
// document. audiences is comma-separated. The underlying go-oidc verifier
// runs in SkipClientIDCheck mode and the aud claim is checked manually
// against the allow-list, so a token minted for any of several first-party
// clients passes the same instance.
//
// Returns a nil *Verifier and the underlying error if the provider can't be
// reached at boot — the caller logs and continues so /healthz still answers.
func NewVerifier(ctx context.Context, issuer, audiences string) (*Verifier, error) {
	if issuer == "" {
		return nil, errors.New("oidc issuer empty (set OIDC_ISSUER)")
	}
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, err
	}
	auds := splitAndTrim(audiences)
	if len(auds) == 0 {
		return nil, errors.New("oidc audiences empty (set OIDC_AUDIENCE to a comma-separated list)")
	}
	return &Verifier{
		v:         provider.Verifier(&oidc.Config{SkipClientIDCheck: true}),
		audiences: auds,
	}, nil
}

// Middleware enforces a valid Bearer token on protected paths. /healthz and
// /metrics always bypass.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/healthz" || p == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		if v == nil || v.v == nil {
			slog.Warn("oidc not configured; rejecting request", "path", p)
			http.Error(w, "auth unavailable", http.StatusServiceUnavailable)
			return
		}
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if raw == "" {
			http.Error(w, "missing bearer", http.StatusUnauthorized)
			return
		}
		tok, err := v.v.Verify(r.Context(), raw)
		if err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		var claims struct {
			Aud audClaim `json:"aud"`
		}
		if err := tok.Claims(&claims); err != nil {
			http.Error(w, "claims unreadable", http.StatusUnauthorized)
			return
		}
		matched := false
		for _, want := range v.audiences {
			if slices.Contains([]string(claims.Aud), want) {
				matched = true
				break
			}
		}
		if !matched {
			slog.Warn("audience not in allowlist", "got", []string(claims.Aud), "want_any_of", v.audiences)
			http.Error(w, "audience not permitted", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// audClaim handles both the string and []string forms of the OIDC aud claim.
type audClaim []string

func (a *audClaim) UnmarshalJSON(b []byte) error {
	var arr []string
	if err := json.Unmarshal(b, &arr); err == nil {
		*a = arr
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*a = []string{s}
	return nil
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}
