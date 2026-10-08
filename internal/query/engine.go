package query

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "github.com/marcboeker/go-duckdb"
)

type ColumnInfo struct {
	Name string
	Type string
}

// views is the allowlist of queryable signal views.
var views = map[string]bool{
	"traces":  true,
	"logs":    true,
	"metrics": true,
}

func validView(name string) bool {
	return views[name]
}

type Engine struct {
	db      *sql.DB
	dataDir string
}

// cgroupMemoryFiles are the paths read to detect a container memory limit —
// cgroup v2 first, v1 second. A var so tests can point it at fixture files.
var cgroupMemoryFiles = []string{
	"/sys/fs/cgroup/memory.max",                   // cgroup v2: "max" or bytes
	"/sys/fs/cgroup/memory/memory.limit_in_bytes", // cgroup v1: bytes
}

// cgroupMemoryLimit returns the cgroup memory limit in bytes, or 0 when there
// is none (bare-metal dev machine, or the file says "max").
func cgroupMemoryLimit() int64 {
	for _, p := range cgroupMemoryFiles {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(b))
		if s == "max" {
			return 0
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		if n > 1<<60 {
			// cgroup v1's "unlimited" sentinel (≈ 2^63); not a real limit.
			continue
		}
		return n
	}
	return 0
}

// memoryLimitFraction is how much of the detected cgroup limit DuckDB's
// buffer manager may use. The rest stays free for the Go heap and page cache,
// so a wide query spills to DuckDB's temp directory instead of pushing the
// whole process over the limit and getting OOM-killed.
const memoryLimitFraction = 70

func Open(dataDir string) (*Engine, error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("opening duckdb: %w", err)
	}

	if limit := cgroupMemoryLimit(); limit > 0 {
		// DuckDB sizes its buffer manager to ~80% of what it detects; inside
		// a cgroup-limited container that left almost nothing for the rest of
		// the process and wide scans OOM-killed the mcp container (measured:
		// a 7-day metric scan pinned 4Gi). Capping below the limit makes the
		// engine spill to disk rather than die. Division first, so an extreme
		// cgroup value cannot overflow int64.
		// DuckDB's memory_limit pragma wants a unit suffix; whole MiB, rounded
		// down so the cap can only undershoot, never overshoot.
		capped := limit / 100 * memoryLimitFraction / (1 << 20)
		if _, err := db.Exec(fmt.Sprintf("PRAGMA memory_limit='%dMiB';", capped)); err != nil {
			db.Close()
			return nil, fmt.Errorf("setting duckdb memory_limit: %w", err)
		}
	}

	e := &Engine{db: db, dataDir: dataDir}
	if err := e.CreateViews(); err != nil {
		db.Close()
		return nil, err
	}
	return e, nil
}

