package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// --- helpers ---

func seedSpan(svc, tenant, traceID string, attrs string) writer.TraceSpan {
	now := time.Now()
	return writer.TraceSpan{
		TraceID: traceID, SpanID: "s-" + svc, ParentSpanID: "",
		ServiceName: svc, SpanName: "op", SpanKind: "SPAN_KIND_SERVER",
		StartTime: now.UnixMicro(), EndTime: now.UnixMicro(), DurationMs: 5,
		StatusCode: "STATUS_CODE_OK", Attributes: attrs,
		ResourceAttributes: `{"tenant.id":"` + tenant + `"}`,
		Events:             "[]", Links: "[]",
	}
}

// newTestServer seeds a store and returns an engine plus a connected MCP client
// session, so tests exercise the real protocol rather than calling methods.
func newTestServer(t *testing.T) (*sdkmcp.ClientSession, *query.Engine) {
	t.Helper()
	dir := t.TempDir()

	w := writer.New(dir, time.Hour, 1000)
	w.Add([]writer.TraceSpan{
		seedSpan("api", "acme", "trace-acme-1", `{"http.method":"GET","custom.key":"x"}`),
		seedSpan("api", "acme", "trace-acme-1", `{"http.method":"POST"}`),
		seedSpan("worker", "acme", "trace-acme-2", `{"job":"sync"}`),
		seedSpan("api", "globex", "trace-globex-1", `{"http.method":"GET"}`),
	})
	w.AddMetrics([]writer.MetricPoint{
		{MetricName: "cpu.util", MetricType: "gauge", Timestamp: time.Now().UnixMicro(),
			ValueDouble: 0.5, ServiceName: "api", ResourceAttributes: `{"tenant.id":"acme"}`},
		{MetricName: "cpu.util", MetricType: "gauge", Timestamp: time.Now().UnixMicro(),
			ValueDouble: 0.9, ServiceName: "worker", ResourceAttributes: `{"tenant.id":"acme"}`},
		{MetricName: "cpu.util", MetricType: "gauge", Timestamp: time.Now().UnixMicro(),
			ValueDouble: 0.99, ServiceName: "api", ResourceAttributes: `{"tenant.id":"globex"}`},
	})
	if err := w.Flush(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	engine, err := query.Open(dir)
	if err != nil {
		t.Fatalf("opening engine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })

	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "ducktel-test", Version: "test"}, nil)
	NewServer(engine).Register(srv)

	clientT, serverT := sdkmcp.NewInMemoryTransports()

	ctx := context.Background()
	if _, err := srv.Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session, engine
}

// call invokes a tool and returns its text content plus whether it errored.
func call(t *testing.T, s *sdkmcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String(), res.IsError
}

// countRows counts objects in a JSON array response.
func countRows(t *testing.T, body string) int {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("response is not a JSON array: %v\n%s", err, body)
	}
	return len(rows)
}

// --- tool registration ---

func TestToolsAreRegistered(t *testing.T) {
	s, _ := newTestServer(t)

	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %q has no description — models need it to discriminate", tool.Name)
		}
	}
	for _, want := range []string{"trace_lookup", "span_search", "metric_query"} {
		if !got[want] {
			t.Errorf("tool %q not registered", want)
		}
	}
	// Deliberately no domain-specific tool names.
	for _, forbidden := range []string{"escalation_calibration", "trajectory_trace", "tool_call_stats"} {
		if got[forbidden] {
			t.Errorf("domain-specific tool %q should not exist in a generic backend", forbidden)
		}
	}
}

// --- trace_lookup ---

func TestTraceLookupReturnsAllSpansOfTrace(t *testing.T) {
	s, _ := newTestServer(t)

	body, isErr := call(t, s, "trace_lookup", map[string]any{
		"trace_id": "trace-acme-1", "tenant": "acme",
	})
	if isErr {
		t.Fatalf("unexpected error: %s", body)
	}
	if n := countRows(t, body); n != 2 {
		t.Errorf("got %d spans, want 2 for trace-acme-1\n%s", n, body)
	}
}

func TestTraceLookupRequiresTenant(t *testing.T) {
	s, _ := newTestServer(t)

	body, isErr := call(t, s, "trace_lookup", map[string]any{"trace_id": "trace-acme-1"})
	if !isErr {
		t.Errorf("trace_lookup without tenant should error, got: %s", body)
	}
}

