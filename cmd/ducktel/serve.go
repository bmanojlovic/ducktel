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
token would cross an untrusted network.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Env wins only when the flag was not given, so an explicit flag
			// can always override the environment.
			if !cmd.Flags().Changed("auth-token") {
				if v := os.Getenv("DUCKTEL_AUTH_TOKEN"); v != "" {
					token = v
				}
			}

			w := writer.New(dataDir, flushInterval, 1000)
			w.OnError(func(err error) {
				log.Printf("writer flush failed: %v", err)
			})
			w.Start()

			r := receiver.New(host, port, w, token)

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

			errCh := make(chan error, 1)
			go func() {
				errCh <- r.Start()
			}()

			select {
			case err := <-errCh:
				if err := w.Stop(); err != nil {
					log.Printf("final flush failed: %v", err)
				}
				return err
			case <-sigCh:
				log.Println("Shutting down...")
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				r.Stop(ctx)
				if err := w.Stop(); err != nil {
					log.Printf("final flush failed: %v", err)
				}
				log.Println("Stopped.")
				return nil
			}
		},
	}

	cmd.Flags().StringVar(&host, "host", "localhost", "Host/interface to bind (use 0.0.0.0 for all interfaces)")
	cmd.Flags().IntVar(&port, "port", 4318, "Port to listen on")
	cmd.Flags().DurationVar(&flushInterval, "flush-interval", 30*time.Second, "How often to flush buffered spans to disk")
	cmd.Flags().StringVar(&token, "auth-token", "", "Require this bearer token on OTLP endpoints (env: DUCKTEL_AUTH_TOKEN)")

	return cmd
}
