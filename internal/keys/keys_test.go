package keys

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeKeys(t *testing.T, path string, entries []Entry) {
	t.Helper()
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLookupByHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	writeKeys(t, path, []Entry{{Name: "a", SHA256: Hash("tok-a"), Scopes: []string{ScopeIngest}, Tenants: []string{"acme"}}})
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	p := s.Lookup("tok-a")
	if p == nil || p.Name != "a" {
		t.Fatalf("Lookup(tok-a) = %+v", p)
	}
	if s.Lookup("tok-b") != nil || s.Lookup("") != nil {
		t.Fatal("unknown or empty token must not resolve")
	}
	if tn, ok := p.SingleTenant(); !ok || tn != "acme" {
		t.Fatalf("SingleTenant = %q %v", tn, ok)
	}
	if !p.Has(ScopeIngest) || p.Has(ScopeRead) {
		t.Fatal("scopes not honoured")
	}
}

func TestParseRejectsBadFiles(t *testing.T) {
	good := Hash("x")
	cases := map[string]string{
		"not json":       `{`,
		"no name":        `[{"sha256":"` + good + `","scopes":["read"],"tenants":["a"]}]`,
		"bad hash":       `[{"name":"a","sha256":"abc","scopes":["read"],"tenants":["a"]}]`,
		"unknown scope":  `[{"name":"a","sha256":"` + good + `","scopes":["admin"],"tenants":["a"]}]`,
		"no scopes":      `[{"name":"a","sha256":"` + good + `","tenants":["a"]}]`,
		"read no tenant": `[{"name":"a","sha256":"` + good + `","scopes":["read"]}]`,
		"empty tenant":   `[{"name":"a","sha256":"` + good + `","scopes":["read"],"tenants":[""]}]`,
		"dup name":       `[{"name":"a","sha256":"` + good + `","scopes":["flush"]},{"name":"a","sha256":"` + Hash("y") + `","scopes":["flush"]}]`,
		"dup hash":       `[{"name":"a","sha256":"` + good + `","scopes":["flush"]},{"name":"b","sha256":"` + good + `","scopes":["flush"]}]`,
	}
	for name, data := range cases {
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A flush-only key needs no tenant.
	if _, err := Parse([]byte(`[{"name":"f","sha256":"` + good + `","scopes":["flush"]}]`)); err != nil {
		t.Errorf("flush-only key rejected: %v", err)
	}
}

func TestOpenFailsOnMissingOrBadFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(filepath.Join(dir, "nope.json")); err == nil {
		t.Error("missing file accepted at startup")
	}
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte("{"), 0o644)
	if _, err := Open(bad); err == nil {
		t.Error("bad file accepted at startup")
	}
}

func TestReloadPicksUpChangesAndKeepsLastGoodList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	writeKeys(t, path, []Entry{{Name: "a", SHA256: Hash("tok-a"), Scopes: []string{ScopeRead}, Tenants: []string{"*"}}})
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	// Revoke a, add b. Bump mtime explicitly: a fast test can rewrite within
	// the filesystem's timestamp granularity.
	writeKeys(t, path, []Entry{{Name: "b", SHA256: Hash("tok-b"), Scopes: []string{ScopeRead}, Tenants: []string{"*"}}})
	future := time.Now().Add(time.Minute)
	os.Chtimes(path, future, future)
	if err := s.reload(false); err != nil {
		t.Fatal(err)
	}
	if s.Lookup("tok-a") != nil || s.Lookup("tok-b") == nil {
		t.Fatal("reload did not revoke a / add b")
	}

	// A broken edit keeps b working.
	os.WriteFile(path, []byte("{broken"), 0o644)
	later := future.Add(time.Minute)
	os.Chtimes(path, later, later)
	if err := s.reload(false); err == nil {
		t.Fatal("broken file reloaded without error")
	}
	if s.Lookup("tok-b") == nil {
		t.Fatal("a broken edit dropped the previous keys")
	}
}

func TestWriteNewNeverOverwritesAndIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.token")
	hash, err := WriteNew(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	tok := strings.TrimSpace(string(data))
	if !strings.HasPrefix(tok, TokenPrefix) || len(tok) < 40 {
		t.Errorf("token %q does not look like a ducktel token", tok[:3])
	}
	if Hash(tok) != hash {
		t.Error("returned hash does not match the token on disk")
	}
	if got, _ := HashFile(path); got != hash {
		t.Error("HashFile disagrees with WriteNew")
	}
	if _, err := WriteNew(path); !errors.Is(err, ErrExists) {
		t.Errorf("second WriteNew: got %v, want ErrExists", err)
	}
}

func TestPrincipalTenants(t *testing.T) {
	all := &Principal{Name: "all", Scopes: []string{ScopeRead}, Tenants: []string{"*"}}
	if _, ok := all.SingleTenant(); ok {
		t.Error("* is not a single tenant")
	}
	if !all.AllowsTenant("anything") {
		t.Error("* must allow every tenant")
	}
	two := &Principal{Name: "two", Tenants: []string{"a", "b"}}
	if _, ok := two.SingleTenant(); ok || !two.AllowsTenant("b") || two.AllowsTenant("c") {
		t.Error("two-tenant key misbehaves")
	}
	if !LegacyPrincipal.Has(ScopeFlush) || !LegacyPrincipal.AllowsTenant("x") {
		t.Error("legacy principal must stay unrestricted")
	}
}