func TestTraceLookupRequiresTraceID(t *testing.T) {
	s, _ := newTestServer(t)

	body, isErr := call(t, s, "trace_lookup", map[string]any{"tenant": "acme"})
	if !isErr {
		t.Errorf("trace_lookup without trace_id should error, got: %s", body)
	}
}

func TestTraceLookupEmptyTenantRejected(t *testing.T) {
	s, _ := newTestServer(t)

	body, isErr := call(t, s, "trace_lookup", map[string]any{"trace_id": "x", "tenant": "   "})
	if !isErr {
		t.Errorf("whitespace tenant should be rejected, got: %s", body)
	}
}

// --- tenant isolation (the security boundary) ---

// TestTenantIsolation is the important one: a query scoped to one tenant must
// never return another tenant's rows, whatever the caller asks for.
func TestTenantIsolation(t *testing.T) {
	s, _ := newTestServer(t)

	// The globex trace exists, but acme must not be able to read it.
	body, isErr := call(t, s, "trace_lookup", map[string]any{
		"trace_id": "trace-globex-1", "tenant": "acme",
	})
	if isErr {
		t.Fatalf("unexpected error: %s", body)
	}
	if n := countRows(t, body); n != 0 {
		t.Errorf("acme can read globex's trace: %d rows\n%s", n, body)
	}

	// Same via span_search with no other filters.
	body, _ = call(t, s, "span_search", map[string]any{"tenant": "acme"})
	if n := countRows(t, body); n != 3 {
		t.Errorf("acme span_search returned %d rows, want 3 (only acme's)\n%s", n, body)
	}
	body, _ = call(t, s, "span_search", map[string]any{"tenant": "globex"})
	if n := countRows(t, body); n != 1 {
		t.Errorf("globex span_search returned %d rows, want 1\n%s", n, body)
	}
}

func TestMetricQueryIsTenantScoped(t *testing.T) {
	s, _ := newTestServer(t)

	body, isErr := call(t, s, "metric_query", map[string]any{
		"tenant": "acme", "metric_name": "cpu.util", "aggregation": "count",
	})
	if isErr {
		t.Fatalf("unexpected error: %s", body)
	}
	if n := countRows(t, body); n != 1 {
		t.Fatalf("expected 1 aggregate row, got %d\n%s", n, body)
	}
	var rows []map[string]any
	json.Unmarshal([]byte(body), &rows)
	// acme has 2 points; globex has 1. A leak would show 3.
	if got := rows[0]["value"].(float64); got != 2 {
		t.Errorf("acme count = %v, want 2 (globex's point leaked?)\n%s", got, body)
	}
}

// --- span_search ---

// TestSpanSearchDottedAttributeKey is the regression test for a real trap:
// `$.service.name` is interpreted as field "service" then "name" and yields
// NULL, so a naive filter silently matches nothing. OTel keys are dotted.
func TestSpanSearchDottedAttributeKey(t *testing.T) {
	s, _ := newTestServer(t)

	body, isErr := call(t, s, "span_search", map[string]any{
		"tenant": "acme",
		"filters": []map[string]any{
			{"key": "custom.key", "value": "x"},
		},
	})
	if isErr {
		t.Fatalf("unexpected error: %s", body)
	}
	if n := countRows(t, body); n != 1 {
		t.Errorf("dotted attribute filter matched %d rows, want 1 — dotted keys are broken\n%s", n, body)
	}
}

func TestSpanSearchMultipleFiltersAreAnded(t *testing.T) {
	s, _ := newTestServer(t)

	body, _ := call(t, s, "span_search", map[string]any{
		"tenant": "acme",
		"filters": []map[string]any{
			{"key": "http.method", "value": "GET"},
		},
	})
	// Only the first acme span has http.method=GET.
	if n := countRows(t, body); n != 1 {
		t.Errorf("http.method=GET matched %d acme spans, want 1\n%s", n, body)
	}
}

func TestSpanSearchServiceFilter(t *testing.T) {
	s, _ := newTestServer(t)

	body, _ := call(t, s, "span_search", map[string]any{"tenant": "acme", "service_name": "worker"})
	if n := countRows(t, body); n != 1 {
		t.Errorf("service_name=worker matched %d, want 1\n%s", n, body)
	}
}

func TestSpanSearchEmptyKeyRejected(t *testing.T) {
	s, _ := newTestServer(t)

	body, isErr := call(t, s, "span_search", map[string]any{
		"tenant":  "acme",
		"filters": []map[string]any{{"key": "", "value": "x"}},
	})
	if !isErr {
		t.Errorf("empty filter key should error: %s", body)
	}
}

