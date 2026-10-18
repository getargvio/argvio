// Command gendocs generates docs/configuration.md from the actual
// internal/config structs, via reflection over config.Root and its koanf
// tags — so the key names, env var names, and default values in the doc can
// never drift from what config.Load actually parses.
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

	"auth.methods": "Which auth methods this deployment accepts, in any combination: api_key (scoped tenant API key), jwt (dashboard-user session, HS256, requires jwt_signing_key), oidc (OAuth2 access tokens from a third-party OIDC provider, as JWTs or, with oidc.introspection, opaque tokens; requires oidc.issuer_url and oidc.audience). Global — shared by both the public and metrics servers; each still only accepts its own tenant API-key scope (public_ingest vs metrics_query). A token is routed by shape, not tried against each method: an `argv_` prefix is an API key, a three-part JWS is jwt (HS256) or oidc (asymmetric alg), and any other token is introspected if oidc.introspection.enabled, else treated as an API key.",

	"auth.jwt_signing_key": "HMAC secret for verifying jwt-method tokens. Required when auth.methods includes jwt.",

	"auth.oidc.issuer_url":      "OIDC issuer, exactly as the provider's discovery document states it. A trailing slash is significant (`https://idp.example.com/` and `https://idp.example.com` are different issuers): the value is compared byte for byte with the discovery `issuer`, each token's `iss`, and `tenant_members.issuer`. Discovery and the JWKS are fetched (and cached) at request time. Required when auth.methods includes oidc.",
	"auth.oidc.audience":        "Resource identifier of the metrics API (e.g. `https://api.example.com`), which every token's `aud` must contain. It must NOT equal any SPA's `client_id`: an ID token's `aud` is the client_id, so keeping the two different is what stops an ID token being accepted as an access token. Required when auth.methods includes oidc.",
	"auth.oidc.required_scopes": "OAuth2 scopes that must all be present (via the `scope` or `scp` claim, or the introspection response) for a token to be accepted; a token that lacks one gets 403 `insufficient_scope`. Empty means no scope requirement.",
	"auth.oidc.jwks_cache_ttl":  "How long a fetched JWKS is cached before being refreshed. If a refresh fails, the previous keys keep being used. A token naming an unrecognized `kid` triggers one immediate refresh regardless of this TTL (to pick up key rotation), at most once per 10s so random kids cannot turn into a flood of requests to the provider.",

	"auth.oidc.require_access_token_typ": "When true, a JWT whose header has a `typ` other than `at+jwt` (RFC 9068) is rejected, as an extra guard against ID tokens. A token with no `typ` header is still judged by its audience. Leave false for providers that do not set `typ` to `at+jwt` on access tokens.",
	"auth.oidc.trust_tenant_id_claim":    "Unsafe-by-design escape hatch, off by default. Normally a user's tenant comes from tenant_members (see `argvio tenant member`) and a `tenant_id` claim, if the token carries one, must itself be a membership or the request gets 403 `no_tenant_access`. When true, a `tenant_id` claim selects the tenant without a membership, trusting the provider to assign it.",

	"auth.oidc.introspection.enabled":            "Verify opaque (non-JWT, non-`argv_`) access tokens with the provider's RFC 7662 introspection endpoint. Requires oidc in auth.methods and client_id/client_secret. A token is accepted only if `active`, its `aud` contains oidc.audience (the response must include `aud`), `iss` (when present) matches, `exp`/`nbf` are valid and required_scopes are present.",
	"auth.oidc.introspection.endpoint":           "Introspection endpoint URL. Defaults to the discovery document's `introspection_endpoint`; startup fails if introspection is enabled and neither is available. Redirects are never followed.",
	"auth.oidc.introspection.client_id":          "Client ID this API uses to authenticate to the introspection endpoint (client_secret_basic). Required when introspection is enabled.",
	"auth.oidc.introspection.client_secret":      "Client secret for the introspection endpoint. Required when introspection is enabled; prefer the ARGVIO_AUTH__OIDC__INTROSPECTION__CLIENT_SECRET env var over a file.",
	"auth.oidc.introspection.cache_ttl":          "How long an active introspection result is reused, capped by the token's own `exp`. Results are cached by the SHA-256 of the token (the token itself is never stored or logged). This is also the revocation latency: a token revoked at the provider may keep working for up to this long.",
	"auth.oidc.introspection.negative_cache_ttl": "How long a rejection (inactive, wrong audience, missing scope) is remembered, to blunt retry storms with a bad token. Provider failures are never cached.",

	"public.grpc_listen_addr":                          "OTLP/gRPC listen address.",
	"public.http_listen_addr":                          "OTLP/HTTP listen address.",
	"auth.admin.oidc.issuer_url":                       "Admin identities (metrics server only; off unless an admin recognition rule is configured, see docs/architecture.md): issuer that verifies admin access tokens. Unset = auth.oidc.issuer_url, i.e. the same IdP, which is the common case. Same exact-match rules as auth.oidc.issuer_url.",
	"auth.admin.oidc.audience":                         "Audience every admin token's `aud` must contain. Unset = auth.oidc.audience. Set it to a different resource identifier to keep admin tokens distinct from user tokens, so a user token can never be an admin token.",
	"auth.admin.oidc.required_scopes":                  "Scopes an admin token must carry. Empty = auth.oidc.required_scopes (so it cannot be used to require none).",
	"auth.admin.oidc.jwks_cache_ttl":                   "Unset = auth.oidc.jwks_cache_ttl.",
	"auth.admin.oidc.introspection.enabled":            "Verify opaque admin tokens by introspection. Unset = auth.oidc.introspection.enabled when the admin issuer is the same as auth.oidc.issuer_url, otherwise false.",
	"auth.admin.oidc.introspection.endpoint":           "Unset = auth.oidc.introspection.endpoint when the admin issuer is the same, otherwise the admin issuer's discovery document.",
	"auth.admin.oidc.introspection.client_id":          "Unset = auth.oidc.introspection.client_id when the admin issuer is the same. A different issuer never inherits it, so one provider's client credentials are never sent to another.",
	"auth.admin.oidc.introspection.client_secret":      "Unset = auth.oidc.introspection.client_secret when the admin issuer is the same; never inherited by a different issuer. Prefer the ARGVIO_AUTH__ADMIN__OIDC__INTROSPECTION__CLIENT_SECRET env var over a file.",
	"auth.admin.oidc.introspection.cache_ttl":          "Unset = auth.oidc.introspection.cache_ttl. Also the revocation latency for admin tokens.",
	"auth.admin.oidc.introspection.negative_cache_ttl": "Unset = auth.oidc.introspection.negative_cache_ttl.",

	"public.tls.enabled":   "Enable TLS on both listeners.",
	"public.tls.cert_file": "PEM certificate path (required if tls.enabled).",
	"public.tls.key_file":  "PEM private key path (required if tls.enabled).",

	"public.max_batch_size":                    "Max spans/log records/data points per OTLP export request. Over this, the whole request is rejected (not partial success).",
	"public.max_attribute_count":               "Max attributes per resource or per record (span/log record/data point). Over this, the record is rejected.",
	"public.max_attribute_key_length":          "Max attribute key length in bytes. Longer keys are dropped (record kept).",
	"public.max_attribute_string_value_length": "Max string attribute value length in bytes. Longer values are dropped (record kept).",
	"public.max_timestamp_skew_past":           "Reject a record whose timestamp is older than now minus this.",
	"public.max_timestamp_skew_future":         "Reject a record whose timestamp is later than now plus this.",

	"public.api_key_cache_ttl": "TTL for the in-memory tenant identity resolution cache — API-key lookups, tenant lookups for jwt callers, and tenant_members lookups for oidc callers (positive and negative results). Not used by `serve metrics`, which caches for a fixed 60s. A membership added or removed with `argvio tenant member` takes effect within this long.",

	"public.rate_limit_requests_per_second": "Default request rate limit (token bucket) for api_key-authenticated requests. Overridable per tenant in tenant_config.",
	"public.rate_limit_bytes_per_second":    "Default byte-rate limit for api_key-authenticated requests. Overridable per tenant.",
	"public.rate_limit_burst":               "Default request-burst size for api_key-authenticated requests. Overridable per tenant.",

	"public.jwt_rate_limit_requests_per_second": "Request rate limit for jwt/oidc-authenticated requests. 0 (default) means \"use rate_limit_requests_per_second instead\". Overridable per tenant in tenant_config.",
	"public.jwt_rate_limit_bytes_per_second":    "Byte-rate limit for jwt/oidc-authenticated requests. 0 (default) means \"use rate_limit_bytes_per_second instead\".",
	"public.jwt_rate_limit_burst":               "Request-burst size for jwt/oidc-authenticated requests. 0 (default) means \"use rate_limit_burst instead\".",

	"public.allowlist_schema_path": "Path to the versioned allowlist/tier schema YAML (see docs/allowlist.md).",
	"public.allowlist_hot_reload":  "Watch allowlist_schema_path and hot-reload on change without a restart.",

	"public.shutdown_grace_period": "Graceful-shutdown timeout for the HTTP listener on SIGTERM/SIGINT (gRPC uses GracefulStop with no separate timeout).",

	"metrics.listen_addr":   "Metrics REST API listen address.",
	"metrics.tls.enabled":   "Enable TLS on the listener.",
	"metrics.tls.cert_file": "PEM certificate path (required if tls.enabled).",
	"metrics.tls.key_file":  "PEM private key path (required if tls.enabled).",

	"metrics.query_timeout":            "Per-query server-side timeout.",
	"metrics.max_result_page_size":     "Hard ceiling on ?limit=; requests above this are clamped.",
	"metrics.default_result_page_size": "?limit= value used when the caller omits it.",
	"metrics.max_time_range_span":      "Reject a query whose to-from exceeds this (guards against an unbounded raw scan).",

	"metrics.cors.allowed_origins": "Browser origins allowed to call the API cross-origin, e.g. `https://dashboard.example.com`; comma-separated in the env var. Empty (default) disables CORS. Matched exactly: each entry must be scheme://host[:port] with no path or trailing slash, and `*` is not supported. Credentials are never allowed, methods are GET/OPTIONS, request headers are Authorization, Content-Type and X-Argvio-Tenant, and WWW-Authenticate is exposed. Preflight requests are answered (204) before authentication, so they need no token.",
	"metrics.cors.max_age":         "How long a browser may cache a preflight answer (Access-Control-Max-Age; browsers cap this, e.g. Chrome at 2h).",

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
	rows := walk(reflect.TypeOf(config.Root{}), "", config.Defaults())
	publicRows := filterRows(rows, "log_level", "auth.", "public.", "storage.")
	metricsRows := filterRows(rows, "log_level", "auth.", "metrics.", "storage.")

	fmt.Println("# Configuration reference")
	fmt.Println()
	fmt.Println("Generated from `internal/config`'s structs (`go run ./tools/gendocs`) — key names, env var names, types, and defaults come directly from the code that parses them; only the descriptions are hand-authored. Regenerate after changing any config struct.")
	fmt.Println()
	fmt.Println("Layering: **defaults → YAML config file(s) → environment variable overrides** (highest precedence wins). One config document covers every subcommand (`public`, `metrics`, and `storage` sections together) — pass it with `argvio --config path/to/argvio.yaml <command>` (`-c` repeatable: later files override earlier ones), mirroring ory/hydra's single `hydra.yml`. See [`argvio.example.yaml`](../argvio.example.yaml) for a full example. Env vars use prefix `ARGVIO_` and `__` as the nesting delimiter (plain `_` is legal inside a key name), e.g. `ARGVIO_STORAGE__PUBLIC_POOL__MAX_CONNS`. List-valued keys take a comma-separated value in an env var, e.g. `ARGVIO_METRICS__CORS__ALLOWED_ORIGINS=https://a.example.com,https://b.example.com`.")
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

// filterRows keeps rows whose Path is exactly one of exact, or starts with
// one of prefixes — used to split config.Root's single flat field list back
// into the "public server" / "metrics server" doc sections (both of which
// also document the shared storage.* section).
func filterRows(rows []row, keep ...string) []row {
	var out []row
	for _, r := range rows {
		for _, k := range keep {
			if r.Path == k || strings.HasPrefix(r.Path, k) {
				out = append(out, r)
				break
			}
		}
	}
	return out
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
	case reflect.Pointer:
		return typeName(t.Elem())
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int32, reflect.Int64:
		return "int"
	case reflect.Float64:
		return "float"
	case reflect.Slice:
		return "[]" + typeName(t.Elem())
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
