package config

import "time"

// PublicConfig configures the OTLP ingest server. Every bound here exists
// because the public server treats every incoming request as untrusted —
// see docs/architecture.md.
type PublicConfig struct {
	GRPCListenAddr string    `koanf:"grpc_listen_addr"`
	HTTPListenAddr string    `koanf:"http_listen_addr"`
	TLS            TLSConfig `koanf:"tls"`

	// Layer 1 structural bounds (docs/allowlist.md). Deliberately config,
	// not magic numbers in the ingest code.
	MaxBatchSize                  int           `koanf:"max_batch_size"`
	MaxAttributeCount             int           `koanf:"max_attribute_count"`
	MaxAttributeKeyLength         int           `koanf:"max_attribute_key_length"`
	MaxAttributeStringValueLength int           `koanf:"max_attribute_string_value_length"`
	MaxTimestampSkewPast          time.Duration `koanf:"max_timestamp_skew_past"`
	MaxTimestampSkewFuture        time.Duration `koanf:"max_timestamp_skew_future"`

	// Auth (tenant API key validation on every request).
	APIKeyCacheTTL time.Duration `koanf:"api_key_cache_ttl"`

	// Per-tenant rate limiting (token bucket).
	RateLimitRequestsPerSecond float64 `koanf:"rate_limit_requests_per_second"`
	RateLimitBytesPerSecond    float64 `koanf:"rate_limit_bytes_per_second"`
	RateLimitBurst             int     `koanf:"rate_limit_burst"`

	// Allowlist/tier schema (internal/allowlist).
	AllowlistSchemaPath string `koanf:"allowlist_schema_path"`
	AllowlistHotReload  bool   `koanf:"allowlist_hot_reload"`

	ShutdownGracePeriod time.Duration `koanf:"shutdown_grace_period"`
}

func publicDefaults() map[string]any {
	return map[string]any{
		"public.grpc_listen_addr": "0.0.0.0:4317",
		"public.http_listen_addr": "0.0.0.0:4318",
		"public.tls.enabled":      false,

		"public.max_batch_size":                    1000,
		"public.max_attribute_count":               64,
		"public.max_attribute_key_length":          128,
		"public.max_attribute_string_value_length": 4096,
		"public.max_timestamp_skew_past":           defaultDayDuration,
		"public.max_timestamp_skew_future":         "5m",

		"public.api_key_cache_ttl": "60s",

		"public.rate_limit_requests_per_second": 200.0,
		"public.rate_limit_bytes_per_second":    5_000_000.0,
		"public.rate_limit_burst":               400,

		"public.allowlist_schema_path": "schema/allowlist/v1.yaml",
		"public.allowlist_hot_reload":  true,

		"public.shutdown_grace_period": "15s",
	}
}
