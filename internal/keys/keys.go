// Package keys holds ducktel's per-client keys.
//
// A key is a random bearer token. ducktel never stores the token itself, only
// its SHA-256, so the key list can live in a ConfigMap, in Terraform or in git
// without being a secret. The token is created where it is used (by Terraform
// straight into the consumer's Secret, or by `ducktel key new` into a local
// file) and never has to travel.
//
// Each key carries the scopes it grants (ingest, read, flush) and the tenants
// it is bound to. The tenant comes from the key: an ingest key with one tenant
// stamps tenant.id on everything it sends, and a read key only reads its own
// tenants, so senders never set the tenant themselves.
package keys

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// Scopes a key can grant.
const (
	ScopeIngest = "ingest"
	ScopeRead   = "read"
	ScopeFlush  = "flush"
)

// AllTenants in a key's tenant list grants every tenant.
const AllTenants = "*"

// TokenPrefix marks ducktel tokens so secret scanners can recognise a leak.
const TokenPrefix = "dt_"

// Principal is who a request authenticated as.
//
// Legacy marks the old single shared tokens (DUCKTEL_AUTH_TOKEN,
// DUCKTEL_MCP_TOKEN, ...): they are unrestricted and keep today's behaviour,
// where the tenant comes from the sender's tenant.id attribute.
type Principal struct {
	Name    string
	Scopes  []string
	Tenants []string
	Legacy  bool
}

// LegacyPrincipal is the principal of a request authorised by a shared token.
var LegacyPrincipal = &Principal{Name: "legacy", Legacy: true}

// Has reports whether the principal grants scope. A legacy principal is
// checked against its own token by the caller, so it grants everything here.
func (p *Principal) Has(scope string) bool {
	return p.Legacy || slices.Contains(p.Scopes, scope)
}

// AllTenants reports whether the principal may see every tenant.
func (p *Principal) AllTenants() bool {
	return p.Legacy || slices.Contains(p.Tenants, AllTenants)
}

// SingleTenant returns the tenant a key is bound to when it is bound to
// exactly one, so it can be used as the default and stamped on ingest.
func (p *Principal) SingleTenant() (string, bool) {
	if p.Legacy || len(p.Tenants) != 1 || p.Tenants[0] == AllTenants {
		return "", false
	}
	return p.Tenants[0], true
}

// AllowsTenant reports whether the principal may read tenant.
func (p *Principal) AllowsTenant(tenant string) bool {
	return p.AllTenants() || slices.Contains(p.Tenants, tenant)
}

// Entry is one key in the keys file.
type Entry struct {
	Name    string   `json:"name"`
	SHA256  string   `json:"sha256"`
	Scopes  []string `json:"scopes"`
	Tenants []string `json:"tenants"`
}

// Hash returns the hex SHA-256 of a token, the form stored in the keys file.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewToken returns a fresh random token: the prefix plus 32 random bytes in
// lower-case base32 (no padding).
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	return TokenPrefix + strings.ToLower(enc), nil
}

// Parse reads and validates a keys file's contents.
func Parse(data []byte) (map[string]*Principal, error) {
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("keys file: %w", err)
	}
	out := make(map[string]*Principal, len(entries))
	names := make(map[string]bool, len(entries))
	for i, e := range entries {
		if e.Name == "" {
			return nil, fmt.Errorf("keys file: entry %d has no name", i)
		}
		if names[e.Name] {
			return nil, fmt.Errorf("keys file: duplicate name %q", e.Name)
		}
		names[e.Name] = true
		h := strings.ToLower(strings.TrimSpace(e.SHA256))
		if b, err := hex.DecodeString(h); err != nil || len(b) != sha256.Size {
			return nil, fmt.Errorf("keys file: %q: sha256 must be 64 hex characters", e.Name)
		}
		if _, dup := out[h]; dup {
			return nil, fmt.Errorf("keys file: %q: sha256 already used by another key", e.Name)
		}
		if len(e.Scopes) == 0 {
			return nil, fmt.Errorf("keys file: %q: no scopes", e.Name)
		}
		for _, s := range e.Scopes {
			if s != ScopeIngest && s != ScopeRead && s != ScopeFlush {
				return nil, fmt.Errorf("keys file: %q: unknown scope %q", e.Name, s)
			}
		}
		if len(e.Tenants) == 0 && (slices.Contains(e.Scopes, ScopeIngest) || slices.Contains(e.Scopes, ScopeRead)) {
			return nil, fmt.Errorf("keys file: %q: no tenants (use [\"*\"] for all)", e.Name)
		}
		if slices.Contains(e.Tenants, "") {
			return nil, fmt.Errorf("keys file: %q: empty tenant", e.Name)
		}
		out[h] = &Principal{Name: e.Name, Scopes: e.Scopes, Tenants: e.Tenants}
	}
	return out, nil
}

// Store is the live key list, reloaded when the file changes.
type Store struct {
	path string

	mu      sync.RWMutex
	keys    map[string]*Principal
	modTime time.Time
	size    int64
}

// Open loads the keys file. An unreadable or invalid file at startup is an
// error: starting with no keys would lock every client out silently.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	if err := s.reload(true); err != nil {
		return nil, err
	}
	return s, nil
}

// Len is the number of keys currently loaded.
func (s *Store) Len() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.keys)
}

// Lookup returns the principal for a presented token, or nil.
func (s *Store) Lookup(token string) *Principal {
	if s == nil || token == "" {
		return nil
	}
	h := Hash(token)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.keys[h]
}

// reload re-reads the file if its mtime or size changed (always when force).
func (s *Store) reload(force bool) error {
	// os.Stat follows symlinks, so a Kubernetes ConfigMap update (an atomic
	// swap of the ..data symlink) shows up as a new mtime here.
	fi, err := os.Stat(s.path)
	if err != nil {
		return fmt.Errorf("keys file: %w", err)
	}
	s.mu.RLock()
	same := fi.ModTime().Equal(s.modTime) && fi.Size() == s.size
	s.mu.RUnlock()
	if same && !force {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("keys file: %w", err)
	}
	keys, err := Parse(data)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.keys, s.modTime, s.size = keys, fi.ModTime(), fi.Size()
	s.mu.Unlock()
	if !force {
		log.Printf("keys: reloaded %s (%d keys)", s.path, len(keys))
	}
	return nil
}

// Watch reloads the file every interval until stop is closed. A file that
// fails to load keeps the last good list, so a bad edit cannot lock every
// client out; the error is logged.
func (s *Store) Watch(interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	var lastErr string
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			err := s.reload(false)
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			if msg != "" && msg != lastErr {
				log.Printf("keys: keeping previous list: %v", err)
			}
			lastErr = msg
		}
	}
}

// ErrExists is returned by WriteNew when the output file already exists.
var ErrExists = errors.New("token file already exists")

// WriteNew creates a new token in path (mode 0600, never overwriting) and
// returns only its hash. The token itself never leaves the file.
func WriteNew(path string) (hash string, err error) {
	tok, err := NewToken()
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("%s: %w", path, ErrExists)
		}
		return "", err
	}
	if _, err := f.WriteString(tok + "\n"); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return Hash(tok), nil
}

// HashFile returns the hash of the token stored in path (trailing whitespace
// ignored, as written by WriteNew or `openssl rand -hex 32 > file`).
func HashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("%s: empty token file", path)
	}
	return Hash(tok), nil
}
