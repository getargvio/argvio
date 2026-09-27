package otlp

import (
	"log/slog"

	"github.com/getargvio/argvio/internal/allowlist"
	"github.com/getargvio/argvio/internal/authn"
	"github.com/getargvio/argvio/internal/ratelimit"
	"github.com/getargvio/argvio/internal/storage"
)

// Server bundles everything `argvio serve public` needs to stand up both the gRPC and
// HTTP OTLP endpoints against one shared pipeline (auth, rate limiting,
// allowlist, storage).
type Server struct {
	core *core
}

// NewServer wires the public ingest pipeline.
func NewServer(
	verifier *authn.Verifier,
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
		verifier:     verifier,
		schemaLoader: schemaLoader,
		writer:       writer,
		bounds:       bounds,
		limiter:      limiter,
		log:          log,
	}}
}
