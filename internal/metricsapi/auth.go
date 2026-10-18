package metricsapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/getargvio/argvio/internal/authn"
	"github.com/getargvio/argvio/internal/oidc"
	"github.com/getargvio/argvio/internal/storage"
)

type ctxKey string

const (
	scopeCtxKey    ctxKey = "argvio.tenant_scope"
	identityCtxKey ctxKey = "argvio.identity"
)

// TenantHeader lets a caller with several tenant memberships say which one a
// request is for. It only chooses among tenants the caller already owns.
const TenantHeader = "X-Argvio-Tenant"

// scopeFromContext retrieves the TenantScope the auth middleware set. Never
// build a TenantScope from a query param or header directly.
func scopeFromContext(ctx context.Context) (storage.TenantScope, bool) {
	s, ok := ctx.Value(scopeCtxKey).(storage.TenantScope)
	return s, ok
}

func identityFromContext(ctx context.Context) (authn.Identity, bool) {
	id, ok := ctx.Value(identityCtxKey).(authn.Identity)
	return id, ok
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// authMiddleware authenticates via s.Verifier and injects a TenantScope
// derived from the resolved identity, never from client input: the
// X-Argvio-Tenant header is only handed to the verifier, which accepts it
// solely if it names a tenant the verified identity already belongs to.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r)
		if !ok {
			writeBearerChallenge(w, "")
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing Bearer authorization header")
			return
		}

		id, err := s.Verifier.AuthenticateForTenant(r.Context(), raw, r.Header.Get(TenantHeader))
		if err != nil {
			s.writeAuthError(w, err)
			return
		}

		scope, err := storage.NewTenantScope(id.Resolved.Tenant.ID)
		if err != nil {
			writeBearerChallenge(w, "invalid_token")
			writeError(w, http.StatusUnauthorized, "unauthorized", "token does not carry a valid tenant identity")
			return
		}
		s.logAuthenticated(r, id)
		s.auditAdminTenant(r, id)

		ctx := context.WithValue(r.Context(), scopeCtxKey, scope)
		ctx = context.WithValue(ctx, identityCtxKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// userMiddleware authenticates an OIDC caller without choosing a tenant, for
// endpoints that are about the user rather than a tenant's data.
func (s *Server) userMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r)
		if !ok {
			writeBearerChallenge(w, "")
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing Bearer authorization header")
			return
		}
		id, err := s.Verifier.AuthenticateSubject(r.Context(), raw)
		if err != nil {
			s.writeAuthError(w, err)
			return
		}
		if id.Method != authn.MethodOIDC {
			writeError(w, http.StatusForbidden, "forbidden", "this endpoint requires an OIDC user token")
			return
		}
		s.logAuthenticated(r, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityCtxKey, id)))
	})
}

// writeAuthError maps an authentication failure to the status the dashboard
// acts on: 401 means "sign in again", 403 means "signed in but not allowed"
// (re-authenticating would not help), 503 means "the identity provider is
// unreachable, not that your token is bad".
func (s *Server) writeAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, authn.ErrProviderUnavailable):
		s.log().Error("identity provider unavailable", "error", err)
		writeError(w, http.StatusServiceUnavailable, "provider_unavailable", "the identity provider could not be reached; retry shortly")
	case errors.Is(err, authn.ErrInsufficientScope):
		var se *oidc.ScopeError
		scope := ""
		if errors.As(err, &se) {
			scope = strings.Join(se.Missing, " ")
		}
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="`+scope+`"`)
		writeError(w, http.StatusForbidden, "insufficient_scope", "token lacks a required scope")
	case errors.Is(err, authn.ErrTenantSuspended):
		writeError(w, http.StatusForbidden, "forbidden", "tenant is suspended")
	case errors.Is(err, authn.ErrNoTenantAccess):
		writeError(w, http.StatusForbidden, "no_tenant_access", "no access to the requested tenant")
	case errors.Is(err, authn.ErrTenantRequired):
		writeError(w, http.StatusBadRequest, "tenant_required", "this identity belongs to several tenants; send "+TenantHeader+" to choose one")
	case errors.Is(err, authn.ErrUnauthenticated):
		s.log().Debug("credential rejected", "reason", err.Error())
		writeBearerChallenge(w, "invalid_token")
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or expired credential")
	default:
		s.log().Error("failed to resolve identity", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to resolve identity")
	}
}

// writeBearerChallenge sets the RFC 6750 §3 challenge. errCode is omitted for
// a request that carried no credential at all.
func writeBearerChallenge(w http.ResponseWriter, errCode string) {
	h := "Bearer"
	if errCode != "" {
		h += ` error="` + errCode + `"`
	}
	w.Header().Set("WWW-Authenticate", h)
}

// logAuthenticated records who was let in. Only iss, sub and jti are logged:
// never the token or its other claims.
func (s *Server) logAuthenticated(r *http.Request, id authn.Identity) {
	if !s.log().Enabled(r.Context(), slog.LevelDebug) {
		return
	}
	attrs := []any{"method", string(id.Method)}
	if id.Method == authn.MethodOIDC {
		jti, _ := id.Claims["jti"].(string)
		attrs = append(attrs, "iss", id.Issuer, "sub", id.Subject, "jti", jti)
	}
	if id.Resolved != nil {
		attrs = append(attrs, "tenant_id", id.Resolved.Tenant.ID.String())
	}
	s.log().DebugContext(r.Context(), "authenticated", attrs...)
}

// auditAdminTenant leaves a trail whenever an admin identity, which owns no
// membership to explain its access, is given a tenant. Like logAuthenticated
// it never logs the token or any claim beyond iss and sub.
func (s *Server) auditAdminTenant(r *http.Request, id authn.Identity) {
	if !id.Admin || id.Resolved == nil {
		return
	}
	s.log().InfoContext(r.Context(), "admin selected tenant",
		"iss", id.Issuer, "sub", id.Subject, "tenant_id", id.Resolved.Tenant.ID.String(), "rule", id.AdminRule)
}

func bearerToken(r *http.Request) (string, bool) {
	auth := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || tok == "" {
		return "", false
	}
	return tok, true
}
