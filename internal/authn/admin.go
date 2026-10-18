package authn

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/getargvio/argvio/internal/config"
	"github.com/getargvio/argvio/internal/oidc"
	"github.com/getargvio/argvio/internal/tenant"
)

// adminRole is the role an admin identity has in every tenant: the least
// privileged one, since admin must not imply the right to change anything.
const adminRole = tenant.DefaultMemberRole

// AdminRecognizer decides whether a verified OIDC identity is an admin. It
// sees only an identity whose token passed admin verification, so Issuer,
// Subject and Claims are trustworthy; it must not look at anything else (never
// a request header). rule names the rule that matched, for the audit log.
type AdminRecognizer interface {
	Recognize(id Identity) (rule string, ok bool)
}

// AdminRecognizerFunc adapts a function to an AdminRecognizer.
type AdminRecognizerFunc func(id Identity) (rule string, ok bool)

func (f AdminRecognizerFunc) Recognize(id Identity) (string, bool) { return f(id) }

// adminAccess is the admin side of a Verifier. It exists only on a Verifier
// that EnableAdmin was called on, which the public server never does: that is
// what keeps admin identity out of ingest.
type adminAccess struct {
	// verifier checks admin tokens; nil when they are verified exactly like
	// user tokens, in which case the user verifier is used.
	verifier    *oidc.Verifier
	recognizers []AdminRecognizer
}

// EnableAdmin turns on admin identities: a token that passes admin
// verification (auth.admin.oidc.*, falling back to auth.oidc.*) and is
// recognised by a rule of cfg.Admin or by extra becomes an Identity with Admin
// set, which reaches every active tenant without memberships.
//
// With no rule configured this does nothing, so behaviour is unchanged. It
// also does nothing without the oidc method. Call it before the Verifier
// serves requests, and only for the metrics server.
func (v *Verifier) EnableAdmin(cfg config.AuthConfig, extra ...AdminRecognizer) {
	recognizers := append(adminRecognizersFromConfig(cfg.Admin), extra...)
	if len(recognizers) == 0 || !v.methods[MethodOIDC] {
		return
	}
	a := &adminAccess{recognizers: recognizers}
	if eff := cfg.Admin.OIDC.Effective(cfg.OIDC); !eff.SameVerification(cfg.OIDC) {
		a.verifier = newOIDCVerifier(eff)
	}
	v.admin = a
}

// adminRecognizersFromConfig builds the recognition rules configured under
// auth.admin.
func adminRecognizersFromConfig(config.AdminConfig) []AdminRecognizer { return nil }

func (a *adminAccess) recognize(id Identity) (string, bool) {
	for _, r := range a.recognizers {
		if rule, ok := r.Recognize(id); ok {
			return rule, true
		}
	}
	return "", false
}

// introspects reports whether the admin verifier, if it has one of its own,
// verifies opaque tokens.
func (a *adminAccess) introspects() bool {
	return a != nil && a.verifier != nil && a.verifier.IntrospectionEnabled()
}

// authenticateAdminFirst tries admin verification, for the case where it
// differs from user verification. It returns the admin identity when the
// token verifies and is recognised; otherwise the token may still be a user
// token, and err tells why admin verification did not apply (nil when the
// token verified but is not an admin's).
func (v *Verifier) authenticateAdminFirst(ctx context.Context, raw string, opaque bool) (Identity, bool, error) {
	if v.admin == nil || v.admin.verifier == nil || (opaque && !v.admin.verifier.IntrospectionEnabled()) {
		return Identity{}, false, nil
	}
	id, err := v.verifyOIDC(ctx, v.admin.verifier, raw, opaque)
	if err != nil {
		return Identity{}, false, err
	}
	if rule, ok := v.admin.recognize(id); ok {
		id.Admin, id.AdminRule = true, rule
		return id, true, nil
	}
	return Identity{}, false, nil
}

// preferAdminErr picks the error to report when a token failed user
// verification after failing admin verification too. A bad token is a 401
// either way, but if the admin side could not say (provider down) or found
// the token valid yet short of a required scope, that is the more useful
// answer than telling the caller to sign in again.
func preferAdminErr(userErr, adminErr error) error {
	if adminErr != nil && errors.Is(userErr, ErrUnauthenticated) &&
		(errors.Is(adminErr, ErrProviderUnavailable) || errors.Is(adminErr, ErrInsufficientScope)) {
		return adminErr
	}
	return userErr
}

// selectAdminTenant gives an admin identity the one tenant it asked for. Like
// a membership lookup, it derives the tenant only from what the verified
// identity may reach, here every active tenant; an unknown or inactive one is
// ErrNoTenantAccess. Without a hint it picks the only active tenant, if there
// is exactly one.
func (v *Verifier) selectAdminTenant(ctx context.Context, id Identity, hint string) (Identity, error) {
	var tenantID uuid.UUID
	if hint != "" {
		parsed, err := uuid.Parse(hint)
		if err != nil {
			return Identity{}, ErrNoTenantAccess
		}
		tenantID = parsed
	} else {
		tenants, err := v.resolver.ActiveTenants(ctx)
		if err != nil {
			return Identity{}, err
		}
		switch len(tenants) {
		case 0:
			return Identity{}, ErrNoTenantAccess
		case 1:
			tenantID = tenants[0].ID
		default:
			return Identity{}, ErrTenantRequired
		}
	}

	resolved, found, err := v.resolver.ResolveByTenantID(ctx, tenantID)
	if err != nil {
		return Identity{}, err
	}
	if !found || resolved.Tenant.Status != tenant.StatusActive {
		return Identity{}, ErrNoTenantAccess
	}
	id.Resolved, id.Role = resolved, adminRole
	return id, nil
}

// adminMemberships is every active tenant, as an admin's memberships.
func (v *Verifier) adminMemberships(ctx context.Context) ([]tenant.Membership, error) {
	tenants, err := v.resolver.ActiveTenants(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tenant.Membership, len(tenants))
	for i, t := range tenants {
		out[i] = tenant.Membership{Tenant: t, Role: adminRole}
	}
	return out, nil
}
