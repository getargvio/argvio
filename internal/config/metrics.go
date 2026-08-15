package config

import "time"

// AuthMode selects how the metrics server authenticates callers.
type AuthMode string

const (
	AuthModeJWT    AuthMode = "jwt"     // dashboard-user session/JWT
	AuthModeAPIKey AuthMode = "api_key" // scoped tenant API key
)

// MetricsConfig configures the read-oriented query/analysis API. This
// server is internal/trusted-tenant facing (docs/architecture.md) — the
// auth model is looser than public's, but tenant isolation on every query
// is still mandatory (internal/storage's query builder enforces that
// structurally, not via config).
type MetricsConfig struct {
	ListenAddr string    `koanf:"listen_addr"`
	TLS        TLSConfig `koanf:"tls"`

	AuthMode      AuthMode `koanf:"auth_mode"`
	JWTSigningKey string   `koanf:"jwt_signing_key"` // HMAC secret; use a real KMS-backed key in production

	QueryTimeout          time.Duration `koanf:"query_timeout"`
	MaxResultPageSize     int           `koanf:"max_result_page_size"`
	DefaultResultPageSize int           `koanf:"default_result_page_size"`
	// MaxTimeRangeSpan guards against e.g. a 5-year raw scan being requested
	// by an over-broad dashboard query.
	MaxTimeRangeSpan time.Duration `koanf:"max_time_range_span"`

	// DevMode gates /openapi.yaml + the Scalar viewer. Must default false;
	// never flip true in production without an explicit decision.
	DevMode bool `koanf:"dev_mode"`

	ShutdownGracePeriod time.Duration `koanf:"shutdown_grace_period"`
}

// MetricsRoot is the full config document for cmd/metrics.
type MetricsRoot struct {
	LogLevel string        `koanf:"log_level"`
	Metrics  MetricsConfig `koanf:"metrics"`
	Storage  StorageConfig `koanf:"storage"`
}

func metricsDefaults() map[string]any {
	d := map[string]any{
		"log_level": "info",

		"metrics.listen_addr": "0.0.0.0:8080",
		"metrics.tls.enabled": false,

		"metrics.auth_mode":       "jwt",
		"metrics.jwt_signing_key": "",

		"metrics.query_timeout":            "10s",
		"metrics.max_result_page_size":     1000,
		"metrics.default_result_page_size": 100,
		"metrics.max_time_range_span":      "2160h", // 90d

		"metrics.dev_mode": false,

		"metrics.shutdown_grace_period": "15s",
	}
	for k, v := range storageDefaults() {
		d[k] = v
	}
	return d
}

func (c MetricsRoot) Validate() error {
	var errs []string
	if c.Metrics.ListenAddr == "" {
		errs = append(errs, "metrics.listen_addr must not be empty")
	}
	if c.Metrics.TLS.Enabled && (c.Metrics.TLS.CertFile == "" || c.Metrics.TLS.KeyFile == "") {
		errs = append(errs, "metrics.tls.cert_file and key_file are required when metrics.tls.enabled = true")
	}
	switch c.Metrics.AuthMode {
	case AuthModeJWT:
		if c.Metrics.JWTSigningKey == "" {
			errs = append(errs, "metrics.jwt_signing_key must be set when auth_mode = jwt")
		}
	case AuthModeAPIKey:
		// scoped API keys are validated against Postgres at request time;
		// nothing to check at config load beyond the mode itself.
	default:
		errs = append(errs, "metrics.auth_mode must be one of: jwt, api_key")
	}
	if c.Metrics.QueryTimeout <= 0 {
		errs = append(errs, "metrics.query_timeout must be > 0")
	}
	if c.Metrics.MaxResultPageSize <= 0 || c.Metrics.DefaultResultPageSize <= 0 {
		errs = append(errs, "metrics.max_result_page_size and default_result_page_size must be > 0")
	}
	if c.Metrics.DefaultResultPageSize > c.Metrics.MaxResultPageSize {
		errs = append(errs, "metrics.default_result_page_size must not exceed max_result_page_size")
	}
	if c.Metrics.MaxTimeRangeSpan <= 0 {
		errs = append(errs, "metrics.max_time_range_span must be > 0")
	}
	errs = append(errs, c.Storage.validate()...)
	return joinErrors(errs)
}
