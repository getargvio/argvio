package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/getargvio/argvio/internal/catalog"
	"github.com/getargvio/argvio/internal/storage"
)

func newMetricCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "metric",
		Short: "Manage a CLI's metric catalog (argvio.metrics.yaml)",
		Long: `Manage the metric catalog: what each product metric of a CLI means.

The argvio.metrics.yaml file in the CLI's repository is the source of truth:
"apply" replaces the CLI's whole catalog with the file, deprecating metrics
that left it, and nothing edits a definition field by field.`,
	}
	cmd.AddCommand(newMetricValidateCommand(), newMetricApplyCommand(), newMetricListCommand())
	return cmd
}

// readCatalog parses and validates a catalog file.
func readCatalog(path string) (*catalog.File, error) {
	if path == "" {
		return nil, fmt.Errorf("--file is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return catalog.Parse(data)
}

func newMetricValidateCommand() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check an argvio.metrics.yaml file, without a database",
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := readCatalog(file)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %d metrics for %s, valid\n", file, len(f.Metrics), f.Service)
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "argvio.metrics.yaml", "catalog file")
	return cmd
}

// definitionInputs turns a validated catalog file into storage rows.
func definitionInputs(f *catalog.File) ([]storage.MetricDefinitionInput, error) {
	out := make([]storage.MetricDefinitionInput, 0, len(f.Metrics))
	for _, m := range f.Metrics {
		spec, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		out = append(out, storage.MetricDefinitionInput{Name: m.Name, Kind: m.Kind, Spec: spec})
	}
	return out, nil
}

func newMetricApplyCommand() *cobra.Command {
	var dsn, file, tenantIDStr, actor string
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Make a tenant's catalog for the file's CLI exactly the file",
		RunE: func(cmd *cobra.Command, args []string) error {
			tenantID, err := uuid.Parse(tenantIDStr)
			if err != nil {
				return fmt.Errorf("invalid --tenant-id: %w", err)
			}
			f, err := readCatalog(file)
			if err != nil {
				return err
			}
			defs, err := definitionInputs(f)
			if err != nil {
				return err
			}
			if actor == "" {
				actor = "argvio metric apply"
				if u, err := user.Current(); err == nil {
					actor += " (" + u.Username + ")"
				}
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

			res, err := storage.NewWriter(pool).ApplyMetricDefinitions(ctx, tenantID, f.Service, defs, actor, time.Now().UTC())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "applied %s to tenant %s: %d created, %d updated, %d unchanged, %d deprecated\n",
				f.Service, tenantID, len(res.Created), len(res.Updated), len(res.Unchanged), len(res.Deprecated))
			for _, n := range res.Deprecated {
				fmt.Fprintf(cmd.OutOrStdout(), "  deprecated: %s\n", n)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "Postgres DSN (defaults to $ARGVIO_STORAGE_DSN)")
	cmd.Flags().StringVarP(&file, "file", "f", "argvio.metrics.yaml", "catalog file")
	cmd.Flags().StringVar(&tenantIDStr, "tenant-id", "", "tenant id (required)")
	cmd.Flags().StringVar(&actor, "actor", "", "recorded as updated_by (defaults to the OS user)")
	return cmd
}

func newMetricListCommand() *cobra.Command {
	var dsn, tenantIDStr string
	var services []string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List a tenant's metric definitions and the metrics its CLIs send undeclared",
		RunE: func(cmd *cobra.Command, args []string) error {
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
			scope, err := storage.NewTenantScope(tenantID)
			if err != nil {
				return err
			}
			defs, err := storage.NewQueryBuilder(pool).MetricDefinitions(ctx, scope, services, time.Now())
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(defs)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "SERVICE\tNAME\tKIND\tSTATUS")
			for _, d := range defs {
				status := "declared"
				switch {
				case !d.Declared:
					status = "undeclared"
				case d.DeprecatedAt != nil:
					status = "deprecated"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Service, d.Name, d.Kind, status)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "Postgres DSN (defaults to $ARGVIO_STORAGE_DSN)")
	cmd.Flags().StringVar(&tenantIDStr, "tenant-id", "", "tenant id (required)")
	cmd.Flags().StringArrayVar(&services, "service", nil, "only this CLI (repeatable)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}
