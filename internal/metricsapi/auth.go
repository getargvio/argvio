package metricsapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/getargvio/argvio/internal/config"
	"github.com/getargvio/argvio/internal/storage"
	"github.com/getargvio/argvio/internal/tenant"
)

type ctxKey string

const scopeCtxKey ctxKey = "argvio.tenant_scope"

// scopeFromContext retrieves the TenantScope the auth middleware placed on
// the request context. Handlers must never construct a TenantScope
// themselves from a query parameter or header — this is the only path.
func scopeFromContext(ctx context.Context) (storage.TenantScope, bool) {
	s, ok := ctx.Value(scopeCtxKey).(storage.TenantScope)
	return s, ok
}

// authMiddleware validates the caller (JWT dashboard session or scoped API
// key, per AuthMode) and, on success, injects a storage.TenantScope built
// from the *server's* resolution of the caller's identity — never from
// anything the client can directly set (e.g. a `?tenant_id=` query param),
// which is exactly the "attribute-filter injection" tenant-isolation bypass
// docs/architecture.md calls out.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var tenantID uuid.UUID
		var err error

		switch s.AuthMode {
		case config.AuthModeJWT:
			tenantID, err = s.authenticateJWT(r)
		case config.AuthModeAPIKey:
			tenantID, err = s.authenticateAPIKey(r)
		default:
			err = errors.New("metricsapi: server misconfigured: unknown auth mode")
		}
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
			return
		}

		scope, err := storage.NewTenantScope(tenantID)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "token does not carry a valid tenant identity")
			return
		}

		ctx := context.WithValue(r.Context(), scopeCtxKey, scope)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func bearerToken(r *http.Request) (string, error) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return "", errors.New("missing Bearer authorization header")
	}
	return strings.TrimPrefix(auth, "Bearer "), nil
}

// authenticateJWT verifies an HS256 JWT and extracts its "tenant_id" claim.
// HS256 is pinned explicitly (jwt.WithValidMethods) to rule out algorithm-
// confusion attacks where a token crafted with alg=none or an asymmetric
// algorithm is accepted using the wrong verification path.
func (s *Server) authenticateJWT(r *http.Request) (uuid.UUID, error) {
	raw, err := bearerToken(r)
	if err != nil {
		return uuid.Nil, err
	}

	claims := jwt.MapClaims{}
	_, err = jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		return s.JWTSigningKey, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return uuid.Nil, errors.New("invalid or expired token")
	}

	tidRaw, ok := claims["tenant_id"]
	if !ok {
		return uuid.Nil, errors.New("token missing tenant_id claim")
	}
	tidStr, ok := tidRaw.(string)
	if !ok {
		return uuid.Nil, errors.New("token tenant_id claim is not a string")
	}
	tid, err := uuid.Parse(tidStr)
	if err != nil {
		return uuid.Nil, errors.New("token tenant_id claim is not a valid UUID")
	}
	return tid, nil
}

// authenticateAPIKey resolves a scoped API key (scope=metrics_query)
// exactly like the public server resolves ingest keys — same cache-backed
// Resolver interface, different scope.
func (s *Server) authenticateAPIKey(r *http.Request) (uuid.UUID, error) {
	raw, err := bearerToken(r)
	if err != nil {
		return uuid.Nil, err
	}
	resolved, found, err := s.Resolver.Resolve(r.Context(), raw, tenant.ScopeMetricsQuery)
	if err != nil {
		return uuid.Nil, errors.New("internal error resolving api key")
	}
	if !found || !resolved.Active() {
		return uuid.Nil, errors.New("invalid or revoked api key")
	}
	return resolved.Tenant.ID, nil
}
