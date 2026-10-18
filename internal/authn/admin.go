package authn

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/getargvio/argvio/internal/config"
	"github.com/getargvio/argvio/internal/oidc"
	"github.com/getargvio/argvio/internal/role"
	"github.com/getargvio/argvio/internal/tenant"
)

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

	// role is every admin's role, unless roleClaim names a claim of the
	// verified token that carries a valid one.
	role      string
	roleClaim string
	log       *slog.Logger
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
	eff := cfg.Admin.OIDC.Effective(cfg.OIDC)
	recognizers := append(adminRecognizersFromConfig(cfg.Admin, eff.IssuerURL), extra...)
	if len(recognizers) == 0 || !v.methods[MethodOIDC] {
		return
	}
	a := &adminAccess{
		recognizers: recognizers,
		role:        cfg.Admin.EffectiveRole(),
		roleClaim:   cfg.Admin.RoleClaim,
		log:         slog.Default(),
	}
	if !eff.SameVerification(cfg.OIDC) {
		a.verifier = newOIDCVerifier(eff)
	}
	v.admin = a
}

// Rule names, as logged for an admin's tenant selection.
const (
	adminRuleClaim    = "claim"
	adminRuleSubjects = "subjects"
)

// adminRecognizersFromConfig builds the recognition rules configured under
// auth.admin. issuer is the admin issuer (auth.admin.oidc.issuer_url, else
// auth.oidc.issuer_url).
func adminRecognizersFromConfig(cfg config.AdminConfig, issuer string) []AdminRecognizer {
	var rules []AdminRecognizer
	if cfg.Claim.Enabled() {
		rules = append(rules, claimRule{name: cfg.Claim.Name, value: cfg.Claim.Value})
	}
	if subjects := cfg.SubjectAllowlist(); len(subjects) > 0 {
		rules = append(rules, subjectRule{issuer: issuer, subjects: subjects})
	}
	return rules
}

// claimRule recognises a token whose claim name is the string value, or an
// array containing it. The match is exact and case-sensitive; any other claim
// type matches nothing.
type claimRule struct{ name, value string }

func (r claimRule) Recognize(id Identity) (string, bool) {
	if claimHas(id.Claims, r.name, r.value) {
		return adminRuleClaim, true
	}
	return "", false
}

func claimHas(claims jwt.MapClaims, name, value string) bool {
	switch c := claims[name].(type) {
	case string:
		return c == value
	case []string:
		return slices.Contains(c, value)
	case []any:
		return slices.Contains(c, any(value))
	}
	return false
}

// subjectRule recognises the (issuer, sub) pairs of the allowlist, compared
// byte for byte like tenant_members.issuer. The identity's Issuer is the one
// its token was verified against, so a same-named subject of another issuer
// does not match.
type subjectRule struct {
	issuer   string
	subjects []string
}

func (r subjectRule) Recognize(id Identity) (string, bool) {
	if id.Subject != "" && id.Issuer == r.issuer && slices.Contains(r.subjects, id.Subject) {
		return adminRuleSubjects, true
	}
	return "", false
}

// admit marks id as an admin if a rule recognises it, with the role that
// applies to it.
func (a *adminAccess) admit(id Identity) (Identity, bool) {
	for _, r := range a.recognizers {
		if rule, ok := r.Recognize(id); ok {
			id.Admin, id.AdminRule, id.Role = true, rule, a.roleFor(id)
			return id, true
		}
	}
	return id, false
}

// roleFor resolves an admin's role: a valid role in the token's role claim,
// else the configured one. A claim holding anything but a known role falls
// back to the configured role, never to anything else, and is logged (the
// claim's name and the identity, not its value, which the token's holder
// may have chosen).
func (a *adminAccess) roleFor(id Identity) string {
	if a.roleClaim == "" {
		return a.role
	}
	raw, present := id.Claims[a.roleClaim]
	if !present {
		return a.role
	}
	if r, ok := raw.(string); ok && role.Known(r) {
		return r
	}
	a.log.Warn("admin role claim is not a known role; using the configured role",
		"claim", a.roleClaim, "iss", id.Issuer, "sub", id.Subject, "role", a.role)
	return a.role
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
	if admin, ok := v.admin.admit(id); ok {
		return admin, true, nil
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
	id.Resolved = resolved
	return id, nil
}

// adminMemberships is every active tenant, as an admin's memberships, each
// with memberRole.
func (v *Verifier) adminMemberships(ctx context.Context, memberRole string) ([]tenant.Membership, error) {
	tenants, err := v.resolver.ActiveTenants(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tenant.Membership, len(tenants))
	for i, t := range tenants {
		out[i] = tenant.Membership{Tenant: t, Role: memberRole}
	}
	return out, nil
}
