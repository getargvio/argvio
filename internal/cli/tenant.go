package cli

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/getargvio/argvio/internal/config"
	"github.com/getargvio/argvio/internal/consent"
	"github.com/getargvio/argvio/internal/tenant"
)

func newTenantCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tenant",
		Short: "Manage tenants",
	}
	cmd.AddCommand(newTenantCreateCommand(), newTenantConfigCommand(), newTenantMemberCommand())
	return cmd
}

func newTenantCreateCommand() *cobra.Command {
	var dsn, slug, name string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a tenant",
		RunE: func(cmd *cobra.Command, args []string) error {
			if slug == "" || name == "" {
				return fmt.Errorf("--slug and --name are required")
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
			t, err := store.CreateTenant(ctx, slug, name)
			if err != nil {
				return err
			}
			fmt.Printf("created tenant %s (id=%s) — default tier_ceiling=basic, tier_enforcement_mode=strip\n", t.Slug, t.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "Postgres DSN (defaults to $ARGVIO_STORAGE_DSN)")
	cmd.Flags().StringVar(&slug, "slug", "", "tenant slug (unique)")
	cmd.Flags().StringVar(&name, "name", "", "tenant display name")
	return cmd
}

func newTenantConfigCommand() *cobra.Command {
	var (
		dsn, tenantIDStr, tierCeiling, tierMode string
		rateRPS, rateBPS                        float64
		rateBurst                               int
		retTraces, retLogs, retMetrics          int
		caps                                    [8]int
		strictMetrics                           bool
	)
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Update a tenant's configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			if tenantIDStr == "" {
				return fmt.Errorf("--tenant-id is required")
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

			cfg := tenant.Config{
				StrictMetrics:       strictMetrics,
				TenantID:            tenantID,
				TierCeiling:         tierCeiling,
				TierEnforcementMode: consentModeFromString(tierMode),
			}
			if rateRPS > 0 {
				cfg.RateLimitRequestsPerSec = &rateRPS
			}
			if rateBPS > 0 {
				cfg.RateLimitBytesPerSec = &rateBPS
			}
			if rateBurst > 0 {
				cfg.RateLimitBurst = &rateBurst
			}
			if retTraces > 0 {
				cfg.RetentionTracesDays = &retTraces
			}
			if retLogs > 0 {
				cfg.RetentionLogsDays = &retLogs
			}
			if retMetrics > 0 {
				cfg.RetentionMetricsDays = &retMetrics
			}

			for i, p := range []**int{
				&cfg.MetricCaps.MaxCounterNames, &cfg.MetricCaps.MaxCounterLabelsPerName,
				&cfg.MetricCaps.MaxMeasureNames, &cfg.MetricCaps.MaxMeasureLabelsPerName,
				&cfg.MetricCaps.MaxMetricDims, &cfg.MetricCaps.MaxDimValuesPerKey,
				&cfg.MetricCaps.MaxValueNames, &cfg.MetricCaps.MaxValueLabelsPerName,
			} {
				if caps[i] > 0 {
					*p = &caps[i]
				}
			}
			if d := cfg.MetricCaps.MaxMetricDims; d != nil && *d > config.MaxMetricDimsLimit {
				return fmt.Errorf("--max-metric-dims must be between 1 and %d", config.MaxMetricDimsLimit)
			}

			store := tenant.NewStore(pool)
			if err := store.UpsertConfig(ctx, cfg); err != nil {
				return err
			}
			fmt.Printf("updated config for tenant %s\n", tenantID)
			return nil
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "Postgres DSN (defaults to $ARGVIO_STORAGE_DSN)")
	cmd.Flags().StringVar(&tenantIDStr, "tenant-id", "", "tenant id (required)")
	cmd.Flags().StringVar(&tierCeiling, "tier-ceiling", "basic", "anonymous|basic|full|optin_plus")
	cmd.Flags().StringVar(&tierMode, "tier-mode", "strip", "strip|reject")
	cmd.Flags().Float64Var(&rateRPS, "rate-rps", 0, "override requests/sec (0 = use global default)")
	cmd.Flags().Float64Var(&rateBPS, "rate-bps", 0, "override bytes/sec (0 = use global default)")
	cmd.Flags().IntVar(&rateBurst, "rate-burst", 0, "override burst (0 = use global default)")
	cmd.Flags().IntVar(&retTraces, "retention-traces-days", 0, "override traces retention in days (0 = use global default)")
	cmd.Flags().IntVar(&retLogs, "retention-logs-days", 0, "override logs retention in days (0 = use global default)")
	cmd.Flags().IntVar(&retMetrics, "retention-metrics-days", 0, "override metrics retention in days (0 = use global default)")
	cmd.Flags().IntVar(&caps[0], "max-counter-names", 0, "override public.max_counter_names_per_tenant (0 = use global default)")
	cmd.Flags().IntVar(&caps[1], "max-counter-labels-per-name", 0, "override public.max_counter_labels_per_name (0 = use global default)")
	cmd.Flags().IntVar(&caps[2], "max-measure-names", 0, "override public.max_measure_names_per_tenant (0 = use global default)")
	cmd.Flags().IntVar(&caps[3], "max-measure-labels-per-name", 0, "override public.max_measure_labels_per_name (0 = use global default)")
	cmd.Flags().IntVar(&caps[4], "max-metric-dims", 0, "override public.max_metric_dims, 1 to 8 (0 = use global default)")
	cmd.Flags().BoolVar(&strictMetrics, "strict-metrics", false, "reject vendor metrics the sending CLI's catalog (argvio.metrics.yaml) doesn't declare")
	cmd.Flags().IntVar(&caps[6], "max-value-names", 0, "override public.max_value_names_per_tenant (0 = use global default)")
	cmd.Flags().IntVar(&caps[7], "max-value-labels-per-name", 0, "override public.max_value_labels_per_name (0 = use global default)")
	cmd.Flags().IntVar(&caps[5], "max-dim-values-per-key", 0, "override public.max_dim_values_per_key (0 = use global default)")
	return cmd
}

func consentModeFromString(s string) consent.EnforcementMode {
	if s == "reject" {
		return consent.ModeReject
	}
	return consent.ModeStrip
}
