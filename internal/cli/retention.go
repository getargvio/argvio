package cli

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/getargvio/argvio/internal/storage"
	"github.com/getargvio/argvio/internal/tenant"
)

func newRetentionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "retention",
		Short: "Manage per-tenant retention overrides",
	}
	cmd.AddCommand(newRetentionSweepCommand())
	return cmd
}

func newRetentionSweepCommand() *cobra.Command {
	var dsn string
	var defaultLogsDays int
	cmd := &cobra.Command{
		Use:   "sweep",
		Short: "Sweep rows past their per-tenant retention override",
		Long: `Sweep rows past their per-tenant retention override.

Also purges install state (the installs table, with its version changes and
feature/flag discoveries) and unused error reasons by last_seen: tenants with a
logs retention override use it, every other tenant uses --default-logs-days,
which should match storage.logs.retention_after.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveDSN(dsn)
			if err != nil {
				return err
			}

			ctx := context.Background()
			pool, err := connectPool(ctx, resolved)
			if err != nil {
				return err
			}
			defer pool.Close()

			store := tenant.NewStore(pool)
			overrides, err := store.ListRetentionOverrides(ctx)
			if err != nil {
				return err
			}

			type target struct {
				table string
				days  *int
			}
			var totalDeleted int64
			for _, cfg := range overrides {
				for _, t := range []target{
					{storage.TableTraces, cfg.RetentionTracesDays},
					{storage.TableLogs, cfg.RetentionLogsDays},
					{storage.TableMetrics, cfg.RetentionMetricsDays},
				} {
					if t.days == nil {
						continue
					}
					n, err := storage.SweepTenantRetention(ctx, pool, t.table, cfg.TenantID, *t.days)
					if err != nil {
						return fmt.Errorf("sweeping %s for tenant %s: %w", t.table, cfg.TenantID, err)
					}
					if n > 0 {
						fmt.Printf("swept %d rows from %s for tenant %s (retention=%dd)\n", n, t.table, cfg.TenantID, *t.days)
					}
					totalDeleted += n
				}
			}

			var withLogsOverride []uuid.UUID
			for _, cfg := range overrides {
				if cfg.RetentionLogsDays == nil {
					continue
				}
				withLogsOverride = append(withLogsOverride, cfg.TenantID)
				n, err := storage.SweepTenantInstalls(ctx, pool, cfg.TenantID, *cfg.RetentionLogsDays)
				if err != nil {
					return fmt.Errorf("sweeping installs for tenant %s: %w", cfg.TenantID, err)
				}
				if n > 0 {
					fmt.Printf("swept %d installs for tenant %s (retention=%dd)\n", n, cfg.TenantID, *cfg.RetentionLogsDays)
				}
				totalDeleted += n
			}
			n, err := storage.SweepInstallsExcept(ctx, pool, defaultLogsDays, withLogsOverride)
			if err != nil {
				return fmt.Errorf("sweeping installs: %w", err)
			}
			if n > 0 {
				fmt.Printf("swept %d installs past the default %dd window\n", n, defaultLogsDays)
			}
			totalDeleted += n

			fmt.Printf("retention sweep complete: %d rows deleted across %d tenant overrides\n", totalDeleted, len(overrides))
			return nil
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "Postgres DSN (defaults to $ARGVIO_STORAGE_DSN)")
	cmd.Flags().IntVar(&defaultLogsDays, "default-logs-days", 90, "install retention in days for tenants without a logs override")
	return cmd
}
