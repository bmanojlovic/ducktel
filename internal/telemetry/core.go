package telemetry

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/davidgeorgehope/ducktel/internal/query"
)

// Core runs ducktel's generic, tenant-scoped telemetry queries. It is the one
// implementation of "what is a valid tenant, what does a span search's WHERE
// clause look like, what aggregations are allowed" — shared by every consumer
// so a fix or a tightened check cannot drift between two copies.
type Core struct {
	engine *query.Engine
	// tenantAttr is the resource attribute holding the tenant id.
	tenantAttr string
}

// NewCore wires a query engine into a Core. An engine of nil is valid for
// tests that only exercise validation, not queries.
func NewCore(engine *query.Engine) *Core {
	return &Core{engine: engine, tenantAttr: "tenant.id"}
}

// AttrFilter is one key/value attribute match, ANDed with any others.
type AttrFilter struct {
	Key   string
	Value string
}

// errInvalidInput classifies an error as a caller input problem (missing or
// malformed parameter) rather than a query engine failure, so an HTTP layer
// can map it to 400 instead of 500. Check with IsInvalidInput; the sentinel
// itself is unexported so its text never leaks into a returned error message.
var errInvalidInput = errors.New("invalid input")

type inputError struct{ msg string }

func (e *inputError) Error() string { return e.msg }
func (e *inputError) Unwrap() error { return errInvalidInput }
func invalidInput(format string, a ...any) error {
	return &inputError{msg: fmt.Sprintf(format, a...)}
}

// IsInvalidInput reports whether err represents a caller input problem, as
// opposed to a query engine failure.
func IsInvalidInput(err error) bool {
	return errors.Is(err, errInvalidInput)
}

func validateTenant(tenant string) error {
	if strings.TrimSpace(tenant) == "" {
		return invalidInput("tenant is required: every query is scoped to one tenant")
	}
	return nil
}

// tenantPredicate returns the mandatory isolation predicate. It is appended to
// every query as a non-optional condition.
func (c *Core) tenantPredicate() string {
	return fmt.Sprintf("json_extract_string(resource_attributes, '%s') = ?",
		strings.ReplaceAll(jsonPath(c.tenantAttr), "'", "''"))
}

// aggregationExpr maps a caller-supplied aggregation name to SQL. The value is
// chosen from a fixed set, never interpolated from input.
func aggregationExpr(name string) (string, error) {
	switch strings.ToLower(name) {
	case "avg", "mean":
		return "avg(value_double)", nil
	case "sum":
		return "sum(value_double)", nil
	case "min":
		return "min(value_double)", nil
	case "max":
		return "max(value_double)", nil
	case "count":
		return "count(*)", nil
	case "p50":
		return "approx_quantile(value_double, 0.5)", nil
	case "p95":
		return "approx_quantile(value_double, 0.95)", nil
	case "p99":
		return "approx_quantile(value_double, 0.99)", nil
	}
	return "", invalidInput("aggregation %q is not supported; use one of: avg, sum, min, max, count, p50, p95, p99", name)
}

// LookupTrace retrieves every span belonging to one trace, ordered by start
// time, scoped to tenant.
func (c *Core) LookupTrace(traceID, tenant string, limit int) ([]map[string]any, []string, error) {
	if traceID == "" {
		return nil, nil, invalidInput("trace_id is required")
	}
	if err := validateTenant(tenant); err != nil {
		return nil, nil, err
	}
	if limit <= 0 {
		limit = 1000
	}

	q := `SELECT trace_id, span_id, parent_span_id, service_name, span_name, span_kind,
	             start_time, end_time, duration_ms, status_code, status_message, attributes
	      FROM traces
	      WHERE trace_id = ?
	        AND ` + c.tenantPredicate() + `
	      ORDER BY start_time
	      LIMIT ?`

	return c.engine.Query(q, traceID, tenant, limit)
}

