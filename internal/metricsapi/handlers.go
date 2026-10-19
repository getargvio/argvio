package metricsapi

import (
	"context"
	"net/http"

	"github.com/getargvio/argvio/internal/storage"
)

// responseDataKey is the JSON envelope key every handler wraps its result
// in: {"data": ...}.
const responseDataKey = "data"

func (s *Server) withQuery(ctx context.Context) (context.Context, context.CancelFunc) {
	// A dedicated per-query timeout (MetricsConfig.QueryTimeout) so one slow
	// analytical query can't hang a connection indefinitely — separate from
	// any client-side HTTP timeout.
	return context.WithTimeout(ctx, s.QueryTimeout)
}

func (s *Server) handleListTraces(w http.ResponseWriter, r *http.Request) {
	scope, ok := scopeFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing tenant scope")
		return
	}
	f, err := s.parseFilters(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx, cancel := s.withQuery(r.Context())
	defer cancel()
	rows, err := s.QB.ListTraces(ctx, scope, f)
	if err != nil {
		s.Log.Error("metricsapi: list traces failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{responseDataKey: rows})
}

// Bucket granularities each rollup can serve: command stats and exit codes
// have an hourly rollup underneath; session activity is daily at its
// finest; retention cohorts are day/week/month.
var (
	anyBucket   = []storage.Bucket{storage.BucketHour, storage.BucketDay, storage.BucketWeek, storage.BucketMonth}
	dailyBucket = []storage.Bucket{storage.BucketDay, storage.BucketWeek, storage.BucketMonth}
)

// bucketedQuery is the shape every time-bucketed aggregate in
// internal/storage shares.
type bucketedQuery[T any] func(context.Context, storage.TenantScope, storage.Filters, storage.Bucket) ([]T, error)

// serveBucketed builds the handler for one time-bucketed aggregate
// endpoint: tenant scope, filters, and bucket (defaulting to def,
// restricted to allowed) are resolved the same way for all of them before
// query runs under the per-query timeout. A free function rather than a
// Server method because Go methods can't take type parameters.
func serveBucketed[T any](s *Server, name string, def storage.Bucket, allowed []storage.Bucket, query bucketedQuery[T]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := scopeFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing tenant scope")
			return
		}
		f, err := s.parseFilters(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		bucket, err := parseBucket(r, def, allowed...)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		ctx, cancel := s.withQuery(r.Context())
		defer cancel()
		points, err := query(ctx, scope, f, bucket)
		if err != nil {
			s.Log.Error("metricsapi: "+name+" failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "query failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{responseDataKey: points})
	}
}

// errorFingerprints adapts QB.ErrorFingerprints to serveBucketed by
// supplying the server's tier ranking for the read-time ceiling check.
func (s *Server) errorFingerprints(ctx context.Context, scope storage.TenantScope, f storage.Filters, bucket storage.Bucket) ([]storage.ErrorFingerprint, error) {
	return s.QB.ErrorFingerprints(ctx, scope, f, bucket, s.Tiers)
}

// handleFeatureDiscovery serves /v1/metrics/feature-discovery. It isn't
// time-bucketed (discovery is measured as time since install, not on the
// calendar), so it can't use serveBucketed; the optional `kind` param narrows
// it to features or flags.
func (s *Server) handleFeatureDiscovery(w http.ResponseWriter, r *http.Request) {
	scope, ok := scopeFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing tenant scope")
		return
	}
	f, err := s.parseFilters(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind != "" && kind != storage.DiscoveryFeature && kind != storage.DiscoveryFlag {
		writeError(w, http.StatusBadRequest, "invalid_request", "kind must be feature or flag")
		return
	}
	ctx, cancel := s.withQuery(r.Context())
	defer cancel()
	points, err := s.QB.FeatureDiscovery(ctx, scope, f, kind)
	if err != nil {
		s.Log.Error("metricsapi: feature discovery failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{responseDataKey: points})
}

// handleVersionFootprint serves /v1/metrics/version-footprint. It isn't
// time-bucketed (one row per CLI version over the whole window, ordered by
// version rather than by time), so it can't use serveBucketed.
func (s *Server) handleVersionFootprint(w http.ResponseWriter, r *http.Request) {
	scope, ok := scopeFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing tenant scope")
		return
	}
	f, err := s.parseFilters(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx, cancel := s.withQuery(r.Context())
	defer cancel()
	points, err := s.QB.VersionFootprint(ctx, scope, f)
	if err != nil {
		s.Log.Error("metricsapi: version footprint failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{responseDataKey: points})
}

// handleDimensions lists the distinct filter values a tenant has data for.
// Deliberately takes no time range (and so skips parseFilters): option
// lists should cover everything the tenant has, not one query window.
func (s *Server) handleDimensions(w http.ResponseWriter, r *http.Request) {
	scope, ok := scopeFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing tenant scope")
		return
	}
	ctx, cancel := s.withQuery(r.Context())
	defer cancel()
	dims, err := s.QB.ListDimensions(ctx, scope, s.MaxResultPageSize)
	if err != nil {
		s.Log.Error("metricsapi: list dimensions failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{responseDataKey: dims})
}
