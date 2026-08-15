// Package metricsapi implements the metrics server's read-oriented REST
// API: filtering + aggregation over ingested telemetry, scoped to exactly
// one tenant per request via internal/storage.TenantScope.
//
// REST, not GraphQL: the query shapes here are a small, fixed set of
// well-known aggregations (percentiles, error rate, frequency, cohorts)
// plus one raw-listing endpoint, not an open graph of relations a client
// needs to traverse arbitrarily — GraphQL's main advantage (client-driven
// shape, avoiding over/under-fetching across relations) doesn't pay for
// itself here, while REST keeps the OpenAPI spec, HTTP caching semantics,
// and query-string-based filtering all simpler and more standard for a
// dashboard backend to consume. See docs/architecture.md.
package metricsapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/getargvio/argvio/internal/config"
	"github.com/getargvio/argvio/internal/storage"
	"github.com/getargvio/argvio/internal/tenant"
)

// Server holds everything the HTTP handlers need. Construct via NewServer.
type Server struct {
	QB       *storage.QueryBuilder
	Resolver tenant.Resolver // used only in AuthModeAPIKey

	AuthMode      config.AuthMode
	JWTSigningKey []byte

	MaxResultPageSize     int
	DefaultResultPageSize int
	MaxTimeRangeSpan      time.Duration
	QueryTimeout          time.Duration

	DevMode         bool
	OpenAPISpecPath string

	Log *slog.Logger
}

func NewServer(cfg config.MetricsConfig, qb *storage.QueryBuilder, resolver tenant.Resolver, openAPISpecPath string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		QB:                    qb,
		Resolver:              resolver,
		AuthMode:              cfg.AuthMode,
		JWTSigningKey:         []byte(cfg.JWTSigningKey),
		MaxResultPageSize:     cfg.MaxResultPageSize,
		DefaultResultPageSize: cfg.DefaultResultPageSize,
		MaxTimeRangeSpan:      cfg.MaxTimeRangeSpan,
		QueryTimeout:          cfg.QueryTimeout,
		DevMode:               cfg.DevMode,
		OpenAPISpecPath:       openAPISpecPath,
		Log:                   log,
	}
}

// Handler builds the full routed HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.router()
}
