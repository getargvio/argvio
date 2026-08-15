package storage

import (
	"errors"

	"github.com/google/uuid"
)

// TenantScope is the only way to obtain something a QueryBuilder method
// will accept as "which tenant". Its field is unexported, so the only way
// to construct one is NewTenantScope — every read-path query in this
// package takes a TenantScope, not a bare uuid.UUID, which makes "forgot
// to scope this query to a tenant" a compile error rather than a runtime
// data leak: there is no bare tenant_id anywhere a query method could
// accidentally skip binding it.
//
// This is the structural enforcement docs/architecture.md refers to for
// "a query must never be able to read another tenant's data" — it's a type
// system guarantee, not a code-review convention.
type TenantScope struct {
	tenantID uuid.UUID
}

// NewTenantScope validates id and returns a TenantScope for it. Called
// exactly once per request, immediately after auth resolves the caller's
// tenant — never constructed from unvalidated user input.
func NewTenantScope(id uuid.UUID) (TenantScope, error) {
	if id == uuid.Nil {
		return TenantScope{}, errors.New("storage: tenant scope requires a non-nil tenant id")
	}
	return TenantScope{tenantID: id}, nil
}
