package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/getargvio/argvio/internal/config"
	"github.com/getargvio/argvio/internal/storage"
)

func newPoliciesCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policies",
		Short: "Manage Timescale retention/compression policies",
	}
	cmd.AddCommand(newPoliciesApplyCommand())
	return cmd
}

func newPoliciesApplyCommand() *cobra.Command {
	var dsn, configPath string
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Reconcile Timescale compression/retention policies against config",
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveDSN(dsn)
			if err != nil {
				return err
			}
			// config.Validate requires storage.dsn to be set; the CLI's
			// --dsn/$ARGVIO_STORAGE_DSN already resolved it above, so mirror
			// it into the koanf-recognized env var rather than making the
			// operator set both.
			if err := os.Setenv("ARGVIO_STORAGE__DSN", resolved); err != nil {
				return err
			}

			root, err := config.LoadPublic(configPath)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			ctx := context.Background()
			pool, err := connectPool(ctx, resolved)
			if err != nil {
				return err
			}
			defer pool.Close()

			if err := storage.ApplyRetentionAndCompressionPolicies(ctx, pool, root.Storage); err != nil {
				return err
			}
			fmt.Println("policies reconciled")
			return nil
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "Postgres DSN (defaults to $ARGVIO_STORAGE_DSN)")
	cmd.Flags().StringVar(&configPath, "config", "", "path to a config YAML with a storage: section (optional)")
	return cmd
}
