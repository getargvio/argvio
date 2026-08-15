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

// PublicRoot is the full config document for cmd/public.
type PublicRoot struct {
	LogLevel string        `koanf:"log_level"`
	Public   PublicConfig  `koanf:"public"`
	Storage  StorageConfig `koanf:"storage"`
}

func publicDefaults() map[string]any {
	d := map[string]any{
		"log_level": "info",

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
	for k, v := range storageDefaults() {
		d[k] = v
	}
	return d
}

// Validate fails fast with every problem found, not just the first, so a
// misconfigured deploy gets one useful error instead of a whack-a-mole
// restart loop.
func (c PublicRoot) Validate() error {
	var errs []string
	if c.Public.GRPCListenAddr == "" {
		errs = append(errs, "public.grpc_listen_addr must not be empty")
	}
	if c.Public.HTTPListenAddr == "" {
		errs = append(errs, "public.http_listen_addr must not be empty")
	}
	if c.Public.TLS.Enabled && (c.Public.TLS.CertFile == "" || c.Public.TLS.KeyFile == "") {
		errs = append(errs, "public.tls.cert_file and key_file are required when public.tls.enabled = true")
	}
	if c.Public.MaxBatchSize <= 0 {
		errs = append(errs, "public.max_batch_size must be > 0")
	}
	if c.Public.MaxAttributeCount <= 0 {
		errs = append(errs, "public.max_attribute_count must be > 0")
	}
	if c.Public.MaxAttributeKeyLength <= 0 {
		errs = append(errs, "public.max_attribute_key_length must be > 0")
	}
	if c.Public.MaxAttributeStringValueLength <= 0 {
		errs = append(errs, "public.max_attribute_string_value_length must be > 0")
	}
	if c.Public.MaxTimestampSkewPast <= 0 || c.Public.MaxTimestampSkewFuture <= 0 {
		errs = append(errs, "public.max_timestamp_skew_past and max_timestamp_skew_future must be > 0")
	}
	if c.Public.APIKeyCacheTTL <= 0 {
		errs = append(errs, "public.api_key_cache_ttl must be > 0")
	}
	if c.Public.RateLimitRequestsPerSecond <= 0 {
		errs = append(errs, "public.rate_limit_requests_per_second must be > 0")
	}
	if c.Public.AllowlistSchemaPath == "" {
		errs = append(errs, "public.allowlist_schema_path must not be empty")
	}
	errs = append(errs, c.Storage.validate()...)
	return joinErrors(errs)
}
