package mcp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/davidgeorgehope/ducktel/internal/query"
)

// Server exposes ducktel's stored telemetry as MCP tools.
//
// Every tool takes a tenant and hard-filters on it, so a caller cannot widen
// its scope by omitting a parameter or by crafting a filter. The tools are
// generic: they operate on spans, attributes, metrics and time, and attach no
// meaning to any particular attribute key.
type Server struct {
	engine *query.Engine
	// tenantAttr is the resource attribute holding the tenant id.
	tenantAttr string
	// flushURL, when set, is the writer's POST /flush endpoint. The query side
	// asks the writer to make buffered records durable, because this process
	// reads Parquet files and cannot see data still in the writer's memory.
	flushURL string
	// flushToken authorises that request. It is the query side's own token,
	// never the ingest token — so this process can ask for a flush but can
	// never write telemetry.
	flushToken string
	// httpClient issues the flush request.
	httpClient *http.Client
}

// NewServer wires the query engine into an MCP server. An empty flushURL
// disables the flush tool.
func NewServer(engine *query.Engine, flushURL, flushToken string) *Server {
	return &Server{
		engine:     engine,
		tenantAttr: "tenant.id",
		flushURL:   flushURL,
		flushToken: flushToken,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// Register adds the tools to an MCP server.
func (s *Server) Register(srv *sdkmcp.Server) {
	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name: "trace_lookup",
		Description: "Retrieve every span belonging to one trace, ordered by start time. " +
			"Use this to reconstruct the full call tree of a single request: it returns all " +
			"spans sharing the given trace id, with parent/child relationships, timing, status " +
			"and attributes. Requires the tenant the trace belongs to.",
	}, s.traceLookup)

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name: "span_search",
		Description: "Find spans matching attribute filters within a time range. " +
			"Use this to locate spans by any attribute key/value pair — for example " +
			"service.name, http.request.method, or any custom attribute. Returns matching " +
			"spans with timing, status and attributes, capped at limit results.",
	}, s.spanSearch)

	sdkmcp.AddTool(srv, &sdkmcp.Tool{
		Name: "metric_query",
		Description: "Aggregate a metric over a time range, optionally grouped by a column. " +
			"Use this for numeric questions — request rates, resource utilisation, " +
			"min/avg/max of a recorded value — rather than fetching raw points. " +
			"Supports avg, sum, min, max, count and percentiles. " +
			"IMPORTANT for senders: this aggregates the value_double column, which is " +
			"only populated for gauge and sum points. A histogram point stores its data " +
			"in sum/min/max/count/bucket_counts instead and is therefore INVISIBLE here " +
			"— it returns 0 rather than an error. Send a per-event duration or size as a " +
			"gauge or sum, not a histogram, or it will not be aggregatable.",
	}, s.metricQuery)

	// Only offered when a flush endpoint is configured: without one the tool
	// could not do anything, and advertising a tool that always fails would
	// waste a caller's attempt and muddy tool selection.
	if s.flushURL != "" {
		sdkmcp.AddTool(srv, &sdkmcp.Tool{
			Name: "flush_buffer",
			Description: "Make recently received telemetry queryable immediately. " +
				"This server reads telemetry from files that the receiver writes every " +
				"flush interval, so data sent in the last interval is not yet visible. " +
				"Call this when a query returns nothing for data you just sent, then " +
				"repeat the query. Accepts no arguments.",
		}, s.flushBuffer)
	}
}

// --- flush_buffer ---

type flushArgs struct{}

// flushBuffer asks the writing process to persist its buffered records.
//
// It authenticates with this server's own token, not the ingest token: the
// query side is allowed to request a flush but must never be able to write
// telemetry, so the two secrets stay separate.
func (s *Server) flushBuffer(ctx context.Context, req *sdkmcp.CallToolRequest, args flushArgs) (*sdkmcp.CallToolResult, any, error) {
	if s.flushURL == "" {
		return errResult("flush is not configured: the server was started without a flush endpoint")
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.flushURL, nil)
	if err != nil {
		return errResult(fmt.Sprintf("building flush request: %v", err))
	}
	if s.flushToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+s.flushToken)
	}

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return errResult(fmt.Sprintf("flush request failed: %v", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return errResult(fmt.Sprintf("flush rejected: %s: %s",
			resp.Status, strings.TrimSpace(string(body))))
	}

	return &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{
			Text: `{"status":"flushed","note":"buffered telemetry is now on disk; repeat your query"}`,
		}},
	}, nil, nil
}

// --- trace_lookup ---

type traceLookupArgs struct {
	TraceID string `json:"trace_id" jsonschema:"the trace id to retrieve, as 32 lowercase hex characters"`
	Tenant  string `json:"tenant" jsonschema:"the tenant this trace belongs to; only data for this tenant is returned"`
	Limit   int    `json:"limit,omitempty" jsonschema:"maximum spans to return, default 1000"`
}

func (s *Server) traceLookup(ctx context.Context, req *sdkmcp.CallToolRequest, args traceLookupArgs) (*sdkmcp.CallToolResult, any, error) {
	if args.TraceID == "" {
		return errResult("trace_id is required")
	}
	if err := validateTenant(args.Tenant); err != nil {
		return errResult(err.Error())
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 1000
	}

	q := `SELECT trace_id, span_id, parent_span_id, service_name, span_name, span_kind,
	             start_time, end_time, duration_ms, status_code, status_message, attributes
	      FROM traces
	      WHERE trace_id = ?
	        AND ` + s.tenantPredicate() + `
	      ORDER BY start_time
	      LIMIT ?`

	rows, cols, err := s.engine.Query(q, args.TraceID, args.Tenant, limit)
	if err != nil {
		return errResult(fmt.Sprintf("query failed: %v", err))
	}
	return rowsResult(rows, cols)
}