// CreateViews (re)derives the signal views from the Parquet files currently on
// disk. DuckDB resolves a read_parquet glob when the view is created, so views
// must be re-derived to observe files written afterwards. See refresh in Query:
// without it an engine opened before any data existed kept a placeholder
// `WHERE false` view and returned zero rows forever.
func (e *Engine) CreateViews() error {
	views := []struct {
		name      string
		signal    string
		emptyCols string
	}{
		{
			name:   "traces",
			signal: "traces",
			emptyCols: `'' as trace_id, '' as span_id, '' as parent_span_id,
				'' as trace_state,
				'' as service_name, '' as span_name, '' as span_kind,
				CAST(0 AS BIGINT) as start_time, CAST(0 AS BIGINT) as end_time,
				CAST(0.0 AS DOUBLE) as duration_ms,
				'' as status_code, '' as status_message, '' as attributes,
				'' as resource_attributes, '' as scope_name, '' as scope_version,
				'' as events, '' as links`,
		},
		{
			name:   "logs",
			signal: "logs",
			emptyCols: `CAST(0 AS BIGINT) as timestamp, CAST(0 AS BIGINT) as observed_timestamp,
				'' as trace_id, '' as span_id,
				CAST(0 AS INTEGER) as severity_number, '' as severity_text,
				'' as body, '' as attributes, '' as resource_attributes,
				'' as service_name, '' as scope_name, '' as scope_version,
				CAST(0 AS UINTEGER) as flags, '' as event_name`,
		},
		{
			name:   "metrics",
			signal: "metrics",
			emptyCols: `'' as metric_name, '' as metric_description, '' as metric_unit,
				'' as metric_type,
				CAST(0 AS BIGINT) as timestamp, CAST(0 AS BIGINT) as start_timestamp,
				CAST(0.0 AS DOUBLE) as value_double, CAST(0 AS BIGINT) as value_int,
				CAST(0 AS UBIGINT) as count, CAST(0.0 AS DOUBLE) as sum,
				CAST(0.0 AS DOUBLE) as min, CAST(0.0 AS DOUBLE) as max,
				'' as bucket_counts, '' as explicit_bounds, '' as quantile_values,
				'' as attributes, '' as resource_attributes,
				'' as service_name, '' as scope_name, '' as scope_version,
				'' as exemplars, CAST(0 AS UINTEGER) as flags,
				false as is_monotonic, '' as aggregation_temporality`,
		},
	}

	for _, v := range views {
		glob := filepath.Join(e.dataDir, v.signal, "**", "*.parquet")
		q := fmt.Sprintf(`CREATE OR REPLACE VIEW %s AS SELECT * FROM read_parquet('%s', union_by_name=true)`, v.name, glob)
		if _, err := e.db.Exec(q); err != nil {
			// No files yet — create empty view with correct schema
			empty := fmt.Sprintf(`CREATE OR REPLACE VIEW %s AS SELECT %s WHERE false`, v.name, v.emptyCols)
			if _, err2 := e.db.Exec(empty); err2 != nil {
				return fmt.Errorf("creating empty %s view: %w", v.name, err2)
			}
		}
	}
	return nil
}

// Query executes SQL with optional bound parameters. Values passed in args are
// sent to DuckDB as placeholders (?) rather than interpolated into the SQL
// string, which prevents SQL injection from caller-supplied values.
func (e *Engine) Query(sqlStr string, args ...interface{}) ([]map[string]interface{}, []string, error) {
	// Re-derive the views so the query sees Parquet files written since the
	// engine was opened. Cheap: DuckDB resolves the glob at view creation.
	if err := e.CreateViews(); err != nil {
		return nil, nil, fmt.Errorf("refreshing views: %w", err)
	}

	rows, err := e.db.Query(sqlStr, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("executing query: %w", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, nil, fmt.Errorf("getting columns: %w", err)
	}

	var results []map[string]interface{}
	for rows.Next() {
		values := make([]interface{}, len(columns))
		valuePtrs := make([]interface{}, len(columns))
		for i := range values {
			valuePtrs[i] = &values[i]
		}

		if err := rows.Scan(valuePtrs...); err != nil {
			return nil, nil, fmt.Errorf("scanning row: %w", err)
		}

		row := make(map[string]interface{}, len(columns))
		for i, col := range columns {
			val := values[i]
			if b, ok := val.([]byte); ok {
				val = string(b)
			}
			row[col] = val
		}
		results = append(results, row)
	}

	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterating rows: %w", err)
	}

	return results, columns, nil
}

// Describe returns column info for a known view. The view name is validated
// against an allowlist because identifiers cannot be bound as parameters.
func (e *Engine) Describe(view string) ([]ColumnInfo, error) {
	if !validView(view) {
		return nil, fmt.Errorf("unknown view %q (expected traces, logs, or metrics)", view)
	}
	if err := e.CreateViews(); err != nil {
		return nil, fmt.Errorf("refreshing views: %w", err)
	}
	rows, err := e.db.Query(fmt.Sprintf("DESCRIBE %s", view))
	if err != nil {
		return nil, fmt.Errorf("describing %s: %w", view, err)
	}
	defer rows.Close()

	var cols []ColumnInfo
	for rows.Next() {
		var name, typ string
		var null, key, def, extra sql.NullString
		if err := rows.Scan(&name, &typ, &null, &key, &def, &extra); err != nil {
			return nil, fmt.Errorf("scanning column info: %w", err)
		}
		cols = append(cols, ColumnInfo{Name: name, Type: typ})
	}
	return cols, rows.Err()
}

// Schema describes the traces view (kept for backward compatibility).
func (e *Engine) Schema() ([]ColumnInfo, error) {
	return e.Describe("traces")
}

func (e *Engine) Close() error {
	return e.db.Close()
}
