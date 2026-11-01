package metricsapi

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/getargvio/argvio/internal/authn"
	"github.com/getargvio/argvio/internal/catalog"
	"github.com/getargvio/argvio/internal/storage"
)

// maxCatalogBytes bounds a PUT /v1/metric-definitions body: a catalog of
// catalog's maximum number of metrics stays well under it.
const maxCatalogBytes = 1 << 20

// handlePutMetricDefinitions serves PUT /v1/metric-definitions/{service}:
// the body is a CLI's argvio.metrics.yaml (or the same document as JSON),
// and the CLI's catalog becomes exactly that file, as `argvio metric apply`
// does. The file's service must be the one in the path, so a key can't be
// pointed at the wrong CLI by accident.
func (s *Server) handlePutMetricDefinitions(w http.ResponseWriter, r *http.Request) {
	// The auth middleware derived the tenant scope from this identity's
	// tenant, which is what the write is keyed by.
	id, ok := identityFromContext(r.Context())
	if !ok || id.Resolved == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing tenant scope")
		return
	}
	if s.Catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "metric catalog writes are not enabled on this server")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCatalogBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "a catalog file must be at most 1 MiB")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "could not read the request body")
		return
	}
	f, err := catalog.Parse(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_catalog", err.Error())
		return
	}
	service := chi.URLParam(r, "service")
	if f.Service != service {
		writeError(w, http.StatusBadRequest, "invalid_catalog", "the file is for service "+f.Service+", not "+service)
		return
	}
	defs := make([]storage.MetricDefinitionInput, 0, len(f.Metrics))
	for _, m := range f.Metrics {
		spec, err := m.SpecJSON()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "could not encode a definition")
			return
		}
		defs = append(defs, storage.MetricDefinitionInput{Name: m.Name, Kind: m.Kind, Spec: spec})
	}

	ctx, cancel := s.withQuery(r.Context())
	defer cancel()
	actor := catalogActor(id)
	res, err := s.Catalog.ApplyMetricDefinitions(ctx, id.Resolved.Tenant.ID, service, defs, actor, time.Now().UTC())
	if err != nil {
		s.log().Error("metricsapi: apply metric definitions failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "applying the catalog failed")
		return
	}
	s.log().Info("metricsapi: metric catalog applied", "tenant_id", id.Resolved.Tenant.ID, "service", service, "actor", actor,
		"created", len(res.Created), "updated", len(res.Updated), "deprecated", len(res.Deprecated))
	writeJSON(w, http.StatusOK, map[string]any{responseDataKey: res})
}

// catalogActor is what updated_by records for a caller: the key's prefix, or
// the token's issuer and subject. Never a token.
func catalogActor(id authn.Identity) string {
	if id.Method == authn.MethodAPIKey && id.Resolved != nil {
		return "api_key:" + id.Resolved.APIKey.KeyPrefix
	}
	if id.Subject != "" {
		return string(id.Method) + ":" + id.Issuer + "#" + id.Subject
	}
	return string(id.Method)
}
