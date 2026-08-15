package metricsapi

import (
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func (s *Server) router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	// No middleware.RealIP: it's deprecated (trusts X-Forwarded-For/
	// X-Real-IP unconditionally, which lets a client spoof its logged IP)
	// and nothing here makes security decisions based on client IP —
	// r.RemoteAddr (the actual TCP peer) is enough for access logs.
	r.Use(middleware.Recoverer)
	r.Use(middleware.Logger)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	r.Group(func(r chi.Router) {
		r.Use(s.authMiddleware)
		r.Get("/v1/traces", s.handleListTraces)
		r.Get("/v1/metrics/latency", s.handleLatencyPercentiles)
		r.Get("/v1/metrics/error-rate", s.handleErrorRate)
		r.Get("/v1/metrics/command-frequency", s.handleCommandFrequency)
		r.Get("/v1/metrics/cohorts", s.handleCohorts)
	})

	// OpenAPI spec + viewer: dev-only, unauthenticated (per the task spec —
	// this is a documentation aid for whoever is running the server
	// locally, not a customer-facing route). Never wired up unless
	// MetricsConfig.DevMode is explicitly true.
	if s.DevMode {
		r.Get("/openapi.yaml", s.handleOpenAPISpec)
		r.Get("/docs", s.handleAPIDocsViewer)
	}

	return r
}

func (s *Server) handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	b, err := os.ReadFile(s.OpenAPISpecPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "openapi spec not found")
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(b)
}

const scalarViewerHTML = `<!doctype html>
<html>
<head>
  <title>Argvio Metrics API — dev docs</title>
  <meta charset="utf-8" />
</head>
<body>
  <script id="api-reference" data-url="/openapi.yaml"></script>
  <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
</body>
</html>`

func (s *Server) handleAPIDocsViewer(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	_, _ = w.Write([]byte(scalarViewerHTML))
}
