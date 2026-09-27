package metricsapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/getargvio/argvio/internal/authn"
	"github.com/getargvio/argvio/internal/storage"
)

type ctxKey string

const scopeCtxKey ctxKey = "argvio.tenant_scope"

// scopeFromContext retrieves the TenantScope the auth middleware set. Never
// build a TenantScope from a query param or header directly.
func scopeFromContext(ctx context.Context) (storage.TenantScope, bool) {
	s, ok := ctx.Value(scopeCtxKey).(storage.TenantScope)
	return s, ok
}

// authMiddleware authenticates via s.Verifier and injects a TenantScope
// derived from the resolved identity, never from client input.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := bearerToken(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
			return
		}

		id, err := s.Verifier.Authenticate(r.Context(), raw)
		switch {
		case err == nil:
		case errors.Is(err, authn.ErrTenantSuspended):
			writeError(w, http.StatusForbidden, "forbidden", "tenant is suspended")
			return
		case errors.Is(err, authn.ErrUnauthenticated):
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or expired credential")
			return
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", "failed to resolve identity")
			return
		}

		scope, err := storage.NewTenantScope(id.Resolved.Tenant.ID)
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
