package webapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/telemetry"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

func seedSpan(svc, tenant, traceID string) writer.TraceSpan {
	now := time.Now()
	return writer.TraceSpan{
		TraceID: traceID, SpanID: "s-" + svc, ParentSpanID: "",
		ServiceName: svc, SpanName: "op", SpanKind: "SPAN_KIND_SERVER",
		StartTime: now.UnixMicro(), EndTime: now.UnixMicro(), DurationMs: 5,
		StatusCode: "STATUS_CODE_OK", Attributes: "{}",
		ResourceAttributes: `{"service.name":"` + svc + `","tenant.id":"` + tenant + `"}`,
		Events:             "[]", Links: "[]",
	}
}

// newTestMux seeds a store and returns a mux with /api/* registered, guarded
// by the given token (empty disables auth, matching httpauth.Bearer).
func newTestMux(t *testing.T, token string) *http.ServeMux {
	t.Helper()
	dir := t.TempDir()

	w := writer.New(dir, time.Hour, 1000)
	w.Add([]writer.TraceSpan{
		seedSpan("api", "acme", "trace-acme-1"),
		seedSpan("worker", "acme", "trace-acme-2"),
		seedSpan("api", "globex", "trace-globex-1"),
	})
	w.AddMetrics([]writer.MetricPoint{
		{MetricName: "cpu.util", MetricType: "gauge", Timestamp: time.Now().UnixMicro(),
			ValueDouble: 0.5, ResourceAttributes: `{"tenant.id":"acme"}`},
		{MetricName: "cpu.util", MetricType: "gauge", Timestamp: time.Now().UnixMicro(),
			ValueDouble: 0.9, ResourceAttributes: `{"tenant.id":"globex"}`},
	})
	if err := w.Flush(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	engine, err := query.Open(dir)
	if err != nil {
		t.Fatalf("opening engine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })

	mux := http.NewServeMux()
	NewHandlers(telemetry.NewCore(engine), telemetry.NewFlusher("", ""), token).Register(mux)
	return mux
}

func doReq(t *testing.T, mux *http.ServeMux, method, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// --- auth ---

func TestAuthRejectsMissingToken(t *testing.T) {
	mux := newTestMux(t, "secret")

	rec := doReq(t, mux, "GET", "/api/traces?tenant=acme", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAuthRejectsWrongToken(t *testing.T) {
	mux := newTestMux(t, "secret")

	rec := doReq(t, mux, "GET", "/api/traces?tenant=acme", "wrong")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAuthAcceptsCorrectToken(t *testing.T) {
	mux := newTestMux(t, "secret")

	rec := doReq(t, mux, "GET", "/api/traces?tenant=acme", "secret")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestAuthDisabledWhenNoToken(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/traces?tenant=acme", "")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 with auth disabled: %s", rec.Code, rec.Body.String())
	}
}

// --- input validation -> 400 ---

func TestMissingTenantIs400(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/traces", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestBadFilterFormatIs400(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/traces?tenant=acme&filter=noColon", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestUnknownAggregationIs400(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/metrics?tenant=acme&metric_name=cpu&aggregation=median", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// --- tenant isolation through the HTTP layer ---

func TestTenantIsolationThroughHTTP(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/traces?tenant=acme", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp rowsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(resp.Rows) != 2 {
		t.Errorf("acme got %d rows, want 2 (globex must not leak)", len(resp.Rows))
	}

	rec = doReq(t, mux, "GET", "/api/traces/trace-globex-1?tenant=acme", "")
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Rows) != 0 {
		t.Errorf("acme could read globex's trace via /api/traces/{id}: %d rows", len(resp.Rows))
	}
}

func TestListServicesScopedToTenant(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/services?tenant=acme", "")
	var resp rowsResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Rows) != 2 {
		t.Errorf("acme services = %d, want 2 (api, worker)", len(resp.Rows))
	}
}

func TestListMetricNamesScopedToTenant(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/metric-names?tenant=acme", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp rowsResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Rows) != 1 || resp.Rows[0]["metric_name"] != "cpu.util" {
		t.Errorf("acme metric names = %v, want exactly [cpu.util]", resp.Rows)
	}
}

func TestListMetricNamesMissingTenantIs400(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/metric-names", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestListTenantsReturnsBoth(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/tenants", "")
	var resp rowsResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	got := map[string]bool{}
	for _, r := range resp.Rows {
		got[r["tenant"].(string)] = true
	}
	if !got["acme"] || !got["globex"] {
		t.Errorf("tenants = %v, want acme and globex", resp.Rows)
	}
}

// --- flush ---

func TestFlushNotConfiguredIs404(t *testing.T) {
	mux := newTestMux(t, "") // built with an empty (disabled) Flusher

	rec := doReq(t, mux, "POST", "/api/flush", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when flush is not configured", rec.Code)
	}
}

func TestConfigReportsFlushDisabled(t *testing.T) {
	mux := newTestMux(t, "")

	rec := doReq(t, mux, "GET", "/api/config", "")
	var cfg map[string]bool
	json.Unmarshal(rec.Body.Bytes(), &cfg)
	if cfg["flush_enabled"] {
		t.Error("flush_enabled should be false when no Flusher URL is configured")
	}
}
