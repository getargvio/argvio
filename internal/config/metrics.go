package config

import "time"

// MetricsConfig configures the read-oriented query/analysis API. This
// server is internal/trusted-tenant facing (docs/architecture.md) — the
// auth model is looser than public's, but tenant isolation on every query
// is still mandatory (internal/storage's query builder enforces that
// structurally, not via config). Authentication itself (which methods are
// accepted) is global, not per-server — see AuthConfig.
type MetricsConfig struct {
	ListenAddr string    `koanf:"listen_addr"`
	TLS        TLSConfig `koanf:"tls"`

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

func metricsDefaults() map[string]any {
	return map[string]any{
		"metrics.listen_addr": "0.0.0.0:8080",
		"metrics.tls.enabled": false,

		"metrics.query_timeout":            "10s",
		"metrics.max_result_page_size":     1000,
		"metrics.default_result_page_size": 100,
		"metrics.max_time_range_span":      "2160h", // 90d

		"metrics.dev_mode": false,

		"metrics.shutdown_grace_period": "15s",
	}
}
