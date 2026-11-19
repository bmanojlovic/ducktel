package receiver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/davidgeorgehope/ducktel/internal/keys"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

type captureConsumer struct {
	mu      sync.Mutex
	spans   []writer.TraceSpan
	logs    []writer.LogRecord
	metrics []writer.MetricPoint
}

func (c *captureConsumer) Add(s []writer.TraceSpan) {
	c.mu.Lock()
	c.spans = append(c.spans, s...)
	c.mu.Unlock()
}
func (c *captureConsumer) AddLogs(r []writer.LogRecord) {
	c.mu.Lock()
	c.logs = append(c.logs, r...)
	c.mu.Unlock()
}
func (c *captureConsumer) AddMetrics(p []writer.MetricPoint) {
	c.mu.Lock()
	c.metrics = append(c.metrics, p...)
	c.mu.Unlock()
}

func keyStore(t *testing.T, entries ...keys.Entry) *keys.Store {
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

func newKeyedReceiver(t *testing.T) (*Receiver, *captureConsumer) {
	t.Helper()
	c := &captureConsumer{}
	r := New("localhost", 0, c, "shared", "flush-secret", func() error { return nil })
	r.SetKeys(keyStore(t,
		keys.Entry{Name: "bla-otel", SHA256: keys.Hash("bla-key"), Scopes: []string{keys.ScopeIngest}, Tenants: []string{"bla"}},
		keys.Entry{Name: "multi", SHA256: keys.Hash("multi-key"), Scopes: []string{keys.ScopeIngest}, Tenants: []string{"a", "b"}},
		keys.Entry{Name: "reader", SHA256: keys.Hash("read-key"), Scopes: []string{keys.ScopeRead}, Tenants: []string{"bla"}},
		keys.Entry{Name: "flusher", SHA256: keys.Hash("flush-key"), Scopes: []string{keys.ScopeFlush}},
	))
	return r, c
}

func post(t *testing.T, r *Receiver, path, token, body string) int {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	r.server.Handler.ServeHTTP(rec, req)
	return rec.Code
}

func resourceJSON(tenant string) string {
	attrs := `{"key":"service.name","value":{"stringValue":"svc"}}`
	if tenant != "" {
		attrs += `,{"key":"tenant.id","value":{"stringValue":"` + tenant + `"}}`
	}
	return `{"attributes":[` + attrs + `]}`
}

func traceBody(tenant string) string {
	return `{"resourceSpans":[{"resource":` + resourceJSON(tenant) + `,"scopeSpans":[{"spans":[{"traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"0102030405060708","name":"op","startTimeUnixNano":"1","endTimeUnixNano":"2"}]}]}]}`
}

func tenantOf(t *testing.T, resourceAttrs string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(resourceAttrs), &m); err != nil {
		t.Fatalf("resource attrs %q: %v", resourceAttrs, err)
	}
	s, _ := m["tenant.id"].(string)
	return s
}

func TestIngestKeyStampsTenant(t *testing.T) {
	cases := []struct {
		name, token, sent, want string
	}{
		{"key, sender sets nothing", "bla-key", "", "bla"},
		{"key overrides a foreign tenant", "bla-key", "evil", "bla"},
		{"shared token keeps sender's tenant", "shared", "acme", "acme"},
		{"multi-tenant key keeps sender's tenant", "multi-key", "b", "b"},
	}
	for _, c := range cases {
		r, cons := newKeyedReceiver(t)
		if code := post(t, r, "/v1/traces", c.token, traceBody(c.sent)); code != http.StatusOK {
			t.Fatalf("%s: status %d", c.name, code)
		}
		if len(cons.spans) != 1 {
			t.Fatalf("%s: %d spans", c.name, len(cons.spans))
		}
		if got := tenantOf(t, cons.spans[0].ResourceAttributes); got != c.want {
			t.Errorf("%s: tenant.id = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestIngestKeyStampsLogsAndMetrics(t *testing.T) {
	r, cons := newKeyedReceiver(t)
	logs := `{"resourceLogs":[{"resource":` + resourceJSON("evil") + `,"scopeLogs":[{"logRecords":[{"timeUnixNano":"1","body":{"stringValue":"hi"}}]}]}]}`
	metrics := `{"resourceMetrics":[{"resource":{},"scopeMetrics":[{"metrics":[{"name":"m","gauge":{"dataPoints":[{"timeUnixNano":"1","asDouble":1}]}}]}]}]}`
	if code := post(t, r, "/v1/logs", "bla-key", logs); code != http.StatusOK {
		t.Fatalf("logs: %d", code)
	}
	if code := post(t, r, "/v1/metrics", "bla-key", metrics); code != http.StatusOK {
		t.Fatalf("metrics: %d", code)
	}
	if len(cons.logs) != 1 || tenantOf(t, cons.logs[0].ResourceAttributes) != "bla" {
		t.Errorf("logs not stamped: %+v", cons.logs)
	}
	if len(cons.metrics) != 1 || tenantOf(t, cons.metrics[0].ResourceAttributes) != "bla" {
		t.Errorf("metrics not stamped (resource had no attributes): %+v", cons.metrics)
	}
}

func TestIngestScopes(t *testing.T) {
	r, cons := newKeyedReceiver(t)
	if code := post(t, r, "/v1/traces", "read-key", traceBody("")); code != http.StatusUnauthorized {
		t.Errorf("read key ingested: %d", code)
	}
	if code := post(t, r, "/v1/traces", "nope", traceBody("")); code != http.StatusUnauthorized {
		t.Errorf("unknown token ingested: %d", code)
	}
	if len(cons.spans) != 0 {
		t.Error("rejected requests reached the consumer")
	}
	if code := post(t, r, "/flush", "flush-key", ""); code != http.StatusOK {
		t.Errorf("flush key on /flush: %d", code)
	}
	if code := post(t, r, "/flush", "flush-secret", ""); code != http.StatusOK {
		t.Errorf("legacy flush token on /flush: %d", code)
	}
	if code := post(t, r, "/flush", "bla-key", ""); code != http.StatusUnauthorized {
		t.Errorf("ingest key on /flush: %d", code)
	}
	if code := post(t, r, "/flush", "shared", ""); code != http.StatusUnauthorized {
		t.Errorf("ingest token on /flush: %d (must stay a separate secret)", code)
	}
}
