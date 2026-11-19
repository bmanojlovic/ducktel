// Package httpauth provides bearer-token middleware shared by ducktel's HTTP
// surfaces.
//
// The OTLP receiver and the MCP server are separate trust domains — one grants
// write access to senders, the other read access to consumers — so each has its
// own token, but both check it identically.
//
// Besides those shared (legacy) tokens, a request may present a per-client key
// from a keys.Store; the key must grant the scope the surface requires, and the
// resulting keys.Principal is put on the request context for the handlers that
// enforce tenants.
package httpauth

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/davidgeorgehope/ducktel/internal/keys"
)

type ctxKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p *keys.Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFrom returns the principal a request authenticated as. A request
// that passed through no auth (auth disabled) is treated as legacy, which is
// exactly today's unrestricted behaviour.
func PrincipalFrom(ctx context.Context) *keys.Principal {
	if p, ok := ctx.Value(ctxKey{}).(*keys.Principal); ok && p != nil {
		return p
	}
	return keys.LegacyPrincipal
}

// Bearer wraps a handler with an RFC 6750 bearer-token check.
//
// It is a no-op when token is empty, and fails closed (401) when a token is
// configured but the request does not present it. The `Bearer ` prefix is
// required; the scheme name is case-insensitive. A bare token is rejected.
func Bearer(token string, next http.Handler) http.Handler {
	return Authenticate(token, nil, "", next)
}

// Authenticate accepts either the shared legacy token or a key from store that
// grants scope. With neither a token nor any keys configured it is a no-op, as
// Bearer always was.
func Authenticate(token string, store *keys.Store, scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if token == "" && store == nil {
			next.ServeHTTP(w, req)
			return
		}
		scheme, cred, ok := strings.Cut(req.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || cred == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Constant-time compare so the token cannot be recovered by timing.
		if token != "" && subtle.ConstantTimeCompare([]byte(cred), []byte(token)) == 1 {
			next.ServeHTTP(w, req.WithContext(WithPrincipal(req.Context(), keys.LegacyPrincipal)))
			return
		}
		// Keys are looked up by the SHA-256 of the presented token, so there is
		// no secret-dependent comparison to time.
		if p := store.Lookup(cred); p != nil && (scope == "" || p.Has(scope)) {
			next.ServeHTTP(w, req.WithContext(WithPrincipal(req.Context(), p)))
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}
