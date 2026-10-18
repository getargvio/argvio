package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/getargvio/argvio/internal/storage"
)

func newInstallsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "installs",
		Short: "Manage derived per-install state",
	}
	cmd.AddCommand(newInstallsBackfillCommand())
	return cmd
}

func newInstallsBackfillCommand() *cobra.Command {
	var dsn string
	var days int
	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Rebuild install state from stored logs",
		Long: `Rebuild the installs table from cli.session.started and
cli.command.completed logs still in storage, oldest first, and the
install_discoveries table (first use of each feature and flag) from
cli.feature.activated and cli.command.invoked logs.

Run it once after migrating to 0006 (installs) and 0008 (discoveries). It is
safe to rerun, but version changes older than what live ingest has since
recorded for an install can't be recovered by a rerun. Installs and
discoveries older than log retention don't appear.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if days <= 0 {
				return fmt.Errorf("--days must be positive")
			}
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

			n, err := storage.BackfillInstalls(ctx, pool, time.Now().Add(-time.Duration(days)*24*time.Hour))
			if err != nil {
				return err
			}
			fmt.Printf("installs backfill complete: replayed %d events\n", n)

			d, err := storage.BackfillDiscoveries(ctx, pool, time.Now().Add(-time.Duration(days)*24*time.Hour))
			if err != nil {
				return err
			}
			fmt.Printf("discoveries backfill complete: replayed %d first uses\n", d)
			return nil
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "Postgres DSN (defaults to $ARGVIO_STORAGE_DSN)")
	cmd.Flags().IntVar(&days, "days", 90, "how many days of logs to replay")
	return cmd
}
