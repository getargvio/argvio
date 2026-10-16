package metricsapi

import "net/http"

// tenantJSON is one element of the GET /v1/tenants response.
type tenantJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// handleTenants lists the tenants the caller may select with X-Argvio-Tenant.
// The body is a bare array, as the dashboard contract specifies, unlike the
// {"data": ...} envelope of the query endpoints. An identity with no
// memberships gets an empty array, which lets the dashboard show a "no access"
// screen instead of an error.
func (s *Server) handleTenants(w http.ResponseWriter, r *http.Request) {
	id, ok := identityFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing identity")
		return
	}
	memberships, err := s.Verifier.Memberships(r.Context(), id)
	if err != nil {
		s.log().Error("metricsapi: list memberships failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to list tenants")
		return
	}
	out := make([]tenantJSON, 0, len(memberships))
	for _, m := range memberships {
		out = append(out, tenantJSON{ID: m.Tenant.ID.String(), Name: m.Tenant.Name, Role: m.Role})
	}
	writeJSON(w, http.StatusOK, out)
}
