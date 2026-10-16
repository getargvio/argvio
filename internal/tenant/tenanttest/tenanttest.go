// Package tenanttest is an in-memory tenant.Resolver for tests that exercise
// authentication without Postgres.
package tenanttest

import (
	"context"

	"github.com/google/uuid"

	"github.com/getargvio/argvio/internal/tenant"
)

type memberKey struct{ issuer, subject string }

// Resolver implements tenant.Resolver.
type Resolver struct {
	tenants map[uuid.UUID]*tenant.Resolved
	members map[memberKey][]tenant.Membership
	keys    map[string]*tenant.Resolved
}

var _ tenant.Resolver = (*Resolver)(nil)

func New() *Resolver {
	return &Resolver{
		tenants: map[uuid.UUID]*tenant.Resolved{},
		members: map[memberKey][]tenant.Membership{},
		keys:    map[string]*tenant.Resolved{},
	}
}

// AddTenant registers a tenant (status "active" or "suspended") and returns its ID.
func (r *Resolver) AddTenant(name, status string) uuid.UUID {
	id := uuid.New()
	r.tenants[id] = &tenant.Resolved{Tenant: tenant.Tenant{ID: id, Slug: name, Name: name, Status: status}}
	return id
}

// AddMember makes (issuer, subject) a member of tenantID.
func (r *Resolver) AddMember(issuer, subject string, tenantID uuid.UUID, role string) {
	k := memberKey{issuer, subject}
	r.members[k] = append(r.members[k], tenant.Membership{Tenant: r.tenants[tenantID].Tenant, Role: role})
}

// AddAPIKey makes rawKey resolve to tenantID, for any scope.
func (r *Resolver) AddAPIKey(rawKey string, tenantID uuid.UUID) {
	res := *r.tenants[tenantID]
	res.APIKey = tenant.APIKey{TenantID: tenantID}
	r.keys[rawKey] = &res
}

func (r *Resolver) Resolve(_ context.Context, rawKey, _ string) (*tenant.Resolved, bool, error) {
	res, ok := r.keys[rawKey]
	return res, ok, nil
}

func (r *Resolver) ResolveByTenantID(_ context.Context, id uuid.UUID) (*tenant.Resolved, bool, error) {
	res, ok := r.tenants[id]
	return res, ok, nil
}

func (r *Resolver) Memberships(_ context.Context, issuer, subject string) ([]tenant.Membership, error) {
	return r.members[memberKey{issuer, subject}], nil
}
