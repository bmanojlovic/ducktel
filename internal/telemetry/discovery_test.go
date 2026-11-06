package telemetry

import (
	"testing"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

// TestDiscoveryIgnoresDataOutsideWindow: the List* pickers only look at
// DiscoveryWindow, so a tenant/service/metric silent for longer is not listed
// (and its old files are not scanned).
func TestDiscoveryIgnoresDataOutsideWindow(t *testing.T) {
	dir := t.TempDir()
	w := writer.New(dir, time.Hour, 1000)
	old := time.Now().Add(-DiscoveryWindow - 24*time.Hour).UnixMicro()
	now := time.Now().UnixMicro()
	span := func(svc, tenant string, ts int64) writer.TraceSpan {
		return writer.TraceSpan{
			TraceID: "t-" + svc, SpanID: "s-" + svc, ServiceName: svc, SpanName: "op",
			StartTime: ts, EndTime: ts, DurationMs: 1, StatusCode: "STATUS_CODE_OK",
			Attributes: "{}", ResourceAttributes: `{"tenant.id":"` + tenant + `"}`,
			Events: "[]", Links: "[]",
		}
	}
	w.Add([]writer.TraceSpan{span("fresh", "acme", now), span("stale", "acme", old), span("gone", "oldco", old)})
	w.AddMetrics([]writer.MetricPoint{
		{MetricName: "fresh.metric", MetricType: "gauge", Timestamp: now, ResourceAttributes: `{"tenant.id":"acme"}`},
		{MetricName: "stale.metric", MetricType: "gauge", Timestamp: old, ResourceAttributes: `{"tenant.id":"acme"}`},
	})
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	engine, err := query.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	c := NewCore(engine)

	values := func(rows []map[string]any, col string) []any {
		var out []any
		for _, r := range rows {
			out = append(out, r[col])
		}
		return out
	}

	rows, _, err := c.ListTenants()
	if err != nil {
		t.Fatal(err)
	}
	if got := values(rows, "tenant"); len(got) != 1 || got[0] != "acme" {
		t.Errorf("tenants = %v, want [acme]", got)
	}
	rows, _, err = c.ListServices("acme")
	if err != nil {
		t.Fatal(err)
	}
	if got := values(rows, "service_name"); len(got) != 1 || got[0] != "fresh" {
		t.Errorf("services = %v, want [fresh]", got)
	}
	rows, _, err = c.ListMetricNames("acme")
	if err != nil {
		t.Fatal(err)
	}
	if got := values(rows, "metric_name"); len(got) != 1 || got[0] != "fresh.metric" {
		t.Errorf("metric names = %v, want [fresh.metric]", got)
	}
}

// TestDiscoveryIsCached: a second call within discoveryTTL is served from the
// cache, so a reload does not re-scan. Proved by closing the engine: a cached
// answer still comes back, an uncached one (other tenant) errors.
func TestDiscoveryIsCached(t *testing.T) {
	c := newTestCore(t)
	first, _, err := c.ListServices("acme")
	if err != nil {
		t.Fatal(err)
	}
	c.engine.Close()

	again, _, err := c.ListServices("acme")
	if err != nil {
		t.Fatalf("cached call hit the closed engine: %v", err)
	}
	if len(again) != len(first) {
		t.Errorf("cached result differs: %d vs %d rows", len(again), len(first))
	}
	if _, _, err := c.ListServices("globex"); err == nil {
		t.Error("uncached tenant should have queried the (closed) engine and failed")
	}
}
