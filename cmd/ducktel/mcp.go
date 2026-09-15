package main

import (
	"context"
	"fmt"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/davidgeorgehope/ducktel/internal/mcp"
	"github.com/davidgeorgehope/ducktel/internal/query"
)

func mcpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve generic telemetry queries over the Model Context Protocol",
		Long: `Serve ducktel's stored telemetry as MCP tools on stdio.

The tools are generic and domain-agnostic — they know about spans, attributes,
metrics and time, and nothing about what any attribute means:

  trace_lookup(trace_id)                    all spans of one trace
  span_search(filters, time_range)          spans matching attribute filters
  metric_query(name, aggregation, ...)      metric aggregation over a range

Every tool requires a tenant and hard-filters on the tenant.id resource
attribute, so a caller cannot widen its scope.

Transport is stdio, so the process boundary is the trust boundary and no token
is needed. Run it as a subprocess of the agent; the same --data-dir as the
receiver must be readable.

Example client configuration:
  {"command": "ducktel", "args": ["mcp", "--data-dir", "/data"]}`,
		RunE: func(cmd *cobra.Command, args []string) error {
			engine, err := query.Open(dataDir)
			if err != nil {
				return fmt.Errorf("opening query engine: %w", err)
			}
			defer engine.Close()

			srv := sdkmcp.NewServer(&sdkmcp.Implementation{
				Name:    "ducktel",
				Version: "0.1.0",
			}, nil)
			mcp.NewServer(engine).Register(srv)

			if err := srv.Run(context.Background(), &sdkmcp.StdioTransport{}); err != nil {
				return fmt.Errorf("mcp server: %w", err)
			}
			return nil
		},
	}
	return cmd
}
