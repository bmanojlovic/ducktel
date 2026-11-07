package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidgeorgehope/ducktel/internal/keys"
	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// headerTransport adds an Authorization header to every request, as an MCP
// client configured with a bearer token does.
type headerTransport struct{ token string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+h.token)
	return http.DefaultTransport.RoundTrip(r)
}

// newHTTPSession serves the MCP server over real Streamable HTTP, so the
// tool calls carry the request headers the key resolution reads.
func newHTTPSession(t *testing.T, token string) *sdkmcp.ClientSession {
	t.Helper()
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	w.Add([]writer.TraceSpan{
		seedSpan("api", "acme", "trace-acme-1"),
		seedSpan("api", "globex", "trace-globex-1"),
	})
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	engine, err := query.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })

	principals := map[string]*keys.Principal{
		"acme-key": {Name: "acme-reader", Scopes: []string{keys.ScopeRead}, Tenants: []string{"acme"}},
	}
	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server {
		srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "ducktel-test", Version: "test"}, nil)
		s := NewServer(engine, "http://unused/flush", "")
		s.SetPrincipalFunc(func(h http.Header) *keys.Principal {
			tok := strings.TrimPrefix(h.Get("Authorization"), "Bearer ")
			if p, ok := principals[tok]; ok {
				return p
			}
			return keys.LegacyPrincipal
		})
		s.Register(srv)
		return srv
	}, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(context.Background(), &sdkmcp.StreamableClientTransport{
		Endpoint:   ts.URL,
		HTTPClient: &http.Client{Transport: headerTransport{token: token}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func TestKeyTenantEnforcedOnTools(t *testing.T) {
	s := newHTTPSession(t, "acme-key")

	// Tenant omitted: the key's single tenant is used.
	body, isErr := call(t, s, "span_search", map[string]any{})
	if isErr || countRows(t, body) != 1 {
		t.Fatalf("default tenant: err=%v body=%s", isErr, body)
	}
	// Another tenant: refused.
	body, isErr = call(t, s, "span_search", map[string]any{"tenant": "globex"})
	if !isErr || !strings.Contains(body, "not allowed") {
		t.Errorf("foreign tenant: err=%v body=%s", isErr, body)
	}
	body, isErr = call(t, s, "trace_lookup", map[string]any{"trace_id": "trace-globex-1", "tenant": "globex"})
	if !isErr {
		t.Errorf("trace_lookup on foreign tenant succeeded: %s", body)
	}
	// No flush scope.
	body, isErr = call(t, s, "flush_buffer", map[string]any{})
	if !isErr || !strings.Contains(body, "flush scope") {
		t.Errorf("flush without scope: err=%v body=%s", isErr, body)
	}
}

func TestSharedTokenStillNeedsTenant(t *testing.T) {
	s := newHTTPSession(t, "shared")
	if _, isErr := call(t, s, "span_search", map[string]any{}); !isErr {
		t.Error("shared token without tenant must still be rejected")
	}
	body, isErr := call(t, s, "span_search", map[string]any{"tenant": "globex"})
	if isErr || countRows(t, body) != 1 {
		t.Errorf("shared token on globex: err=%v body=%s", isErr, body)
	}
}
