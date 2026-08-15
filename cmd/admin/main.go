// Command admin is the operator CLI for one-off tasks: running migrations,
// seeding a tenant + API key, and reconciling Timescale compression/
// retention policies against config. It is explicitly not a tenant
// onboarding UI (see the task's non-goals) — just enough to stand the
// system up locally or bootstrap a first real tenant.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getargvio/argvio/internal/config"
	"github.com/getargvio/argvio/internal/consent"
	"github.com/getargvio/argvio/internal/storage"
	"github.com/getargvio/argvio/internal/tenant"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "migrate":
		cmdMigrate(os.Args[2:])
	case "tenant":
		cmdTenant(os.Args[2:])
	case "apikey":
		cmdAPIKey(os.Args[2:])
	case "policies":
		cmdPolicies(os.Args[2:])
	case "retention":
		cmdRetention(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `argvio-admin — operator CLI

Usage:
  argvio-admin migrate up   [--dsn DSN]
  argvio-admin migrate down [--dsn DSN]
  argvio-admin migrate version [--dsn DSN]

  argvio-admin tenant create --slug SLUG --name NAME [--dsn DSN]
  argvio-admin tenant config --tenant-id ID [--tier-ceiling TIER] [--tier-mode strip|reject]
                              [--rate-rps N] [--rate-bps N] [--rate-burst N]
                              [--retention-traces-days N] [--retention-logs-days N] [--retention-metrics-days N] [--dsn DSN]

  argvio-admin apikey create --tenant-id ID --scope public_ingest|metrics_query [--dsn DSN]
  argvio-admin apikey revoke --key-id ID [--dsn DSN]

  argvio-admin policies apply [--dsn DSN]

  argvio-admin retention sweep [--dsn DSN]

DSN defaults to $ARGVIO_STORAGE_DSN if --dsn is not given.
`)
}

func resolveDSN(fs *flag.FlagSet) string {
	dsn := fs.Lookup("dsn").Value.String()
	if dsn != "" {
		return dsn
	}
	if env := os.Getenv("ARGVIO_STORAGE_DSN"); env != "" {
		return env
	}
	fmt.Fprintln(os.Stderr, "error: --dsn or $ARGVIO_STORAGE_DSN is required")
	os.Exit(2)
	return ""
}

func connectPool(ctx context.Context, dsn string) *pgxpool.Pool {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: connect: %v\n", err)
		os.Exit(1)
	}
	return pool
}

func cmdMigrate(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: argvio-admin migrate <up|down|version> [--dsn DSN]")
		os.Exit(2)
	}
	verb := args[0]

	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	dsnFlag := fs.String("dsn", "", "Postgres DSN")
	_ = fs.Parse(args[1:])
	_ = dsnFlag
	dsn := resolveDSN(fs)

	switch verb {
	case "up":
		if err := storage.MigrateUp(dsn); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("migrations applied")
	case "down":
		if err := storage.MigrateDown(dsn); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("migrations rolled back")
	case "version":
		v, dirty, err := storage.MigrationVersion(dsn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("version=%d dirty=%v\n", v, dirty)
	default:
		fmt.Fprintf(os.Stderr, "unknown migrate subcommand %q\n", verb)
		os.Exit(2)
	}
}

func cmdTenant(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: argvio-admin tenant <create|config> ...")
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		cmdTenantCreate(args[1:])
	case "config":
		cmdTenantConfig(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown tenant subcommand %q\n", args[0])
		os.Exit(2)
	}
}

func cmdTenantCreate(args []string) {
	fs := flag.NewFlagSet("tenant create", flag.ExitOnError)
	dsnFlag := fs.String("dsn", "", "Postgres DSN")
	slug := fs.String("slug", "", "tenant slug (unique)")
	name := fs.String("name", "", "tenant display name")
	_ = fs.Parse(args)
	_ = dsnFlag
	if *slug == "" || *name == "" {
		fmt.Fprintln(os.Stderr, "error: --slug and --name are required")
		os.Exit(2)
	}
	dsn := resolveDSN(fs)

	ctx := context.Background()
	pool := connectPool(ctx, dsn)

	store := tenant.NewStore(pool)
	t, err := store.CreateTenant(ctx, *slug, *name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		pool.Close()
		os.Exit(1)
	}
	pool.Close()
	fmt.Printf("created tenant %s (id=%s) — default tier_ceiling=basic, tier_enforcement_mode=strip\n", t.Slug, t.ID)
}

