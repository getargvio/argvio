package metricsapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/getargvio/argvio/internal/catalog"
	"github.com/getargvio/argvio/internal/role"
	"github.com/getargvio/argvio/internal/storage"
)

// Bounds of a saved layout.
const (
	maxDashboardCards   = 100
	maxCardOptionsBytes = 4096
	maxDashboardBytes   = 1 << 20
)

// Card types.
const (
	cardKPI          = "kpi"
	cardTrend        = "trend"
	cardBreakdown    = "breakdown"
	cardDistribution = "distribution"
	cardReach        = "reach"
)

var cardMetric = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// cardTypesFor lists the card types each metric kind can be shown as.
var cardTypesFor = map[string][]string{
	catalog.KindCounter:  {cardKPI, cardTrend, cardBreakdown, cardReach},
	catalog.KindDuration: {cardKPI, cardTrend, cardBreakdown, cardDistribution, cardReach},
	catalog.KindValue:    {cardKPI, cardTrend, cardBreakdown, cardDistribution, cardReach},
	catalog.KindRatio:    {cardKPI, cardTrend},
}

// defaultCardTypes is the default layout's cards per metric kind.
var defaultCardTypes = map[string][]string{
	catalog.KindCounter:  {cardKPI, cardTrend},
	catalog.KindDuration: {cardKPI, cardTrend},
	catalog.KindValue:    {cardKPI, cardDistribution},
	catalog.KindRatio:    {cardKPI, cardTrend},
}

// dashboardCard is a card as served: with its metric's status in the
// catalog, so a card on a removed metric is flagged rather than dropped.
type dashboardCard struct {
	storage.DashboardCard
	// Kind is the metric's kind, "" when it isn't declared.
	Kind string `json:"kind"`
	// Status is ok, deprecated (left the catalog file) or missing (never
	// declared, or no longer known).
	Status string `json:"status"`
}

type dashboardLayout struct {
	Service string          `json:"service"`
	Saved   bool            `json:"saved"`
	Cards   []dashboardCard `json:"cards"`
}

// handleGetDashboardCards serves GET /v1/dashboard/cards?service=: the saved
// layout of a CLI's Product dashboard or, when none is saved, the default
// one generated from its catalog (each declared metric's default cards, by
// name).
func (s *Server) handleGetDashboardCards(w http.ResponseWriter, r *http.Request) {
	scope, ok := scopeFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing tenant scope")
		return
	}
	service := r.URL.Query().Get("service")
	if service == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "service is required")
		return
	}
	ctx, cancel := s.withQuery(r.Context())
	defer cancel()
	defs, err := s.QB.MetricDefinitions(ctx, scope, []string{service}, time.Now())
	if err != nil {
		s.Log.Error("metricsapi: dashboard definitions failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "query failed")
		return
	}
	type declared struct {
		kind       string
		deprecated bool
	}
	byName := map[string]declared{}
	for _, d := range defs {
		if d.Declared {
			byName[d.Name] = declared{d.Kind, d.DeprecatedAt != nil}
		}
	}
	saved, isSaved, err := s.QB.DashboardCards(ctx, scope, service)
	if err != nil {
		s.Log.Error("metricsapi: dashboard cards failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "query failed")
		return
	}
	out := dashboardLayout{Service: service, Saved: isSaved, Cards: []dashboardCard{}}
	if isSaved {
		for _, c := range saved {
			d, ok := byName[c.Metric]
			status := "ok"
			switch {
			case !ok:
				status = "missing"
			case d.deprecated:
				status = "deprecated"
			}
			out.Cards = append(out.Cards, dashboardCard{DashboardCard: c, Kind: d.kind, Status: status})
		}
	} else {
		for _, d := range defs {
			if !d.Declared || d.DeprecatedAt != nil {
				continue
			}
			for _, t := range defaultCardTypes[d.Kind] {
				out.Cards = append(out.Cards, dashboardCard{DashboardCard: storage.DashboardCard{Metric: d.Name, CardType: t}, Kind: d.Kind, Status: "ok"})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{responseDataKey: out})
}

// handlePutDashboardCards serves PUT /v1/dashboard/cards/{service}: the body
// {"cards": [...]} becomes the CLI's saved layout, in order; an empty list
// goes back to the default. Every card must be on a declared, current metric
// of the CLI, with a card type its kind supports.
func (s *Server) handlePutDashboardCards(w http.ResponseWriter, r *http.Request) {
	scope, okScope := scopeFromContext(r.Context())
	id, ok := identityFromContext(r.Context())
	if !ok || !okScope || id.Resolved == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing tenant scope")
		return
	}
	if s.Catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "dashboard writes are not enabled on this server")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDashboardBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "a layout must be at most 1 MiB")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "could not read the request body")
		return
	}
	var req struct {
		Cards []storage.DashboardCard `json:"cards"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Cards == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", `the body must be {"cards": [...]}`)
		return
	}
	service := chi.URLParam(r, "service")
	ctx, cancel := s.withQuery(r.Context())
	defer cancel()
	defs, err := s.QB.MetricDefinitions(ctx, scope, []string{service}, time.Now())
	if err != nil {
		s.Log.Error("metricsapi: dashboard definitions failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "query failed")
		return
	}
	kinds := map[string]string{}
	for _, d := range defs {
		if d.Declared && d.DeprecatedAt == nil {
			kinds[d.Name] = d.Kind
		}
	}
	if err := validateCards(req.Cards, kinds); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_layout", err.Error())
		return
	}
	if err := s.Catalog.ReplaceDashboardCards(ctx, id.Resolved.Tenant.ID, service, req.Cards, catalogActor(id), time.Now().UTC()); err != nil {
		s.Log.Error("metricsapi: save dashboard cards failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "saving the layout failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validateCards(cards []storage.DashboardCard, kinds map[string]string) error {
	if len(cards) > maxDashboardCards {
		return fmt.Errorf("at most %d cards", maxDashboardCards)
	}
	for i, c := range cards {
		if !cardMetric.MatchString(c.Metric) {
			return fmt.Errorf("cards[%d]: metric %q is not a metric name", i, c.Metric)
		}
		kind, ok := kinds[c.Metric]
		if !ok {
			return fmt.Errorf("cards[%d]: %s is not a declared metric of this CLI", i, c.Metric)
		}
		allowed := false
		for _, t := range cardTypesFor[kind] {
			allowed = allowed || t == c.CardType
		}
		if !allowed {
			return fmt.Errorf("cards[%d]: a %s can't be shown as %q (one of %v)", i, kind, c.CardType, cardTypesFor[kind])
		}
		if len(c.Options) > 0 {
			var obj map[string]any
			if len(c.Options) > maxCardOptionsBytes || json.Unmarshal(c.Options, &obj) != nil {
				return fmt.Errorf("cards[%d]: options must be a JSON object of at most %d bytes", i, maxCardOptionsBytes)
			}
		}
	}
	return nil
}

// layoutWriteAccess admits identities with the admin role in the tenant. Not
// a metrics_admin key: the layout belongs to the dashboard, not to CI.
func (s *Server) layoutWriteAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFromContext(r.Context())
		if !ok || isCatalogKey(id) || id.Role != role.Admin {
			writeError(w, http.StatusForbidden, "insufficient_scope", "saving the dashboard layout needs the admin role")
			return
		}
		next.ServeHTTP(w, r)
	})
}