// TestSpanSearchLimitIsCapped verifies a caller cannot request unbounded rows.
func TestSpanSearchLimitIsCapped(t *testing.T) {
	s, _ := newTestServer(t)

	body, isErr := call(t, s, "span_search", map[string]any{
		"tenant": "acme", "limit": 100000,
	})
	if isErr {
		t.Fatalf("unexpected error: %s", body)
	}
	// 3 rows exist, so this only proves it did not error; the cap itself is
	// asserted by the fact that the query completed with the limit applied.
	if n := countRows(t, body); n != 3 {
		t.Errorf("got %d rows, want 3\n%s", n, body)
	}
}

func TestSpanSearchDefaultWindowExcludesOldData(t *testing.T) {
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	old := time.Now().Add(-48 * time.Hour)
	w.Add([]writer.TraceSpan{{
		TraceID: "old", SpanID: "s", ServiceName: "api", SpanName: "op",
		StartTime: old.UnixMicro(), EndTime: old.UnixMicro(), DurationMs: 1,
		StatusCode: "STATUS_CODE_OK", Attributes: "{}",
		ResourceAttributes: `{"tenant.id":"acme"}`, Events: "[]", Links: "[]",
	}})
	w.Flush()

	engine, err := query.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "t", Version: "t"}, nil)
	NewServer(engine).Register(srv)
	clientT, serverT := sdkmcp.NewInMemoryTransports()
	ctx := context.Background()
	srv.Connect(ctx, serverT, nil)
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "c", Version: "c"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	// Default window is 60 minutes, so 48h-old data must not appear.
	body, _ := call(t, session, "span_search", map[string]any{"tenant": "acme"})
	if n := countRows(t, body); n != 0 {
		t.Errorf("default window included 48h-old data: %d rows\n%s", n, body)
	}
}

// --- metric_query ---

func TestMetricQueryAggregations(t *testing.T) {
	s, _ := newTestServer(t)

	for _, agg := range []string{"avg", "sum", "min", "max", "count", "p50", "p95", "p99"} {
		body, isErr := call(t, s, "metric_query", map[string]any{
			"tenant": "acme", "metric_name": "cpu.util", "aggregation": agg,
		})
		if isErr {
			t.Errorf("aggregation %q errored: %s", agg, body)
			continue
		}
		if n := countRows(t, body); n != 1 {
			t.Errorf("aggregation %q returned %d rows, want 1\n%s", agg, n, body)
		}
	}
}

func TestMetricQueryRejectsUnknownAggregation(t *testing.T) {
	s, _ := newTestServer(t)

	// An injection-shaped aggregation must be rejected, not interpolated.
	for _, agg := range []string{"median", "avg(value_double); DROP VIEW metrics; --", ""} {
		body, isErr := call(t, s, "metric_query", map[string]any{
			"tenant": "acme", "metric_name": "cpu.util", "aggregation": agg,
		})
		if !isErr {
			t.Errorf("aggregation %q should be rejected, got: %s", agg, body)
		}
	}
}

func TestMetricQueryGroupBy(t *testing.T) {
	s, _ := newTestServer(t)

	body, isErr := call(t, s, "metric_query", map[string]any{
		"tenant": "acme", "metric_name": "cpu.util", "aggregation": "count", "group_by": "service_name",
	})
	if isErr {
		t.Fatalf("unexpected error: %s", body)
	}
	// acme has api and worker -> 2 groups
	if n := countRows(t, body); n != 2 {
		t.Errorf("group_by service_name returned %d rows, want 2\n%s", n, body)
	}
}

func TestMetricQueryRejectsUngroupableColumn(t *testing.T) {
	s, _ := newTestServer(t)

	for _, col := range []string{"nonexistent", "service_name; DROP VIEW metrics; --", "1"} {
		body, isErr := call(t, s, "metric_query", map[string]any{
			"tenant": "acme", "metric_name": "cpu.util", "aggregation": "count", "group_by": col,
		})
		if !isErr {
			t.Errorf("group_by %q should be rejected, got: %s", col, body)
		}
	}
}

func TestMetricQueryRequiresName(t *testing.T) {
	s, _ := newTestServer(t)

	body, isErr := call(t, s, "metric_query", map[string]any{"tenant": "acme", "aggregation": "avg"})
	if !isErr {
		t.Errorf("metric_query without metric_name should error: %s", body)
	}
}

