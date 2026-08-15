package metricsapi

import (
	"context"
	"net/http"
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

func (s *Server) handleLatencyPercentiles(w http.ResponseWriter, r *http.Request) {
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
	bucket, err := parseBucket(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx, cancel := s.withQuery(r.Context())
	defer cancel()
	points, err := s.QB.LatencyPercentiles(ctx, scope, f, bucket)
	if err != nil {
		s.Log.Error("metricsapi: latency percentiles failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{responseDataKey: points})
}

func (s *Server) handleErrorRate(w http.ResponseWriter, r *http.Request) {
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
	bucket, err := parseBucket(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx, cancel := s.withQuery(r.Context())
	defer cancel()
	points, err := s.QB.ErrorRateSeries(ctx, scope, f, bucket)
	if err != nil {
		s.Log.Error("metricsapi: error rate failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{responseDataKey: points})
}

func (s *Server) handleCommandFrequency(w http.ResponseWriter, r *http.Request) {
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
	bucket, err := parseBucket(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx, cancel := s.withQuery(r.Context())
	defer cancel()
	points, err := s.QB.CommandFrequency(ctx, scope, f, bucket)
	if err != nil {
		s.Log.Error("metricsapi: command frequency failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{responseDataKey: points})
}

func (s *Server) handleCohorts(w http.ResponseWriter, r *http.Request) {
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
	points, err := s.QB.SessionCohort(ctx, scope, f)
	if err != nil {
		s.Log.Error("metricsapi: cohorts failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{responseDataKey: points})
}
