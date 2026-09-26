package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/getargvio/argvio/internal/config"
	"github.com/getargvio/argvio/internal/metricsapi"
	"github.com/getargvio/argvio/internal/storage"
	"github.com/getargvio/argvio/internal/tenant"
)

// newServeMetricsCommand runs the read-oriented query/analysis API:
// filtering and aggregation over ingested telemetry, for tenant dashboards
// or our own dashboard backend. Internal/trusted-tenant facing — see
// docs/architecture.md for why it runs as a separate process from `serve
// public`.
func newServeMetricsCommand() *cobra.Command {
	var configPath, openAPIPath string

	cmd := &cobra.Command{
		Use:   "metrics",
		Short: "Run the metrics query API",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadMetrics(configPath)
			if err != nil {
				return fmt.Errorf("config error:\n%w", err)
			}
			log := newLogger(cfg.LogLevel)
			if err := runMetrics(cfg, openAPIPath, log); err != nil {
				log.Error("metrics server exited with error", "error", err)
				return err
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to config YAML (optional; defaults + env still apply)")
	cmd.Flags().StringVar(&openAPIPath, "openapi-spec", "openapi/openapi.yaml", "path to the OpenAPI spec served at /openapi.yaml when dev_mode is enabled")
	return cmd
}

func runMetrics(cfg *config.MetricsRoot, openAPIPath string, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	poolCfg, err := pgxpool.ParseConfig(cfg.Storage.DSN)
	if err != nil {
		return fmt.Errorf("parsing storage DSN: %w", err)
	}
	poolCfg.MaxConns = cfg.Storage.MetricsPool.MaxConns
	poolCfg.MinConns = cfg.Storage.MetricsPool.MinConns
	poolCfg.ConnConfig.RuntimeParams["statement_timeout"] = fmt.Sprintf("%d", cfg.Storage.StatementTimeout.Milliseconds())
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("connecting to storage: %w", err)
	}
	defer pool.Close()

	var resolver tenant.Resolver
	if cfg.Metrics.AuthMode == config.AuthModeAPIKey {
		const apiKeyCacheTTL = 60 * time.Second
		resolver = tenant.NewCache(tenant.NewStore(pool), apiKeyCacheTTL)
	}

	srv := metricsapi.NewServer(cfg.Metrics, storage.NewQueryBuilder(pool), resolver, openAPIPath, log)

	// ReadHeaderTimeout bounds how long a client can hold the connection
	// open while trickling in headers (Slowloris-style DoS).
	const readHeaderTimeout = 10 * time.Second

	httpServer := &http.Server{
		Addr:              cfg.Metrics.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("metrics api listening", "addr", cfg.Metrics.ListenAddr, "auth_mode", cfg.Metrics.AuthMode, "dev_mode", cfg.Metrics.DevMode)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http server: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Metrics.ShutdownGracePeriod)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}
