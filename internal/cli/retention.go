package cli

import (
	"context"
	"fmt"

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
	cmd := &cobra.Command{
		Use:   "sweep",
		Short: "Sweep rows past their per-tenant retention override",
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
			fmt.Printf("retention sweep complete: %d rows deleted across %d tenant overrides\n", totalDeleted, len(overrides))
			return nil
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "Postgres DSN (defaults to $ARGVIO_STORAGE_DSN)")
	return cmd
}