// --- span_search ---

type attrFilter struct {
	Key   string `json:"key" jsonschema:"attribute key to match, e.g. service.name or http.request.method. Dotted keys are handled correctly."`
	Value string `json:"value" jsonschema:"exact value to match for this key"`
}

type spanSearchArgs struct {
	Tenant       string       `json:"tenant" jsonschema:"the tenant to query; only data for this tenant is returned"`
	Filters      []attrFilter `json:"filters,omitempty" jsonschema:"attribute key/value pairs to match (ANDed together). Omit to match all spans in the time range."`
	ServiceName  string       `json:"service_name,omitempty" jsonschema:"restrict to one service name"`
	SinceMinutes int          `json:"since_minutes,omitempty" jsonschema:"how far back to search, in minutes. Default 60."`
	Limit        int          `json:"limit,omitempty" jsonschema:"maximum spans to return, default 100. Cap 1000."`
}

func (s *Server) spanSearch(ctx context.Context, req *sdkmcp.CallToolRequest, args spanSearchArgs) (*sdkmcp.CallToolResult, any, error) {
	if err := validateTenant(args.Tenant); err != nil {
		return errResult(err.Error())
	}

	limit := args.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	since := args.SinceMinutes
	if since <= 0 {
		since = 60
	}

	cutoff := time.Now().Add(-time.Duration(since) * time.Minute).UnixMicro()

	conds := []string{"start_time >= ?", s.tenantPredicate()}
	params := []any{cutoff, args.Tenant}

	if args.ServiceName != "" {
		conds = append(conds, "service_name = ?")
		params = append(params, args.ServiceName)
	}
	for _, f := range args.Filters {
		if f.Key == "" {
			return errResult("attribute filter key must not be empty")
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

	rows, cols, err := s.engine.Query(q, params...)
	if err != nil {
		return errResult(fmt.Sprintf("query failed: %v", err))
	}
	return rowsResult(rows, cols)
}

// --- metric_query ---

type metricQueryArgs struct {
	Tenant       string `json:"tenant" jsonschema:"the tenant to query; only data for this tenant is returned"`
	MetricName   string `json:"metric_name" jsonschema:"the metric name to aggregate, e.g. process.cpu.utilization"`
	Aggregation  string `json:"aggregation" jsonschema:"one of: avg, sum, min, max, count, p50, p95, p99"`
	SinceMinutes int    `json:"since_minutes,omitempty" jsonschema:"how far back to aggregate, in minutes. Default 60."`
	GroupBy      string `json:"group_by,omitempty" jsonschema:"optional column to group by, e.g. service_name or metric_type"`
}

func (s *Server) metricQuery(ctx context.Context, req *sdkmcp.CallToolRequest, args metricQueryArgs) (*sdkmcp.CallToolResult, any, error) {
	if err := validateTenant(args.Tenant); err != nil {
		return errResult(err.Error())
	}
	if args.MetricName == "" {
		return errResult("metric_name is required")
	}

	agg, err := aggregationExpr(args.Aggregation)
	if err != nil {
		return errResult(err.Error())
	}

	since := args.SinceMinutes
	if since <= 0 {
		since = 60
	}
	cutoff := time.Now().Add(-time.Duration(since) * time.Minute).UnixMicro()

	// value_double is the normalised numeric column: it is populated for both
	// double and int points (see the receiver's fillNumberDataPoint).
	selectCols := []string{}
	groupCols := []string{}
	if args.GroupBy != "" {
		col, ok := quoteIdent(args.GroupBy)
		if !ok {
			return errResult(fmt.Sprintf("group_by %q is not a groupable column; allowed: %s",
				args.GroupBy, knownColumnsList()))
		}
		selectCols = append(selectCols, col)
		groupCols = append(groupCols, col)
	}
	selectCols = append(selectCols, fmt.Sprintf("%s AS value", agg))

	q := "SELECT " + strings.Join(selectCols, ", ") + ` FROM metrics
	      WHERE metric_name = ?
	        AND timestamp >= ?
	        AND ` + s.tenantPredicate()
	params := []any{args.MetricName, cutoff, args.Tenant}

	if len(groupCols) > 0 {
		q += " GROUP BY " + strings.Join(groupCols, ", ") + " ORDER BY " + groupCols[0]
	}

	rows, cols, err := s.engine.Query(q, params...)
	if err != nil {
		return errResult(fmt.Sprintf("query failed: %v", err))
	}
	return rowsResult(rows, cols)
}

// --- shared helpers ---

// tenantPredicate returns the mandatory isolation predicate. It is appended to
// every query as a non-optional condition.
func (s *Server) tenantPredicate() string {
	return fmt.Sprintf("json_extract_string(resource_attributes, '%s') = ?",
		strings.ReplaceAll(jsonPath(s.tenantAttr), "'", "''"))
}

func validateTenant(tenant string) error {
	if strings.TrimSpace(tenant) == "" {
		return fmt.Errorf("tenant is required: every query is scoped to one tenant")
	}
	return nil
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
	return "", fmt.Errorf("aggregation %q is not supported; use one of: avg, sum, min, max, count, p50, p95, p99", name)
}

func errResult(msg string) (*sdkmcp.CallToolResult, any, error) {
	return &sdkmcp.CallToolResult{
		IsError: true,
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: msg}},
	}, nil, nil
}

func rowsResult(rows []map[string]any, cols []string) (*sdkmcp.CallToolResult, any, error) {
	// Return the rows as JSON text: unambiguous, and the shape the caller
	// expects from a query tool.
	out, err := marshalRows(rows, cols)
	if err != nil {
		return errResult(fmt.Sprintf("encoding results: %v", err))
	}
	return &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: out}},
	}, nil, nil
}