func cmdTenantConfig(args []string) {
	fs := flag.NewFlagSet("tenant config", flag.ExitOnError)
	dsnFlag := fs.String("dsn", "", "Postgres DSN")
	tenantIDStr := fs.String("tenant-id", "", "tenant id (required)")
	tierCeiling := fs.String("tier-ceiling", "basic", "anonymous|basic|full|optin_plus")
	tierMode := fs.String("tier-mode", "strip", "strip|reject")
	rateRPS := fs.Float64("rate-rps", 0, "override requests/sec (0 = use global default)")
	rateBPS := fs.Float64("rate-bps", 0, "override bytes/sec (0 = use global default)")
	rateBurst := fs.Int("rate-burst", 0, "override burst (0 = use global default)")
	retTraces := fs.Int("retention-traces-days", 0, "override traces retention in days (0 = use global default)")
	retLogs := fs.Int("retention-logs-days", 0, "override logs retention in days (0 = use global default)")
	retMetrics := fs.Int("retention-metrics-days", 0, "override metrics retention in days (0 = use global default)")
	_ = fs.Parse(args)
	_ = dsnFlag
	if *tenantIDStr == "" {
		fmt.Fprintln(os.Stderr, "error: --tenant-id is required")
		os.Exit(2)
	}
	tenantID, err := uuid.Parse(*tenantIDStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid --tenant-id: %v\n", err)
		os.Exit(2)
	}
	dsn := resolveDSN(fs)

	ctx := context.Background()
	pool := connectPool(ctx, dsn)

	cfg := tenant.Config{
		TenantID:            tenantID,
		TierCeiling:         *tierCeiling,
		TierEnforcementMode: consentModeFromString(*tierMode),
	}
	if *rateRPS > 0 {
		cfg.RateLimitRequestsPerSec = rateRPS
	}
	if *rateBPS > 0 {
		cfg.RateLimitBytesPerSec = rateBPS
	}
	if *rateBurst > 0 {
		cfg.RateLimitBurst = rateBurst
	}
	if *retTraces > 0 {
		cfg.RetentionTracesDays = retTraces
	}
	if *retLogs > 0 {
		cfg.RetentionLogsDays = retLogs
	}
	if *retMetrics > 0 {
		cfg.RetentionMetricsDays = retMetrics
	}

	store := tenant.NewStore(pool)
	if err := store.UpsertConfig(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		pool.Close()
		os.Exit(1)
	}
	pool.Close()
	fmt.Printf("updated config for tenant %s\n", tenantID)
}

func cmdAPIKey(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: argvio-admin apikey <create|revoke> ...")
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		cmdAPIKeyCreate(args[1:])
	case "revoke":
		cmdAPIKeyRevoke(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown apikey subcommand %q\n", args[0])
		os.Exit(2)
	}
}

func cmdAPIKeyCreate(args []string) {
	fs := flag.NewFlagSet("apikey create", flag.ExitOnError)
	dsnFlag := fs.String("dsn", "", "Postgres DSN")
	tenantIDStr := fs.String("tenant-id", "", "tenant id (required)")
	scope := fs.String("scope", "", "public_ingest|metrics_query (required)")
	_ = fs.Parse(args)
	_ = dsnFlag
	if *tenantIDStr == "" || (*scope != tenant.ScopePublicIngest && *scope != tenant.ScopeMetricsQuery) {
		fmt.Fprintln(os.Stderr, "error: --tenant-id and --scope (public_ingest|metrics_query) are required")
		os.Exit(2)
	}
	tenantID, err := uuid.Parse(*tenantIDStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid --tenant-id: %v\n", err)
		os.Exit(2)
	}
	dsn := resolveDSN(fs)

	ctx := context.Background()
	pool := connectPool(ctx, dsn)

	store := tenant.NewStore(pool)
	raw, key, err := store.CreateAPIKey(ctx, tenantID, *scope)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		pool.Close()
		os.Exit(1)
	}
	pool.Close()
	fmt.Printf("created api key (id=%s, scope=%s)\n", key.ID, key.Scope)
	fmt.Printf("RAW KEY (shown once, store it now): %s\n", raw)
}

