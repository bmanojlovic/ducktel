package httpauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/davidgeorgehope/ducktel/internal/keys"
)

func testStore(t *testing.T, entries ...keys.Entry) *keys.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys.json")
	data, _ := json.Marshal(entries)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := keys.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAuthenticate(t *testing.T) {
	store := testStore(t,
		keys.Entry{Name: "reader", SHA256: keys.Hash("read-key"), Scopes: []string{keys.ScopeRead}, Tenants: []string{"acme"}},
		keys.Entry{Name: "sender", SHA256: keys.Hash("ingest-key"), Scopes: []string{keys.ScopeIngest}, Tenants: []string{"acme"}},
	)
	var got *keys.Principal
	h := Authenticate("shared", store, keys.ScopeRead, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = PrincipalFrom(r.Context())
	}))

	cases := []struct {
		auth string
		code int
		name string
	}{
		{"", http.StatusUnauthorized, ""},
		{"Bearer nope", http.StatusUnauthorized, ""},
		{"read-key", http.StatusUnauthorized, ""},          // bare token, no scheme
		{"Bearer ingest-key", http.StatusUnauthorized, ""}, // wrong scope
		{"Bearer shared", http.StatusOK, "legacy"},
		{"bearer read-key", http.StatusOK, "reader"},
	}
	for _, c := range cases {
		got = nil
		req := httptest.NewRequest("GET", "/", nil)
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.code {
			t.Errorf("%q: status %d, want %d", c.auth, rec.Code, c.code)
		}
		if c.code == http.StatusOK && (got == nil || got.Name != c.name) {
			t.Errorf("%q: principal %+v, want %s", c.auth, got, c.name)
		}
	}
}

func TestAuthenticateKeysOnly(t *testing.T) {
	store := testStore(t, keys.Entry{Name: "reader", SHA256: keys.Hash("read-key"), Scopes: []string{keys.ScopeRead}, Tenants: []string{"*"}})
	h := Authenticate("", store, keys.ScopeRead, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("keys-only without a token: %d, want 401 (empty shared token must not mean open)", rec.Code)
	}
}

func TestNoAuthConfiguredIsOpenAndLegacy(t *testing.T) {
	var got *keys.Principal
	h := Authenticate("", nil, keys.ScopeRead, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = PrincipalFrom(r.Context())
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK || got == nil || !got.Legacy {
		t.Fatalf("status %d principal %+v", rec.Code, got)
	}
}
