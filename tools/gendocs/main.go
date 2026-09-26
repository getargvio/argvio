// Command gendocs generates docs/configuration.md from the actual
// internal/config structs, via reflection over PublicRoot/MetricsRoot and
// their koanf tags — so the key names, env var names, and default values in
// the doc can never drift from what LoadPublic/LoadMetrics actually parse.
//
// Field descriptions are hand-authored (colocated below, not derivable from
// reflection alone since many of this codebase's doc comments cover a
// group of fields rather than one each) — reviewed alongside config.go/
// public.go/metrics.go whenever a field is added or changed, same as any
// other code review.
//
// Run: go run ./tools/gendocs > docs/configuration.md
package main

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/getargvio/argvio/internal/config"
)

type row struct {
	Path    string
	EnvVar  string
	Type    string
	Default string
}

// descriptions is keyed by dotted koanf path. Missing entries render as
// "—" rather than failing the build — a new field is still documented
// (key/env/default/type), just without prose until someone adds one here.
var descriptions = map[string]string{
	"log_level": "Log level: debug, info, warn, error.",

	"public.grpc_listen_addr": "OTLP/gRPC listen address.",
	"public.http_listen_addr": "OTLP/HTTP listen address.",
	"public.tls.enabled":      "Enable TLS on both listeners.",
	"public.tls.cert_file":    "PEM certificate path (required if tls.enabled).",
	"public.tls.key_file":     "PEM private key path (required if tls.enabled).",

	"public.max_batch_size":                    "Max spans/log records/data points per OTLP export request. Over this, the whole request is rejected (not partial success).",
	"public.max_attribute_count":               "Max attributes per resource or per record (span/log record/data point). Over this, the record is rejected.",
	"public.max_attribute_key_length":          "Max attribute key length in bytes. Longer keys are dropped (record kept).",
	"public.max_attribute_string_value_length": "Max string attribute value length in bytes. Longer values are dropped (record kept).",
	"public.max_timestamp_skew_past":           "Reject a record whose timestamp is older than now minus this.",
	"public.max_timestamp_skew_future":         "Reject a record whose timestamp is later than now plus this.",

	"public.api_key_cache_ttl": "TTL for the in-memory API-key resolution cache (positive and negative results).",

	"public.rate_limit_requests_per_second": "Default per-tenant request rate limit (token bucket). Overridable per tenant in tenant_config.",
	"public.rate_limit_bytes_per_second":    "Default per-tenant byte-rate limit. Overridable per tenant.",
	"public.rate_limit_burst":               "Default per-tenant request-burst size. Overridable per tenant.",

	"public.allowlist_schema_path": "Path to the versioned allowlist/tier schema YAML (see docs/allowlist.md).",
	"public.allowlist_hot_reload":  "Watch allowlist_schema_path and hot-reload on change without a restart.",

	"public.shutdown_grace_period": "Graceful-shutdown timeout for the HTTP listener on SIGTERM/SIGINT (gRPC uses GracefulStop with no separate timeout).",

	"metrics.listen_addr":   "Metrics REST API listen address.",
	"metrics.tls.enabled":   "Enable TLS on the listener.",
	"metrics.tls.cert_file": "PEM certificate path (required if tls.enabled).",
	"metrics.tls.key_file":  "PEM private key path (required if tls.enabled).",

	"metrics.auth_mode":       "jwt (dashboard-user session, HS256, requires jwt_signing_key) or api_key (scoped API key, scope=metrics_query).",
	"metrics.jwt_signing_key": "HMAC secret for verifying dashboard JWTs. Required when auth_mode=jwt.",

	"metrics.query_timeout":            "Per-query server-side timeout.",
	"metrics.max_result_page_size":     "Hard ceiling on ?limit=; requests above this are clamped.",
	"metrics.default_result_page_size": "?limit= value used when the caller omits it.",
	"metrics.max_time_range_span":      "Reject a query whose to-from exceeds this (guards against an unbounded raw scan).",

	"metrics.dev_mode": "Serve /openapi.yaml and /docs (Scalar viewer), unauthenticated. Must default false; never enable in production without an explicit decision.",

	"metrics.shutdown_grace_period": "Graceful-shutdown timeout for the HTTP listener on SIGTERM/SIGINT.",

	"storage.dsn":                    "Postgres/Timescale connection string (postgres://...). No default — must be set.",
	"storage.public_pool.max_conns":  "Max connections in the public server's pgx pool.",
	"storage.public_pool.min_conns":  "Min (kept-warm) connections in the public server's pgx pool.",
	"storage.metrics_pool.max_conns": "Max connections in the metrics server's pgx pool. Independent from public_pool so a slow analytical query can never starve ingest.",
	"storage.metrics_pool.min_conns": "Min (kept-warm) connections in the metrics server's pgx pool.",
	"storage.statement_timeout":      "Postgres statement_timeout applied to the metrics server's connections.",

	"storage.traces.chunk_interval":     "Timescale hypertable chunk_time_interval for traces, applied at hypertable-creation time only (migrations/0002).",
	"storage.traces.compression_after":  "Global compression policy age threshold for traces (see docs/schema.md).",
	"storage.traces.retention_after":    "Global retention (chunk-drop) policy age threshold for traces.",
	"storage.logs.chunk_interval":       "Timescale hypertable chunk_time_interval for logs.",
	"storage.logs.compression_after":    "Global compression policy age threshold for logs.",
	"storage.logs.retention_after":      "Global retention policy age threshold for logs.",
	"storage.metrics.chunk_interval":    "Timescale hypertable chunk_time_interval for metrics.",
	"storage.metrics.compression_after": "Global compression policy age threshold for metrics.",
	"storage.metrics.retention_after":   "Global retention policy age threshold for metrics.",
}

