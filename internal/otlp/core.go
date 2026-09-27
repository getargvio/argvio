package otlp

import (
	"context"
	"errors"
	"log/slog"

	"github.com/getargvio/argvio/internal/allowlist"
	"github.com/getargvio/argvio/internal/authn"
	"github.com/getargvio/argvio/internal/ratelimit"
	"github.com/getargvio/argvio/internal/storage"
)

// CredentialHeader is settable via OTEL_EXPORTER_OTLP_HEADERS.
const CredentialHeader = "x-argvio-api-key"

var ErrUnauthenticated = errors.New("otlp: invalid or missing credential")
var ErrRateLimited = errors.New("otlp: rate limit exceeded")
var ErrTenantSuspended = errors.New("otlp: tenant suspended")

// core holds what the gRPC and HTTP handlers share.
type core struct {
	verifier     *authn.Verifier
	schemaLoader *allowlist.Loader
	writer       *storage.Writer
	bounds       Bounds
	limiter      *ratelimit.Limiter
	log          *slog.Logger
}

// authenticate is cache-backed so a retry storm never reaches Postgres.
func (c *core) authenticate(ctx context.Context, token string, nBytes int) (*authn.Identity, error) {
	id, err := c.verifier.Authenticate(ctx, token)
	switch {
	case err == nil:
	case errors.Is(err, authn.ErrTenantSuspended):
		return nil, ErrTenantSuspended
	case errors.Is(err, authn.ErrUnauthenticated):
		return nil, ErrUnauthenticated
	default:
		return nil, err
	}
	if !c.limiter.Allow(id, nBytes) {
		return nil, ErrRateLimited
	}
	return &id, nil
}

func (c *core) newPipeline(id *authn.Identity) *Pipeline {
	return &Pipeline{
		Schema: c.schemaLoader.Current(),
		Tenant: id.Resolved,
		Bounds: c.bounds,
		Writer: c.writer,
		Log:    c.log,
	}
}
