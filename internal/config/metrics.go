package config

import (
	"net/url"
	"strings"
	"time"
)

// CORSConfig lets a browser SPA on another origin call the metrics API.
// Disabled while AllowedOrigins is empty.
type CORSConfig struct {
	AllowedOrigins []string      `koanf:"allowed_origins"`
	MaxAge         time.Duration `koanf:"max_age"`
}

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

	CORS CORSConfig `koanf:"cors"`

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

		"metrics.cors.allowed_origins": []string{},
		"metrics.cors.max_age":         "10m",

		"metrics.dev_mode": false,

		"metrics.shutdown_grace_period": "15s",
	}
}

func (c CORSConfig) validate() []string {
	var errs []string
	for _, o := range c.AllowedOrigins {
		u, err := url.Parse(o)
		switch {
		case strings.Contains(o, "*"):
			errs = append(errs, "metrics.cors.allowed_origins must list exact origins, wildcards are not supported (got \""+o+"\")")
		case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil:
			errs = append(errs, "metrics.cors.allowed_origins entry \""+o+"\" must be an origin like https://dashboard.example.com")
		case u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Hostname() == "":
			errs = append(errs, "metrics.cors.allowed_origins entry \""+o+"\" must be scheme://host[:port] only, with no path, query, fragment or trailing slash")
		}
	}
	if len(c.AllowedOrigins) > 0 && c.MaxAge < 0 {
		errs = append(errs, "metrics.cors.max_age must be >= 0")
	}
	return errs
}
