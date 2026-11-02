package metricsapi

import (
	"encoding/json"
	"net/http"

	"github.com/getargvio/argvio/internal/catalog"
	"github.com/getargvio/argvio/internal/storage"
)

// ratioResponse is /v1/metrics/ratios: the ratio's definition, then its value
// per bucket and over the window.
type ratioResponse struct {
	Service    string          `json:"service"`
	Name       string          `json:"name"`
	Definition json.RawMessage `json:"definition"`
	storage.RatioSeries
}

// handleRatios serves /v1/metrics/ratios?service=&name=: a ratio metric of a
// CLI's catalog, computed from its two operands with the request's filters
// and bucket. Nothing is stored. A ratio, or a metric it uses, that isn't
// declared (or is deprecated) is 404. A ratio over sessions or active
// installs has no hour bucket: its default bucket is day.
func (s *Server) handleRatios(w http.ResponseWriter, r *http.Request) {
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
	service, name := q.Get("service"), q.Get("name")
	if service == "" || name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "service and name are required")
		return
	}
	ctx, cancel := s.withQuery(r.Context())
	defer cancel()
	defs, err := s.QB.ActiveMetricDefinitions(ctx, scope, service)
	if err != nil {
		s.Log.Error("metricsapi: ratio definitions failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "query failed")
		return
	}
	raw, ok := defs[name]
	var spec catalog.Metric
	if !ok || json.Unmarshal(raw, &spec) != nil || spec.Kind != catalog.KindRatio || spec.Numerator == nil || spec.Denominator == nil {
		writeError(w, http.StatusNotFound, "not_found", "no ratio "+name+" is declared for "+service)
		return
	}
	resolve := func(o *catalog.Operand) (storage.RatioOperand, bool) {
		op := storage.RatioOperand{Metric: o.Metric, Builtin: o.Builtin, Where: o.Where, Agg: o.Agg}
		if o.Metric == "" {
			return op, true
		}
		var ref catalog.Metric
		if json.Unmarshal(defs[o.Metric], &ref) != nil || ref.Kind == "" || ref.Kind == catalog.KindRatio {
			return op, false
		}
		op.Kind = ref.Kind
		return op, true
	}
	num, okN := resolve(spec.Numerator)
	den, okD := resolve(spec.Denominator)
	if !okN || !okD {
		writeError(w, http.StatusNotFound, "not_found", "ratio "+name+" uses a metric that is no longer declared for "+service)
		return
	}
	daily := num.RatioNeedsDailyBucket() || den.RatioNeedsDailyBucket()
	def := storage.BucketHour
	if daily {
		def = storage.BucketDay
	}
	bucket, err := parseBucket(r, def, anyBucket...)
	if err != nil || (daily && bucket == storage.BucketHour) {
		writeError(w, http.StatusBadRequest, "invalid_request", "this ratio uses daily session data: bucket must be day, week or month")
		return
	}
	scale := spec.Scale
	if scale == 0 {
		scale = 1
	}
	series, err := s.QB.Ratio(ctx, scope, f, service, num, den, scale, bucket)
	if err != nil {
		s.Log.Error("metricsapi: ratio failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{responseDataKey: ratioResponse{Service: service, Name: name, Definition: raw, RatioSeries: series}})
}