func main() {
	publicRows := walk(reflect.TypeOf(config.PublicRoot{}), "", config.PublicDefaults())
	metricsRows := walk(reflect.TypeOf(config.MetricsRoot{}), "", config.MetricsDefaults())

	fmt.Println("# Configuration reference")
	fmt.Println()
	fmt.Println("Generated from `internal/config`'s structs (`go run ./tools/gendocs`) — key names, env var names, types, and defaults come directly from the code that parses them; only the descriptions are hand-authored. Regenerate after changing any config struct.")
	fmt.Println()
	fmt.Println("Layering: **defaults → YAML config file → environment variable overrides** (highest precedence wins). Env vars use prefix `ARGVIO_` and `__` as the nesting delimiter (plain `_` is legal inside a key name), e.g. `ARGVIO_STORAGE__PUBLIC_POOL__MAX_CONNS`.")
	fmt.Println()

	printSection("## `public` server (`argvio serve public`)", publicRows)
	printSection("## `metrics` server (`argvio serve metrics`)", metricsRows)

	fmt.Println("## Per-tenant overrides")
	fmt.Println()
	fmt.Println("Stored in Postgres (`tenant_config` table), not static config — they change per-customer at runtime via `argvio tenant config`. See docs/schema.md.")
	fmt.Println()
	fmt.Println("| Column | Meaning |")
	fmt.Println("|---|---|")
	fmt.Println("| `tier_ceiling` | Max consent tier this tenant may send (anonymous/basic/full/optin_plus). |")
	fmt.Println("| `tier_enforcement_mode` | `strip` or `reject` when a record's declared tier exceeds the ceiling. |")
	fmt.Println("| `rate_limit_requests_per_sec` / `rate_limit_bytes_per_sec` / `rate_limit_burst` | Override the public server's global rate-limit defaults. NULL = use global default. |")
	fmt.Println("| `retention_traces_days` / `retention_logs_days` / `retention_metrics_days` | Shorter-than-global retention, enforced by `argvio retention sweep` (see docs/schema.md). NULL = use the global Timescale retention policy. |")
}

func printSection(header string, rows []row) {
	fmt.Println(header)
	fmt.Println()
	fmt.Println("| Key | Env var | Type | Default | Description |")
	fmt.Println("|---|---|---|---|---|")
	for _, r := range rows {
		desc := descriptions[r.Path]
		if desc == "" {
			desc = "—"
		}
		fmt.Printf("| `%s` | `%s` | %s | `%s` | %s |\n", r.Path, r.EnvVar, r.Type, r.Default, desc)
	}
	fmt.Println()
}

func walk(t reflect.Type, prefix string, defaults map[string]any) []row {
	var rows []row
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("koanf")
		if tag == "" || tag == "-" {
			continue
		}
		path := tag
		if prefix != "" {
			path = prefix + "." + tag
		}

		ft := f.Type
		if ft.Kind() == reflect.Struct && ft != reflect.TypeOf(time.Duration(0)) {
			rows = append(rows, walk(ft, path, defaults)...)
			continue
		}

		rows = append(rows, row{
			Path:    path,
			EnvVar:  envVarName(path),
			Type:    typeName(ft),
			Default: formatDefault(defaults[path]),
		})
	}
	return rows
}

func envVarName(path string) string {
	return "ARGVIO_" + strings.ToUpper(strings.ReplaceAll(path, ".", "__"))
}

func typeName(t reflect.Type) string {
	if t == reflect.TypeOf(time.Duration(0)) {
		return "duration"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int32, reflect.Int64:
		return "int"
	case reflect.Float64:
		return "float"
	default:
		return t.String()
	}
}

func formatDefault(v any) string {
	if v == nil {
		return ""
	}
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprintf("%v", v)
}
