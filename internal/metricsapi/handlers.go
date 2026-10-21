package metricsapi

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

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

// handleErrorTypes serves /v1/metrics/error-types: per error category by
// default, or per category and vendor-assigned reason with group_by=reason.
// The two shapes differ, so each is its own serveBucketed handler.
func (s *Server) handleErrorTypes() http.HandlerFunc {
	byType := serveBucketed(s, "error types", storage.BucketHour, anyBucket, s.QB.ErrorTypes)
	byReason := serveBucketed(s, "error reasons", storage.BucketHour, anyBucket, s.QB.ErrorReasons)
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("group_by") {
		case "", "type":
			byType(w, r)
		case "reason":
			byReason(w, r)
		default:
			writeError(w, http.StatusBadRequest, "invalid_request", "group_by must be type or reason")
		}
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

// serveVendorInstrument serves /v1/metrics/counters and /v1/metrics/measures:
// the tenant's vendor instruments with their statistics, or, with name=, one
// instrument's time series (bucketed like the other endpoints, optionally
// narrowed by label=) and its labels. Two response shapes, so it can't use
// serveBucketed.
func serveVendorInstrument[L, D any](
	s *Server,
	name string,
	list func(ctx context.Context, scope storage.TenantScope, f storage.Filters) (L, error),
	series func(ctx context.Context, scope storage.TenantScope, f storage.Filters, name string, label *string, bucket storage.Bucket) (D, error),
) http.HandlerFunc {
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
		q := r.URL.Query()
		instrument := q.Get("name")
		var label *string
		if q.Has("label") {
			if instrument == "" {
				writeError(w, http.StatusBadRequest, "invalid_request", "label requires name")
				return
			}
			l := q.Get("label")
			label = &l
		}
		ctx, cancel := s.withQuery(r.Context())
		defer cancel()

		if instrument == "" {
			out, err := list(ctx, scope, f)
			if err != nil {
				s.Log.Error("metricsapi: "+name+" failed", "error", err)
				writeError(w, http.StatusInternalServerError, "internal_error", "query failed")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{responseDataKey: out})
			return
		}
		bucket, err := parseBucket(r, storage.BucketHour, anyBucket...)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		out, err := series(ctx, scope, f, instrument, label, bucket)
		if err != nil {
			s.Log.Error("metricsapi: "+name+" series failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "query failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{responseDataKey: out})
	}
}

// handleSessionLengths serves /v1/metrics/session-length. Sessions are built
// from raw logs over the whole window, not per time bucket, so it can't use
// serveBucketed.
func (s *Server) handleSessionLengths(w http.ResponseWriter, r *http.Request) {
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
	dist, err := s.QB.SessionLengthDistribution(ctx, scope, f)
	if err != nil {
		s.Log.Error("metricsapi: session lengths failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{responseDataKey: dist})
}

// handleSessionSequences serves /v1/metrics/session-sequences. The optional
// `min_length` param sets aside sequences shorter than that many commands.
func (s *Server) handleSessionSequences(w http.ResponseWriter, r *http.Request) {
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
	minLength := 1
	if v := r.URL.Query().Get("min_length"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > storage.MaxSequenceLength {
			writeError(w, http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("min_length must be an integer from 1 to %d", storage.MaxSequenceLength))
			return
		}
		minLength = n
	}
	ctx, cancel := s.withQuery(r.Context())
	defer cancel()
	seqs, err := s.QB.SessionSequences(ctx, scope, f, minLength)
	if err != nil {
		s.Log.Error("metricsapi: session sequences failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{responseDataKey: seqs})
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