// SearchSpans finds spans matching attribute filters within a time range,
// scoped to tenant.
func (c *Core) SearchSpans(tenant, serviceName string, filters []AttrFilter, sinceMinutes, limit int) ([]map[string]any, []string, error) {
	if err := validateTenant(tenant); err != nil {
		return nil, nil, err
	}

	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	since := sinceMinutes
	if since <= 0 {
		since = 60
	}
	cutoff := time.Now().Add(-time.Duration(since) * time.Minute).UnixMicro()

	conds := []string{"start_time >= ?", c.tenantPredicate()}
	params := []any{cutoff, tenant}

	if serviceName != "" {
		conds = append(conds, "service_name = ?")
		params = append(params, serviceName)
	}
	for _, f := range filters {
		if f.Key == "" {
			return nil, nil, invalidInput("attribute filter key must not be empty")
		}
		// Search span attributes AND resource attributes: a caller should not
		// need to know which one a key lives in. `service.name`, for example,
		// arrives as a resource attribute (and is additionally promoted to the
		// service_name column) but is absent from span attributes — filtering
		// only attributes silently matches nothing for the most common key.
		conds = append(conds,
			"(json_extract_string(attributes, ?) = ? OR json_extract_string(resource_attributes, ?) = ?)")
		params = append(params, jsonPath(f.Key), f.Value, jsonPath(f.Key), f.Value)
	}

	params = append(params, limit)
	q := `SELECT trace_id, span_id, service_name, span_name, span_kind,
	             start_time, end_time, duration_ms, status_code, attributes
	      FROM traces
	      WHERE ` + strings.Join(conds, " AND ") + `
	      ORDER BY start_time DESC
	      LIMIT ?`

	return c.engine.Query(q, params...)
}

// QueryMetric aggregates a metric over a time range, optionally grouped by a
// column, scoped to tenant.
//
// value_double is the normalised numeric column: it is populated for both
// double and int points (see the receiver's fillNumberDataPoint). A histogram
// point stores its data in sum/min/max/count/bucket_counts instead and is
// therefore invisible here — it aggregates to 0, not an error.
func (c *Core) QueryMetric(tenant, metricName, aggregation string, sinceMinutes int, groupBy string) ([]map[string]any, []string, error) {
	if err := validateTenant(tenant); err != nil {
		return nil, nil, err
	}
	if metricName == "" {
		return nil, nil, invalidInput("metric_name is required")
	}

	agg, err := aggregationExpr(aggregation)
	if err != nil {
		return nil, nil, err
	}

	since := sinceMinutes
	if since <= 0 {
		since = 60
	}
	cutoff := time.Now().Add(-time.Duration(since) * time.Minute).UnixMicro()

	selectCols := []string{}
	groupCols := []string{}
	if groupBy != "" {
		col, ok := quoteIdent(groupBy)
		if !ok {
			return nil, nil, invalidInput("group_by %q is not a groupable column; allowed: %s",
				groupBy, knownColumnsList())
		}
		selectCols = append(selectCols, col)
		groupCols = append(groupCols, col)
	}
	selectCols = append(selectCols, fmt.Sprintf("%s AS value", agg))

	q := "SELECT " + strings.Join(selectCols, ", ") + ` FROM metrics
	      WHERE metric_name = ?
	        AND timestamp >= ?
	        AND ` + c.tenantPredicate()
	params := []any{metricName, cutoff, tenant}

	if len(groupCols) > 0 {
		q += " GROUP BY " + strings.Join(groupCols, ", ") + " ORDER BY " + groupCols[0]
	}

	return c.engine.Query(q, params...)
}

// ListTenants returns every distinct tenant id seen in stored traces. No MCP
// tool needs this — an agent caller already knows its own tenant — but a
// human-facing consumer needs a way to discover what exists.
func (c *Core) ListTenants() ([]map[string]any, []string, error) {
	path := strings.ReplaceAll(jsonPath(c.tenantAttr), "'", "''")
	q := fmt.Sprintf(`SELECT DISTINCT json_extract_string(resource_attributes, '%s') AS tenant
	      FROM traces
	      WHERE json_extract_string(resource_attributes, '%s') IS NOT NULL
	      ORDER BY 1`, path, path)
	return c.engine.Query(q)
}

// ListServices returns distinct service names within one tenant.
func (c *Core) ListServices(tenant string) ([]map[string]any, []string, error) {
	if err := validateTenant(tenant); err != nil {
		return nil, nil, err
	}
	q := `SELECT DISTINCT service_name FROM traces WHERE ` + c.tenantPredicate() + ` ORDER BY 1`
	return c.engine.Query(q, tenant)
}
