package otlp

import (
	"context"
	"errors"
	"log/slog"

	"github.com/getargvio/argvio/internal/allowlist"
	"github.com/getargvio/argvio/internal/ratelimit"
	"github.com/getargvio/argvio/internal/storage"
	"github.com/getargvio/argvio/internal/tenant"
)

// APIKeyHeader is the header/metadata key CLI SDKs present their API key
// under, on both gRPC (lowercased automatically by the grpc-go metadata
// package) and HTTP. Standard OTel exporters support arbitrary custom
// headers via OTEL_EXPORTER_OTLP_HEADERS, so no custom transport is needed
// on the SDK side.
const APIKeyHeader = "x-argvio-api-key"

// ErrUnauthenticated covers a missing/unknown/revoked API key.
var ErrUnauthenticated = errors.New("otlp: invalid or missing api key")

// ErrRateLimited means the tenant's request/byte budget is exhausted.
var ErrRateLimited = errors.New("otlp: rate limit exceeded")

// ErrTenantSuspended means the key is valid but the tenant account is not.
var ErrTenantSuspended = errors.New("otlp: tenant suspended")

// core holds everything the gRPC and HTTP handlers share: auth, rate
// limiting, the live allowlist schema, and where to write accepted rows.
// It intentionally does the minimum possible work before rejecting a
// request — reject unknown/revoked keys before doing any OTLP parsing,
// per the public server's threat model (docs/architecture.md).
type core struct {
	resolver     tenant.Resolver
	schemaLoader *allowlist.Loader
	writer       *storage.Writer
	bounds       Bounds
	limiter      *ratelimit.Limiter
	log          *slog.Logger
}

// authenticate validates the API key and rate limit for one request, cheap
// and cache-backed (tenant.Cache) so a hostile retry storm never reaches
// Postgres per-request.
func (c *core) authenticate(ctx context.Context, apiKey string, nBytes int) (*tenant.Resolved, error) {
	if apiKey == "" {
		return nil, ErrUnauthenticated
	}
	resolved, found, err := c.resolver.Resolve(ctx, apiKey, tenant.ScopePublicIngest)
	if err != nil {
		return nil, err
	}
	if !found || !resolved.Active() {
		return nil, ErrUnauthenticated
	}
	if resolved.Tenant.Status != "active" {
		return nil, ErrTenantSuspended
	}
	if !c.limiter.Allow(resolved.Tenant.ID, resolved.Config, nBytes) {
		return nil, ErrRateLimited
	}
	return resolved, nil
}

func (c *core) newPipeline(resolved *tenant.Resolved) *Pipeline {
	return &Pipeline{
		Schema: c.schemaLoader.Current(),
		Tenant: resolved,
		Bounds: c.bounds,
		Writer: c.writer,
		Log:    c.log,
	}
}
