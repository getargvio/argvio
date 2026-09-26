package cli

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/getargvio/argvio/internal/tenant"
)

func newAPIKeyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apikey",
		Short: "Manage tenant API keys",
	}
	cmd.AddCommand(newAPIKeyCreateCommand(), newAPIKeyRevokeCommand())
	return cmd
}

func newAPIKeyCreateCommand() *cobra.Command {
	var dsn, tenantIDStr, scope string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create an API key for a tenant",
		RunE: func(cmd *cobra.Command, args []string) error {
			if tenantIDStr == "" || (scope != tenant.ScopePublicIngest && scope != tenant.ScopeMetricsQuery) {
				return fmt.Errorf("--tenant-id and --scope (public_ingest|metrics_query) are required")
			}
			tenantID, err := uuid.Parse(tenantIDStr)
			if err != nil {
				return fmt.Errorf("invalid --tenant-id: %w", err)
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

			store := tenant.NewStore(pool)
			raw, key, err := store.CreateAPIKey(ctx, tenantID, scope)
			if err != nil {
				return err
			}
			fmt.Printf("created api key (id=%s, scope=%s)\n", key.ID, key.Scope)
			fmt.Printf("RAW KEY (shown once, store it now): %s\n", raw)
			return nil
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "Postgres DSN (defaults to $ARGVIO_STORAGE_DSN)")
	cmd.Flags().StringVar(&tenantIDStr, "tenant-id", "", "tenant id (required)")
	cmd.Flags().StringVar(&scope, "scope", "", "public_ingest|metrics_query (required)")
	return cmd
}

func newAPIKeyRevokeCommand() *cobra.Command {
	var dsn, keyIDStr string
	cmd := &cobra.Command{
		Use:   "revoke",
		Short: "Revoke an API key",
		RunE: func(cmd *cobra.Command, args []string) error {
			if keyIDStr == "" {
				return fmt.Errorf("--key-id is required")
			}
			keyID, err := uuid.Parse(keyIDStr)
			if err != nil {
				return fmt.Errorf("invalid --key-id: %w", err)
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

			store := tenant.NewStore(pool)
			if err := store.RevokeAPIKey(ctx, keyID); err != nil {
				return err
			}
			fmt.Printf("revoked api key %s\n", keyID)
			return nil
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "Postgres DSN (defaults to $ARGVIO_STORAGE_DSN)")
	cmd.Flags().StringVar(&keyIDStr, "key-id", "", "api key id (required)")
	return cmd
}
