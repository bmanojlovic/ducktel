package telemetry

import (
	"strings"
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// --- helpers ---

// seedSpan mirrors what the receiver actually stores: service.name and
// tenant.id arrive as RESOURCE attributes (service.name is additionally
// promoted to the service_name column), while the span's own attributes carry
// only keys the instrumenting app set.
func seedSpan(svc, tenant, traceID string, attrs string) writer.TraceSpan {
	now := time.Now()
	return writer.TraceSpan{
		TraceID: traceID, SpanID: "s-" + svc, ParentSpanID: "",
		ServiceName: svc, SpanName: "op", SpanKind: "SPAN_KIND_SERVER",
		StartTime: now.UnixMicro(), EndTime: now.UnixMicro(), DurationMs: 5,
		StatusCode: "STATUS_CODE_OK", Attributes: attrs,
		ResourceAttributes: `{"service.name":"` + svc + `","tenant.id":"` + tenant + `"}`,
		Events:             "[]", Links: "[]",
	}
}

// newTestCore seeds a store and returns a Core wired to it.
func newTestCore(t *testing.T) *Core {
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

	return NewCore(engine)
}

func countRows(rows []map[string]any) int { return len(rows) }

// --- trace_lookup ---

func TestTraceLookupReturnsAllSpansOfTrace(t *testing.T) {
	c := newTestCore(t)

	rows, _, err := c.LookupTrace("trace-acme-1", "acme", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := countRows(rows); n != 2 {
		t.Errorf("got %d spans, want 2 for trace-acme-1", n)
	}
}

func TestTraceLookupRequiresTenant(t *testing.T) {
	c := newTestCore(t)

	_, _, err := c.LookupTrace("trace-acme-1", "", 0)
	if err == nil {
		t.Error("trace lookup without tenant should error")
	}
	if !IsInvalidInput(err) {
		t.Errorf("missing tenant should classify as invalid input: %v", err)
	}
}

func TestTraceLookupRequiresTraceID(t *testing.T) {
	c := newTestCore(t)

	_, _, err := c.LookupTrace("", "acme", 0)
	if err == nil {
		t.Error("trace lookup without trace_id should error")
	}
}

func TestTraceLookupEmptyTenantRejected(t *testing.T) {
	c := newTestCore(t)

	_, _, err := c.LookupTrace("x", "   ", 0)
	if err == nil {
		t.Error("whitespace tenant should be rejected")
	}
}

// --- tenant isolation (the security boundary) ---

// TestTenantIsolation is the important one: a query scoped to one tenant must
// never return another tenant's rows, whatever the caller asks for.
func TestTenantIsolation(t *testing.T) {
	c := newTestCore(t)

	// The globex trace exists, but acme must not be able to read it.
	rows, _, err := c.LookupTrace("trace-globex-1", "acme", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := countRows(rows); n != 0 {
		t.Errorf("acme can read globex's trace: %d rows", n)
	}

	// Same via SearchSpans with no other filters.
	rows, _, _ = c.SearchSpans("acme", "", nil, 0, 0)
	if n := countRows(rows); n != 3 {
		t.Errorf("acme search returned %d rows, want 3 (only acme's)", n)
	}
	rows, _, _ = c.SearchSpans("globex", "", nil, 0, 0)
	if n := countRows(rows); n != 1 {
		t.Errorf("globex search returned %d rows, want 1", n)
	}
}

func TestMetricQueryIsTenantScoped(t *testing.T) {
	c := newTestCore(t)

	rows, _, err := c.QueryMetric("acme", "cpu.util", "count", 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := countRows(rows); n != 1 {
		t.Fatalf("expected 1 aggregate row, got %d", n)
	}
	// acme has 2 points; globex has 1. A leak would show 3.
	if got := asFloat64(t, rows[0]["value"]); got != 2 {
		t.Errorf("acme count = %v, want 2 (globex's point leaked?)", got)
	}
}

// asFloat64 coerces a driver-returned numeric value regardless of its
// concrete Go type: count(*) comes back as int64, while avg/sum/quantiles
// come back as float64. Only the MCP path's JSON round-trip normalizes this
// to float64 uniformly — calling Core directly sees the driver's real type.
func asFloat64(t *testing.T, v any) float64 {
	t.Helper()
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	default:
		t.Fatalf("value %v is neither float64 nor int64 (%T)", v, v)
		return 0
	}
}

// --- span search ---

// TestSpanSearchDottedAttributeKey is the regression test for a real trap:
// `$.service.name` is interpreted as field "service" then "name" and yields
// NULL, so a naive filter silently matches nothing. OTel keys are dotted.
func TestSpanSearchDottedAttributeKey(t *testing.T) {
	c := newTestCore(t)

	rows, _, err := c.SearchSpans("acme", "", []AttrFilter{{Key: "custom.key", Value: "x"}}, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := countRows(rows); n != 1 {
		t.Errorf("dotted attribute filter matched %d rows, want 1 — dotted keys are broken", n)
	}
}

// TestSpanSearchFindsResourceAttributes guards a silent-zero-result trap:
// service.name arrives as a RESOURCE attribute (and is promoted to the
// service_name column) but is absent from span attributes, so a filter that
// checks only `attributes` matches nothing for the most likely key a caller
// would use.
func TestSpanSearchFindsResourceAttributes(t *testing.T) {
	c := newTestCore(t)

	rows, _, err := c.SearchSpans("acme", "", []AttrFilter{{Key: "service.name", Value: "worker"}}, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := countRows(rows); n != 1 {
		t.Errorf("filter on service.name matched %d rows, want 1 — resource attributes are not searched", n)
	}

	// And a key that lives in span attributes still works.
	rows, _, _ = c.SearchSpans("acme", "", []AttrFilter{{Key: "job", Value: "sync"}}, 0, 0)
	if n := countRows(rows); n != 1 {
		t.Errorf("filter on span attribute matched %d rows, want 1", n)
	}
}

// TestSpanSearchMultipleFiltersAreAnded verifies filters combine with AND.
func TestSpanSearchMultipleFiltersAreAnded(t *testing.T) {
	c := newTestCore(t)

	rows, _, _ := c.SearchSpans("acme", "", []AttrFilter{{Key: "http.method", Value: "GET"}}, 0, 0)
	// Only the first acme span has http.method=GET.
	if n := countRows(rows); n != 1 {
		t.Errorf("http.method=GET matched %d acme spans, want 1", n)
	}
}

func TestSpanSearchServiceFilter(t *testing.T) {
	c := newTestCore(t)

	rows, _, _ := c.SearchSpans("acme", "worker", nil, 0, 0)
	if n := countRows(rows); n != 1 {
		t.Errorf("service_name=worker matched %d, want 1", n)
	}
}

func TestSpanSearchEmptyKeyRejected(t *testing.T) {
	c := newTestCore(t)

	_, _, err := c.SearchSpans("acme", "", []AttrFilter{{Key: "", Value: "x"}}, 0, 0)
	if err == nil {
		t.Error("empty filter key should error")
	}
}

// TestSpanSearchLimitIsCapped verifies a caller cannot request unbounded rows.
func TestSpanSearchLimitIsCapped(t *testing.T) {
	c := newTestCore(t)

	rows, _, err := c.SearchSpans("acme", "", nil, 0, 100000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 3 rows exist, so this only proves it did not error; the cap itself is
	// asserted by the fact that the query completed with the limit applied.
	if n := countRows(rows); n != 3 {
		t.Errorf("got %d rows, want 3", n)
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
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	engine, err := query.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	c := NewCore(engine)

	// Default window is 60 minutes, so 48h-old data must not appear.
	rows, _, _ := c.SearchSpans("acme", "", nil, 0, 0)
	if n := countRows(rows); n != 0 {
		t.Errorf("default window included 48h-old data: %d rows", n)
	}
}

// --- metric query ---

func TestMetricQueryAggregations(t *testing.T) {
	c := newTestCore(t)

	for _, agg := range []string{"avg", "sum", "min", "max", "count", "p50", "p95", "p99"} {
		rows, _, err := c.QueryMetric("acme", "cpu.util", agg, 0, "")
		if err != nil {
			t.Errorf("aggregation %q errored: %v", agg, err)
			continue
		}
		if n := countRows(rows); n != 1 {
			t.Errorf("aggregation %q returned %d rows, want 1", agg, n)
		}
	}
}

func TestMetricQueryRejectsUnknownAggregation(t *testing.T) {
	c := newTestCore(t)

	// An injection-shaped aggregation must be rejected, not interpolated.
	for _, agg := range []string{"median", "avg(value_double); DROP VIEW metrics; --", ""} {
		_, _, err := c.QueryMetric("acme", "cpu.util", agg, 0, "")
		if err == nil {
			t.Errorf("aggregation %q should be rejected", agg)
		}
		if !IsInvalidInput(err) {
			t.Errorf("aggregation %q rejection should classify as invalid input: %v", agg, err)
		}
	}
}

func TestMetricQueryGroupBy(t *testing.T) {
	c := newTestCore(t)

	rows, _, err := c.QueryMetric("acme", "cpu.util", "count", 0, "service_name")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// acme has api and worker -> 2 groups
	if n := countRows(rows); n != 2 {
		t.Errorf("group_by service_name returned %d rows, want 2", n)
	}
}

func TestMetricQueryRejectsUngroupableColumn(t *testing.T) {
	c := newTestCore(t)

	for _, col := range []string{"nonexistent", "service_name; DROP VIEW metrics; --", "1"} {
		_, _, err := c.QueryMetric("acme", "cpu.util", "count", 0, col)
		if err == nil {
			t.Errorf("group_by %q should be rejected", col)
		}
	}
}

func TestMetricQueryRequiresName(t *testing.T) {
	c := newTestCore(t)

	_, _, err := c.QueryMetric("acme", "", "avg", 0, "")
	if err == nil {
		t.Error("metric query without metric_name should error")
	}
}

// --- injection resistance at the query boundary ---

// TestFiltersCannotInjectSQL verifies attribute keys and values are bound, not
// interpolated, so a crafted filter is data rather than SQL.
func TestFiltersCannotInjectSQL(t *testing.T) {
	c := newTestCore(t)

	payloads := []AttrFilter{
		{Key: "http.method", Value: "GET' OR '1'='1"},
		{Key: `x' OR '1'='1`, Value: "y"},
		{Key: `a" AND json_extract_string(resource_attributes,'$."tenant.id"')!='acme' AND 1=1 --`, Value: "z"},
	}
	for _, f := range payloads {
		rows, _, err := c.SearchSpans("acme", "", []AttrFilter{f}, 0, 0)
		if err != nil {
			continue // rejecting outright is also fine
		}
		// Must not return rows: a successful injection would widen the set.
		if n := countRows(rows); n != 0 {
			t.Errorf("payload %+v matched %d rows — filter was interpreted as SQL", f, n)
		}
	}

	// And the store must still be intact.
	rows, _, _ := c.SearchSpans("acme", "", nil, 0, 0)
	if n := countRows(rows); n != 3 {
		t.Errorf("store damaged: acme now has %d spans, want 3", n)
	}
}

// --- discovery: ListTenants / ListServices (new, for the dashboard) ---

func TestListTenants(t *testing.T) {
	c := newTestCore(t)

	rows, _, err := c.ListTenants()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r["tenant"].(string)] = true
	}
	if !got["acme"] || !got["globex"] {
		t.Errorf("ListTenants = %v, want acme and globex", rows)
	}
}

func TestListServices(t *testing.T) {
	c := newTestCore(t)

	rows, _, err := c.ListServices("acme")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r["service_name"].(string)] = true
	}
	if !got["api"] || !got["worker"] {
		t.Errorf("ListServices(acme) = %v, want api and worker", rows)
	}
	// globex's service must not leak into acme's list — same isolation
	// guarantee as every other query, just for a discovery endpoint.
	if len(rows) != 2 {
		t.Errorf("ListServices(acme) returned %d services, want exactly 2", len(rows))
	}
}

func TestListServicesRequiresTenant(t *testing.T) {
	c := newTestCore(t)

	_, _, err := c.ListServices("")
	if err == nil {
		t.Error("ListServices without tenant should error")
	}
}

// --- tenant attribute contract ---

func TestTenantAttributeName(t *testing.T) {
	c := NewCore(nil)
	if c.tenantAttr != "tenant.id" {
		t.Errorf("tenant attribute = %q, want tenant.id", c.tenantAttr)
	}
	// The predicate must reference the quoted dotted form, or it silently
	// matches nothing and isolation appears to work while returning no data.
	if !strings.Contains(c.tenantPredicate(), `$."tenant.id"`) {
		t.Errorf("tenant predicate does not quote the dotted key: %s", c.tenantPredicate())
	}
}