func cmdAPIKeyRevoke(args []string) {
	fs := flag.NewFlagSet("apikey revoke", flag.ExitOnError)
	dsnFlag := fs.String("dsn", "", "Postgres DSN")
	keyIDStr := fs.String("key-id", "", "api key id (required)")
	_ = fs.Parse(args)
	_ = dsnFlag
	if *keyIDStr == "" {
		fmt.Fprintln(os.Stderr, "error: --key-id is required")
		os.Exit(2)
	}
	keyID, err := uuid.Parse(*keyIDStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid --key-id: %v\n", err)
		os.Exit(2)
	}
	dsn := resolveDSN(fs)

	ctx := context.Background()
	pool := connectPool(ctx, dsn)

	store := tenant.NewStore(pool)
	if err := store.RevokeAPIKey(ctx, keyID); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		pool.Close()
		os.Exit(1)
	}
	pool.Close()
	fmt.Printf("revoked api key %s\n", keyID)
}

func cmdRetention(args []string) {
	if len(args) < 1 || args[0] != "sweep" {
		fmt.Fprintln(os.Stderr, "usage: argvio-admin retention sweep [--dsn DSN]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("retention sweep", flag.ExitOnError)
	dsnFlag := fs.String("dsn", "", "Postgres DSN")
	_ = fs.Parse(args[1:])
	_ = dsnFlag
	dsn := resolveDSN(fs)

	ctx := context.Background()
	pool := connectPool(ctx, dsn)

	store := tenant.NewStore(pool)
	overrides, err := store.ListRetentionOverrides(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		pool.Close()
		os.Exit(1)
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
				fmt.Fprintf(os.Stderr, "error: sweeping %s for tenant %s: %v\n", t.table, cfg.TenantID, err)
				pool.Close()
				os.Exit(1)
			}
			if n > 0 {
				fmt.Printf("swept %d rows from %s for tenant %s (retention=%dd)\n", n, t.table, cfg.TenantID, *t.days)
			}
			totalDeleted += n
		}
	}
	pool.Close()
	fmt.Printf("retention sweep complete: %d rows deleted across %d tenant overrides\n", totalDeleted, len(overrides))
}

func consentModeFromString(s string) consent.EnforcementMode {
	if s == "reject" {
		return consent.ModeReject
	}
	return consent.ModeStrip
}

func cmdPolicies(args []string) {
	if len(args) < 1 || args[0] != "apply" {
		fmt.Fprintln(os.Stderr, "usage: argvio-admin policies apply [--dsn DSN] [--config path/to/public.yaml]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("policies apply", flag.ExitOnError)
	dsnFlag := fs.String("dsn", "", "Postgres DSN")
	configPath := fs.String("config", "", "path to a config YAML with a storage: section (optional)")
	_ = fs.Parse(args[1:])
	_ = dsnFlag
	dsn := resolveDSN(fs)
	// config.Validate requires storage.dsn to be set; the CLI's --dsn/
	// $ARGVIO_STORAGE_DSN already resolved it above, so mirror it into the
	// koanf-recognized env var rather than making the operator set both.
	os.Setenv("ARGVIO_STORAGE__DSN", dsn)

	root, err := config.LoadPublic(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: loading config: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	pool := connectPool(ctx, dsn)

	if err := storage.ApplyRetentionAndCompressionPolicies(ctx, pool, root.Storage); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		pool.Close()
		os.Exit(1)
	}
	pool.Close()
	fmt.Println("policies reconciled")
}
