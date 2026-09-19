package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/davidgeorgehope/ducktel/internal/receiver"
	"github.com/davidgeorgehope/ducktel/internal/writer"
)

func serveCmd() *cobra.Command {
	var (
		host          string
		port          int
		flushInterval time.Duration
		token         string
		flushToken    string
		retention     string
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the OTLP receiver",
		Long: `Start the OTLP receiver.

Authentication is disabled unless a token is configured. Set it with
--auth-token or DUCKTEL_AUTH_TOKEN; senders must then present it as
"Authorization: Bearer <token>", which every OTel exporter supports via
OTEL_EXPORTER_OTLP_HEADERS. /health stays unauthenticated for probes.

TLS is not terminated here — front the receiver with a proxy or ingress if the
token would cross an untrusted network.

--retention deletes whole date-partition directories older than the given
window (accepts Go durations like 720h, or a bare day count like 30d). Empty
(the default) disables it, so upgrading an existing deployment never starts
deleting data without an explicit opt-in.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			retentionDur, err := writer.ParseRetention(retention)
			if err != nil {
				return err
			}

			// Env wins only when the flag was not given, so an explicit flag
			// can always override the environment.
			if !cmd.Flags().Changed("auth-token") {
				if v := os.Getenv("DUCKTEL_AUTH_TOKEN"); v != "" {
					token = v
				}
			}
			// /flush is authorised by its own token, separate from the ingest
			// token, so the query side can request a flush without holding
			// write access.
			if !cmd.Flags().Changed("flush-token") {
				if v := os.Getenv("DUCKTEL_FLUSH_TOKEN"); v != "" {
					flushToken = v
				}
			}
			// Fall back to the MCP token: in the shipped deployment the query
			// container holds only DUCKTEL_MCP_TOKEN, and this process is the
			// one that must accept it.
			if flushToken == "" {
				if v := os.Getenv("DUCKTEL_MCP_TOKEN"); v != "" {
					flushToken = v
				}
			}

			w := writer.New(dataDir, flushInterval, 1000)
			w.OnError(func(err error) {
				log.Printf("writer error: %v", err)
			})
			if retentionDur > 0 {
				w.SetRetention(retentionDur)
			}
			w.Start()

			// Log the effective configuration once at startup: without it the
			// log cannot be used to tell what a running instance was told to do.
			authState := "disabled"
			if token != "" {
				authState = "enabled"
			}
			retentionState := "disabled (keep forever)"
			if retentionDur > 0 {
				retentionState = retentionDur.String()
			}
			log.Printf("starting: data-dir=%s flush-interval=%s auth=%s retention=%s",
				dataDir, flushInterval, authState, retentionState)

			r := receiver.New(host, port, w, token, flushToken, w.Flush)

			sigCh := make(chan os.Signal, 1)
			// SIGHUP requests an immediate flush without shutting down, so
			// buffered telemetry can be made queryable on demand instead of
			// waiting out the flush interval. Intercepting it also stops Go's
			// default behaviour of terminating on SIGHUP.
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

			errCh := make(chan error, 1)
			go func() {
				errCh <- r.Start()
			}()

			for {
				select {
				case err := <-errCh:
					if err := w.Stop(); err != nil {
						log.Printf("final flush failed: %v", err)
					}
					return err
				case sig := <-sigCh:
					if sig == syscall.SIGHUP {
						// Flush in place: the receiver keeps serving and the
						// writer keeps its buffer, so nothing is dropped.
						log.Printf("received %s: flushing buffered telemetry", sig)
						if err := w.Flush(); err != nil {
							log.Printf("flush on %s failed: %v", sig, err)
						} else {
							log.Printf("flush on %s complete", sig)
						}
						continue
					}
					log.Printf("received %s: shutting down...", sig)
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					r.Stop(ctx)
					if err := w.Stop(); err != nil {
						log.Printf("final flush failed: %v", err)
					}
					log.Println("Stopped.")
					return nil
				}
			}
		},
	}

	cmd.Flags().StringVar(&host, "host", "localhost", "Host/interface to bind (use 0.0.0.0 for all interfaces)")
	cmd.Flags().IntVar(&port, "port", 4318, "Port to listen on")
	cmd.Flags().DurationVar(&flushInterval, "flush-interval", 30*time.Second, "How often to flush buffered spans to disk")
	cmd.Flags().StringVar(&token, "auth-token", "", "Require this bearer token on OTLP endpoints (env: DUCKTEL_AUTH_TOKEN)")
	cmd.Flags().StringVar(&flushToken, "flush-token", "", "Require this bearer token on POST /flush (env: DUCKTEL_FLUSH_TOKEN, or DUCKTEL_MCP_TOKEN)")
	cmd.Flags().StringVar(&retention, "retention", "", "Delete date-partition directories older than this (e.g. 30d, 720h); empty disables retention (default: keep forever)")

	return cmd
}
