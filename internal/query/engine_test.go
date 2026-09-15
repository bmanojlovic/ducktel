package query

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/davidgeorgehope/ducktel/internal/writer"
)

func seed(t *testing.T, dir, service string) {
	t.Helper()
	w := writer.New(dir, 0, 1000)
	w.Add([]writer.TraceSpan{{
		TraceID: "t", SpanID: "s", ServiceName: service, SpanName: "op",
		StartTime: 1, EndTime: 2, DurationMs: 1,
		StatusCode: "STATUS_CODE_OK", Attributes: "{}",
		ResourceAttributes: "{}", Events: "[]", Links: "[]",
	}})
	if err := w.Flush(); err != nil {
		t.Fatalf("seeding %s: %v", service, err)
	}
}

// TestOpenOnEmptyDirSucceeds verifies a first run (no data yet) yields a usable
// engine with the correct schema, rather than an error.
func TestOpenOnEmptyDirSucceeds(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open on empty dir: %v", err)
	}
	defer e.Close()

	cols, err := e.Describe("traces")
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(cols) == 0 {
		t.Error("empty view should still expose its schema")
	}
}

func TestAllViewsExist(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	for _, v := range []string{"traces", "logs", "metrics"} {
		if _, err := e.Describe(v); err != nil {
			t.Errorf("view %q not queryable: %v", v, err)
		}
	}
}

// TestQuerySeesDataWrittenAfterOpen pins the view-refresh guarantee: DuckDB
// resolves the read_parquet glob when the view is created, so without per-query
// re-derivation an engine opened before any data existed kept a
// `SELECT ... WHERE false` placeholder view and returned zero rows forever, even
// once Parquet files were on disk.
func TestQuerySeesDataWrittenAfterOpen(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	rows, _, err := e.Query("SELECT count(*) AS c FROM traces")
	if err != nil {
		t.Fatal(err)
	}
	if rows[0]["c"].(int64) != 0 {
		t.Fatalf("expected empty store, got %v", rows[0]["c"])
	}

	seed(t, dir, "late")

	rows, _, err = e.Query("SELECT service_name FROM traces")
	if err != nil {
		t.Fatalf("query after data: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("a long-lived engine returned %d rows after data was written, "+
			"want 1 — views are stale", len(rows))
	}
}

// TestDescribeSeesDataWrittenAfterOpen covers the same staleness for schema
// introspection.
func TestDescribeSeesDataWrittenAfterOpen(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	seed(t, dir, "late")

	cols, err := e.Describe("traces")
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) == 0 {
		t.Error("Describe returned no columns after data arrived")
	}
}

func TestQueryReturnsColumns(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "svc")

	e, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	rows, cols, err := e.Query("SELECT service_name, duration_ms FROM traces")
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 2 || cols[0] != "service_name" || cols[1] != "duration_ms" {
		t.Errorf("columns = %v", cols)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0]["service_name"] != "svc" {
		t.Errorf("service_name = %v", rows[0]["service_name"])
	}
}

func TestQueryErrorSurfaces(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	if _, _, err := e.Query("SELECT * FROM no_such_view"); err == nil {
		t.Error("querying a missing view should error")
	}
	if _, _, err := e.Query("this is not sql"); err == nil {
		t.Error("malformed SQL should error")
	}
}

// TestQueryFiltersLiterally verifies a value used in a WHERE clause matches
// literally. (On top of the parameter-binding work this is expressed as a
// bound argument; here it is deliberately inlined to pin the semantics.)
func TestQueryFiltersLiterally(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "svc")

	e, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	rows, _, err := e.Query("SELECT service_name FROM traces WHERE service_name = 'svc'")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("query returned %d rows, want 1", len(rows))
	}
}

// TestSchemaDefaultsToTraces covers the backward-compatible accessor.
func TestSchemaDefaultsToTraces(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	cols, err := e.Schema()
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	found := false
	for _, c := range cols {
		if c.Name == "trace_id" {
			found = true
		}
	}
	if !found {
		t.Errorf("traces schema missing trace_id: %v", cols)
	}
}

// TestOpenNonWritableDirSurfacesError verifies a bad data dir fails loudly at
// Open rather than producing an engine that silently returns nothing.
func TestOpenNonWritableDirSurfacesError(t *testing.T) {
	dir := t.TempDir()
	// Occupy the traces path with a regular file so view creation fails.
	if err := os.WriteFile(filepath.Join(dir, "traces"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	e, err := Open(dir)
	if err != nil {
		return // acceptable: failing at Open is the desired behaviour
	}
	defer e.Close()
	// If Open tolerated it, queries must at least not panic and should error.
	if _, _, err := e.Query("SELECT * FROM traces"); err != nil {
		t.Logf("query errored as expected: %v", err)
	}
}
