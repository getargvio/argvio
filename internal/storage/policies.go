package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getargvio/argvio/internal/config"
)

// ApplyRetentionAndCompressionPolicies reconciles each hypertable's
// Timescale compression/retention policy to match cfg, so an operator can
// change internal/config.StorageConfig's per-signal windows and roll them
// out with `argvio-admin policies apply` instead of hand-writing SQL.
//
// This governs the *global* (per-hypertable) policy only — a per-tenant
// override shorter than the global window is enforced separately by a
// retention-sweep job, since Timescale's chunk-drop retention policies
// can't be scoped below one hypertable once tenant_id is a hash-partitioned
// dimension (docs/schema.md "Per-tenant retention").
//
// Idempotent: safe to run repeatedly (e.g. from a deploy pipeline) —
// removes then re-adds each policy so an interval change actually takes
// effect, since Timescale's add_*_policy calls don't update an existing
// job's schedule in place.
func ApplyRetentionAndCompressionPolicies(ctx context.Context, pool *pgxpool.Pool, cfg config.StorageConfig) error {
	signals := []struct {
		table  string
		policy config.PerSignalPolicy
	}{
		{TableTraces, cfg.Traces},
		{TableLogs, cfg.Logs},
		{TableMetrics, cfg.Metrics},
	}

	for _, sig := range signals {
		if err := reconcileCompression(ctx, pool, sig.table, sig.policy.CompressionAfter); err != nil {
			return err
		}
		if err := reconcileRetention(ctx, pool, sig.table, sig.policy.RetentionAfter); err != nil {
			return err
		}
	}
	return nil
}

func reconcileCompression(ctx context.Context, pool *pgxpool.Pool, table string, after time.Duration) error {
	if _, err := pool.Exec(ctx, `SELECT remove_compression_policy($1, if_exists => true)`, table); err != nil {
		return fmt.Errorf("storage: remove compression policy for %s: %w", table, err)
	}
	if _, err := pool.Exec(ctx,
		`SELECT add_compression_policy($1, compress_after => $2::interval, if_not_exists => true)`,
		table, intervalLiteral(after)); err != nil {
		return fmt.Errorf("storage: add compression policy for %s: %w", table, err)
	}
	return nil
}

func reconcileRetention(ctx context.Context, pool *pgxpool.Pool, table string, after time.Duration) error {
	if _, err := pool.Exec(ctx, `SELECT remove_retention_policy($1, if_exists => true)`, table); err != nil {
		return fmt.Errorf("storage: remove retention policy for %s: %w", table, err)
	}
	if _, err := pool.Exec(ctx,
		`SELECT add_retention_policy($1, drop_after => $2::interval, if_not_exists => true)`,
		table, intervalLiteral(after)); err != nil {
		return fmt.Errorf("storage: add retention policy for %s: %w", table, err)
	}
	return nil
}

// intervalLiteral renders a Go Duration as an unambiguous Postgres interval
// literal ("N seconds") rather than relying on Postgres to parse Go's
// Duration.String() format ("168h0m0s"), which is not guaranteed to match
// Postgres's interval grammar.
func intervalLiteral(d time.Duration) string {
	return fmt.Sprintf("%d seconds", int64(d.Seconds()))
}
