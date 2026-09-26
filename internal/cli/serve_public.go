package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"github.com/getargvio/argvio/internal/allowlist"
	"github.com/getargvio/argvio/internal/config"
	"github.com/getargvio/argvio/internal/otlp"
	"github.com/getargvio/argvio/internal/ratelimit"
	"github.com/getargvio/argvio/internal/storage"
	"github.com/getargvio/argvio/internal/tenant"
)

// newServePublicCommand runs the OTLP ingest server: gRPC on
// PublicConfig.GRPCListenAddr, OTLP/HTTP on PublicConfig.HTTPListenAddr.
// This is the internet-facing edge — see docs/architecture.md for why it
// runs as a separate process from `serve metrics`.
func newServePublicCommand() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "public",
		Short: "Run the OTLP ingest server (gRPC + HTTP)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadPublic(configPath)
			if err != nil {
				return fmt.Errorf("config error:\n%w", err)
			}
			log := newLogger(cfg.LogLevel)
			if err := runPublic(cfg, log); err != nil {
				log.Error("public server exited with error", "error", err)
				return err
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to config YAML (optional; defaults + env still apply)")
	return cmd
}

func runPublic(cfg *config.PublicRoot, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	schemaLoader, err := allowlist.Load(cfg.Public.AllowlistSchemaPath, log)
	if err != nil {
		return fmt.Errorf("loading allowlist schema: %w", err)
	}
	if cfg.Public.AllowlistHotReload {
		stopWatch := make(chan struct{})
		go func() {
			if err := schemaLoader.Watch(stopWatch); err != nil {
				log.Error("allowlist watcher stopped", "error", err)
			}
		}()
		go func() {
			<-ctx.Done()
			close(stopWatch)
		}()
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.Storage.DSN)
	if err != nil {
		return fmt.Errorf("parsing storage DSN: %w", err)
	}
	poolCfg.MaxConns = cfg.Storage.PublicPool.MaxConns
	poolCfg.MinConns = cfg.Storage.PublicPool.MinConns
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("connecting to storage: %w", err)
	}
	defer pool.Close()

	tenantStore := tenant.NewStore(pool)
	tenantCache := tenant.NewCache(tenantStore, cfg.Public.APIKeyCacheTTL)
	cacheStop := make(chan struct{})
	go tenantCache.Janitor(cfg.Public.APIKeyCacheTTL, cacheStop)
	go func() {
		<-ctx.Done()
		close(cacheStop)
	}()

	limiter := ratelimit.New(ratelimit.Defaults{
		RequestsPerSecond: cfg.Public.RateLimitRequestsPerSecond,
		BytesPerSecond:    cfg.Public.RateLimitBytesPerSecond,
		Burst:             cfg.Public.RateLimitBurst,
	})

	bounds := otlp.Bounds{
		MaxBatchSize:                  cfg.Public.MaxBatchSize,
		MaxAttributeCount:             cfg.Public.MaxAttributeCount,
		MaxAttributeKeyLength:         cfg.Public.MaxAttributeKeyLength,
		MaxAttributeStringValueLength: cfg.Public.MaxAttributeStringValueLength,
		MaxTimestampSkewPast:          cfg.Public.MaxTimestampSkewPast,
		MaxTimestampSkewFuture:        cfg.Public.MaxTimestampSkewFuture,
	}

	srv := otlp.NewServer(tenantCache, schemaLoader, storage.NewWriter(pool), bounds, limiter, log)

	grpcServer := grpc.NewServer()
	srv.RegisterGRPC(grpcServer)
	grpcLis, err := net.Listen("tcp", cfg.Public.GRPCListenAddr)
	if err != nil {
		return fmt.Errorf("listening on grpc addr %s: %w", cfg.Public.GRPCListenAddr, err)
	}

	// ReadHeaderTimeout bounds how long a client can hold the connection
	// open while trickling in headers (Slowloris-style DoS).
	const readHeaderTimeout = 10 * time.Second

	httpServer := &http.Server{
		Addr:              cfg.Public.HTTPListenAddr,
		Handler:           srv.HTTPHandler(),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	errCh := make(chan error, 2)
	go func() {
		log.Info("otlp grpc listening", "addr", cfg.Public.GRPCListenAddr)
		if err := grpcServer.Serve(grpcLis); err != nil {
			errCh <- fmt.Errorf("grpc server: %w", err)
		}
	}()
	go func() {
		log.Info("otlp http listening", "addr", cfg.Public.HTTPListenAddr)
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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Public.ShutdownGracePeriod)
	defer cancel()
	grpcServer.GracefulStop()
	return httpServer.Shutdown(shutdownCtx)
}