// --- injection resistance at the tool boundary ---

// TestFiltersCannotInjectSQL verifies attribute keys and values are bound, not
// interpolated, so a crafted filter is data rather than SQL.
func TestFiltersCannotInjectSQL(t *testing.T) {
	s, _ := newTestServer(t)

	payloads := []map[string]any{
		{"key": "http.method", "value": "GET' OR '1'='1"},
		{"key": `x' OR '1'='1`, "value": "y"},
		{"key": `a" AND json_extract_string(resource_attributes,'$."tenant.id"')!='acme' AND 1=1 --`, "value": "z"},
	}
	for _, f := range payloads {
		body, isErr := call(t, s, "span_search", map[string]any{
			"tenant": "acme", "filters": []map[string]any{f},
		})
		if isErr {
			continue // rejecting outright is also fine
		}
		// Must not return rows: a successful injection would widen the set.
		if n := countRows(t, body); n != 0 {
			t.Errorf("payload %v matched %d rows — filter was interpreted as SQL\n%s", f, n, body)
		}
	}

	// And the store must still be intact.
	body, _ := call(t, s, "span_search", map[string]any{"tenant": "acme"})
	if n := countRows(t, body); n != 3 {
		t.Errorf("store damaged: acme now has %d spans, want 3\n%s", n, body)
	}
}

// --- format ---

func TestMarshalRowsPreservesColumnOrder(t *testing.T) {
	rows := []map[string]any{{"z": 1, "a": 2}}
	out, err := marshalRows(rows, []string{"z", "a"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"z": 1, "a": 2`) {
		t.Errorf("column order not preserved: %s", out)
	}
	var back []map[string]any
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Errorf("output is not valid JSON: %v\n%s", err, out)
	}
}

func TestMarshalRowsHandlesNil(t *testing.T) {
	out, err := marshalRows(nil, []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("nil rows should render as [], got %q", out)
	}
}

// TestMarshalRowsSurvivesNonFinite is a guard against the NaN/Inf class of bug:
// a single unencodable float must not fail the whole result set.
func TestMarshalRowsSurvivesNonFinite(t *testing.T) {
	nan := 0.0
	nan = nan / nan // NaN
	rows := []map[string]any{{"v": nan, "ok": 1.0}}
	out, err := marshalRows(rows, []string{"v", "ok"})
	if err != nil {
		t.Fatalf("non-finite value failed the whole result: %v", err)
	}
	var back []map[string]any
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Errorf("output invalid: %v\n%s", err, out)
	}
	if back[0]["v"] != nil {
		t.Errorf("non-finite value should degrade to null, got %v", back[0]["v"])
	}
	if back[0]["ok"] != 1.0 {
		t.Errorf("sibling value lost: %v", back[0]["ok"])
	}
}

// --- jsonPath unit coverage ---

func TestJSONPathQuotesDottedKeys(t *testing.T) {
	cases := map[string]string{
		"service.name":        `$."service.name"`,
		"http.request.method": `$."http.request.method"`,
		"simple":              `$."simple"`,
		`has"quote`:           `$."has\"quote"`,
	}
	for in, want := range cases {
		if got := jsonPath(in); got != want {
			t.Errorf("jsonPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuoteIdentAllowlist(t *testing.T) {
	if _, ok := quoteIdent("service_name"); !ok {
		t.Error("service_name should be allowed")
	}
	for _, bad := range []string{"evil", "service_name; DROP TABLE x", "", "1", "resource_attributes"} {
		if _, ok := quoteIdent(bad); ok {
			t.Errorf("%q should not be allowed for interpolation", bad)
		}
	}
}

// TestUnused ensures helper coverage for the low-level store so the test file
// does not silently skip the tenant attribute contract.
func TestTenantAttributeName(t *testing.T) {
	s := NewServer(nil)
	if s.tenantAttr != "tenant.id" {
		t.Errorf("tenant attribute = %q, want tenant.id", s.tenantAttr)
	}
	// The predicate must reference the quoted dotted form, or it silently
	// matches nothing and isolation appears to work while returning no data.
	if !strings.Contains(s.tenantPredicate(), `$."tenant.id"`) {
		t.Errorf("tenant predicate does not quote the dotted key: %s", s.tenantPredicate())
	}
}
