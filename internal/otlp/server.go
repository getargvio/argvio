package otlp

import (
	"log/slog"

	"github.com/getargvio/argvio/internal/allowlist"
	"github.com/getargvio/argvio/internal/ratelimit"
	"github.com/getargvio/argvio/internal/storage"
	"github.com/getargvio/argvio/internal/tenant"
)

// Server bundles everything `argvio serve public` needs to stand up both the gRPC and
// HTTP OTLP endpoints against one shared pipeline (auth, rate limiting,
// allowlist, storage).
type Server struct {
	core *core
}

// NewServer wires the public ingest pipeline. resolver is typically a
// *tenant.Cache (not *tenant.Store directly) so the hot path never hits
// Postgres per-request.
func NewServer(
	resolver tenant.Resolver,
	schemaLoader *allowlist.Loader,
	writer *storage.Writer,
	bounds Bounds,
	limiter *ratelimit.Limiter,
	log *slog.Logger,
) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{core: &core{
		resolver:     resolver,
		schemaLoader: schemaLoader,
		writer:       writer,
		bounds:       bounds,
		limiter:      limiter,
		log:          log,
	}}
}
