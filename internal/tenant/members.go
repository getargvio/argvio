package tenant

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// FindTenant resolves a tenant by UUID or slug.
func (s *Store) FindTenant(ctx context.Context, idOrSlug string) (*Tenant, error) {
	const cols = `SELECT id, slug, name, status FROM tenants WHERE `
	var (
		row pgx.Row
		t   Tenant
	)
	if id, err := uuid.Parse(idOrSlug); err == nil {
		row = s.pool.QueryRow(ctx, cols+`id = $1`, id)
	} else {
		row = s.pool.QueryRow(ctx, cols+`slug = $1`, idOrSlug)
	}
	err := row.Scan(&t.ID, &t.Slug, &t.Name, &t.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("tenant: find tenant: %w", err)
	}
	return &t, nil
}

// AddMember grants (issuer, subject) access to a tenant. It is idempotent: an
// existing membership is left alone unless role is non-empty and differs, in
// which case only the role changes. created reports whether a row was added.
func (s *Store) AddMember(ctx context.Context, tenantID uuid.UUID, issuer, subject, role string) (m Member, created bool, err error) {
	effective := role
	if effective == "" {
		effective = DefaultMemberRole
	}

	const cols = `tenant_id, issuer, subject, role, created_at`
	err = s.pool.QueryRow(ctx, `
INSERT INTO tenant_members (tenant_id, issuer, subject, role) VALUES ($1, $2, $3, $4)
ON CONFLICT (tenant_id, issuer, subject) DO NOTHING
RETURNING `+cols, tenantID, issuer, subject, effective,
	).Scan(&m.TenantID, &m.Issuer, &m.Subject, &m.Role, &m.CreatedAt)
	switch {
	case err == nil:
		return m, true, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return Member{}, false, fmt.Errorf("tenant: add member: %w", err)
	}

	// Already a member: only an explicit role is allowed to change anything.
	q := `SELECT ` + cols + ` FROM tenant_members WHERE tenant_id = $1 AND issuer = $2 AND subject = $3`
	args := []any{tenantID, issuer, subject}
	if role != "" {
		q = `UPDATE tenant_members SET role = $4 WHERE tenant_id = $1 AND issuer = $2 AND subject = $3 RETURNING ` + cols
		args = append(args, role)
	}
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&m.TenantID, &m.Issuer, &m.Subject, &m.Role, &m.CreatedAt); err != nil {
		return Member{}, false, fmt.Errorf("tenant: add member: %w", err)
	}
	return m, false, nil
}

// RemoveMember revokes a membership, reporting whether one existed. Caches in
// running servers keep honouring it for up to their TTL.
func (s *Store) RemoveMember(ctx context.Context, tenantID uuid.UUID, issuer, subject string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM tenant_members WHERE tenant_id = $1 AND issuer = $2 AND subject = $3`,
		tenantID, issuer, subject)
	if err != nil {
		return false, fmt.Errorf("tenant: remove member: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ListMembers lists a tenant's members, optionally narrowed to one issuer
// and/or subject (empty means any).
func (s *Store) ListMembers(ctx context.Context, tenantID uuid.UUID, issuer, subject string) ([]Member, error) {
	rows, err := s.pool.Query(ctx, `
SELECT tenant_id, issuer, subject, role, created_at FROM tenant_members
WHERE tenant_id = $1 AND ($2 = '' OR issuer = $2) AND ($3 = '' OR subject = $3)
ORDER BY issuer, subject`, tenantID, issuer, subject)
	if err != nil {
		return nil, fmt.Errorf("tenant: list members: %w", err)
	}
	defer rows.Close()

	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.TenantID, &m.Issuer, &m.Subject, &m.Role, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("tenant: scan member: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListMemberships returns every tenant an identity belongs to, in a stable
// order. It is empty (not an error) for an unknown identity.
func (s *Store) ListMemberships(ctx context.Context, issuer, subject string) ([]Membership, error) {
	rows, err := s.pool.Query(ctx, `
SELECT t.id, t.slug, t.name, t.status, m.role
FROM tenant_members m
JOIN tenants t ON t.id = m.tenant_id
WHERE m.issuer = $1 AND m.subject = $2
ORDER BY t.name, t.id`, issuer, subject)
	if err != nil {
		return nil, fmt.Errorf("tenant: list memberships: %w", err)
	}
	defer rows.Close()

	var out []Membership
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.Tenant.ID, &m.Tenant.Slug, &m.Tenant.Name, &m.Tenant.Status, &m.Role); err != nil {
			return nil, fmt.Errorf("tenant: scan membership: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
