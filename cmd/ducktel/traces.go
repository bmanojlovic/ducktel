package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/davidgeorgehope/ducktel/internal/cli"
	"github.com/davidgeorgehope/ducktel/internal/query"
)

func tracesCmd() *cobra.Command {
	var (
		service string
		since   time.Duration
		status  string
		limit   int
	)

	cmd := &cobra.Command{
		Use:   "traces",
		Short: "Query traces with filters",
		RunE: func(cmd *cobra.Command, args []string) error {
			engine, err := query.Open(dataDir)
			if err != nil {
				return fmt.Errorf("opening query engine: %w", err)
			}
			defer engine.Close()

			q := "SELECT trace_id, span_id, service_name, span_name, span_kind, duration_ms, status_code FROM traces"

			var conditions []string
			var params []interface{}
			if service != "" {
				conditions = append(conditions, "service_name = ?")
				params = append(params, service)
			}
			if since > 0 {
				cutoff := time.Now().Add(-since).UnixMicro()
				conditions = append(conditions, "start_time >= ?")
				params = append(params, cutoff)
			}
			if status != "" {
				code := "STATUS_CODE_" + strings.ToUpper(status)
				conditions = append(conditions, "status_code = ?")
				params = append(params, code)
			}

			if len(conditions) > 0 {
				q += " WHERE " + strings.Join(conditions, " AND ")
			}
			q += " ORDER BY start_time DESC"
			q += fmt.Sprintf(" LIMIT %d", clampLimit(limit))

			results, columns, err := engine.Query(q, params...)
			if err != nil {
				return fmt.Errorf("querying traces: %w", err)
			}

			return cli.FormatResults(os.Stdout, results, columns, format)
		},
	}

	cmd.Flags().StringVar(&service, "service", "", "Filter by service name")
	cmd.Flags().DurationVar(&since, "since", 0, "Filter spans newer than this duration (e.g. 1h, 30m)")
	cmd.Flags().StringVar(&status, "status", "", "Filter by status: error, ok, unset")
	cmd.Flags().IntVar(&limit, "limit", 20, "Maximum number of spans to return")

	return cmd
}
