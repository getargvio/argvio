package metricsapi

import (
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"github.com/getargvio/argvio/internal/storage"
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

	// Registered before any auth: a browser sends the CORS preflight without
	// a token, and the allow/deny decision must not depend on credentials.
	if h := s.corsHandler(); h != nil {
		r.Use(h)
	}

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// About the user, not a tenant's data: needs no tenant to be chosen yet.
	r.Group(func(r chi.Router) {
		r.Use(s.userMiddleware)
		r.Get("/v1/tenants", s.handleTenants)
	})

	r.Group(func(r chi.Router) {
		r.Use(s.authMiddleware)
		r.Get("/v1/traces", s.handleListTraces)
		r.Get("/v1/meta/dimensions", s.handleDimensions)
		r.Get("/v1/metrics/latency", serveBucketed(s, "latency percentiles", storage.BucketHour, anyBucket, s.QB.LatencyPercentiles))
		r.Get("/v1/metrics/latency-cold-warm", serveBucketed(s, "cold vs warm latency", storage.BucketDay, anyBucket, s.QB.ColdWarmLatency))
		r.Get("/v1/metrics/resource-usage", serveBucketed(s, "resource usage", storage.BucketHour, anyBucket, s.QB.ResourceUsage))
		r.Get("/v1/metrics/version-footprint", s.handleVersionFootprint)
		r.Get("/v1/metrics/error-rate", serveBucketed(s, "error rate", storage.BucketHour, anyBucket, s.QB.ErrorRateSeries))
		r.Get("/v1/metrics/command-frequency", serveBucketed(s, "command frequency", storage.BucketHour, anyBucket, s.QB.CommandFrequency))
		r.Get("/v1/metrics/exit-codes", serveBucketed(s, "exit code distribution", storage.BucketHour, anyBucket, s.QB.ExitCodeDistribution))
		r.Get("/v1/metrics/error-types", s.handleErrorTypes())
		r.Get("/v1/metrics/error-fingerprints", serveBucketed(s, "error fingerprints", storage.BucketDay, anyBucket, s.errorFingerprints))
		r.Get("/v1/metrics/friction", serveBucketed(s, "friction", storage.BucketHour, anyBucket, s.QB.Friction))
		r.Get("/v1/metrics/install-funnel", serveBucketed(s, "install funnel", storage.BucketWeek, dailyBucket, s.QB.InstallFunnel))
		r.Get("/v1/metrics/install-activation", serveBucketed(s, "install activation", storage.BucketWeek, dailyBucket, s.QB.InstallActivation))
		r.Get("/v1/metrics/install-methods", serveBucketed(s, "install methods", storage.BucketWeek, dailyBucket, s.QB.InstallMethods))
		r.Get("/v1/metrics/new-vs-returning", serveBucketed(s, "new vs returning", storage.BucketWeek, dailyBucket, s.QB.NewVsReturning))
		r.Get("/v1/metrics/version-changes", serveBucketed(s, "version changes", storage.BucketWeek, dailyBucket, s.QB.VersionChanges))
		r.Get("/v1/metrics/feature-discovery", s.handleFeatureDiscovery)
		r.Get("/v1/metrics/counters", serveVendorInstrument(s, "counters", s.QB.Counters, s.QB.CounterSeries))
		r.Get("/v1/metrics/measures", serveVendorInstrument(s, "measures", s.QB.Measures, s.QB.MeasureSeries))
		r.Get("/v1/metrics/install-coverage", serveBucketed(s, "install coverage", storage.BucketDay, dailyBucket, s.QB.InstallCoverage))
		r.Get("/v1/metrics/ci-split", serveBucketed(s, "ci split", storage.BucketHour, anyBucket, s.QB.CISplit))
		r.Get("/v1/metrics/cohorts", serveBucketed(s, "cohorts", storage.BucketDay, dailyBucket, s.QB.SessionCohort))
		r.Get("/v1/metrics/active-installs", serveBucketed(s, "active installs", storage.BucketDay, dailyBucket, s.QB.ActiveInstalls))
		r.Get("/v1/metrics/session-length", s.handleSessionLengths)
		r.Get("/v1/metrics/session-sequences", s.handleSessionSequences)
		r.Get("/v1/metrics/retries", s.handleRetries)
		r.Get("/v1/metrics/retention", serveBucketed(s, "retention", storage.BucketWeek, dailyBucket, s.QB.Retention))
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

// corsHandler returns the CORS middleware, or nil while no origin is allowed.
// Origins match exactly (config validation rejects wildcards), credentials are
// never allowed (bearer tokens travel in a header, not a cookie), and only the
// verbs and headers this API uses are permitted.
func (s *Server) corsHandler() func(http.Handler) http.Handler {
	if len(s.CORS.AllowedOrigins) == 0 {
		return nil
	}
	handler := cors.Handler(cors.Options{
		AllowedOrigins:   s.CORS.AllowedOrigins,
		AllowedMethods:   []string{http.MethodGet, http.MethodOptions},
		AllowedHeaders:   []string{"Authorization", "Content-Type", TenantHeader},
		ExposedHeaders:   []string{"WWW-Authenticate"},
		AllowCredentials: false,
		MaxAge:           int(s.CORS.MaxAge.Seconds()),
	})
	return func(next http.Handler) http.Handler { return preflightNoContent(handler(next)) }
}

// preflightNoContent answers a CORS preflight with 204 rather than the 200
// go-chi/cors hard-codes, the conventional status for an empty reply.
func preflightNoContent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" && r.Header.Get("Origin") != "" {
			w = &statusRewriter{ResponseWriter: w}
		}
		next.ServeHTTP(w, r)
	})
}

type statusRewriter struct{ http.ResponseWriter }

func (w *statusRewriter) WriteHeader(code int) {
	if code == http.StatusOK {
		code = http.StatusNoContent
	}
	w.ResponseWriter.WriteHeader(code)
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
