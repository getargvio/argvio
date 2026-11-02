// Package tenant resolves API keys to tenants and their per-tenant
// configuration (consent tier ceiling, rate limits, retention overrides).
//
// internal/tenant.Store talks to Postgres directly; internal/tenant.Cache
// wraps it with an in-memory TTL cache so the public server's hot ingest
// path never hits Postgres per-request (per the "cheap in-memory/
// Redis-cacheable lookup" requirement) — swapping in a Redis-backed cache
// later just means implementing the same Resolver interface.
package tenant

import (
	"time"

	"github.com/google/uuid"

	"github.com/getargvio/argvio/internal/consent"
	"github.com/getargvio/argvio/internal/role"
)

// StatusActive is the Tenant.Status of a tenant allowed to use the service.
const StatusActive = "active"

// Tenant is a row from the tenants table.
type Tenant struct {
	ID     uuid.UUID
	Slug   string
	Name   string
	Status string // active | suspended
}

// Config is a row from tenant_config — the runtime-tunable overrides that
// change per-customer without a code deploy.
type Config struct {
	TenantID uuid.UUID

	TierCeiling         string
	TierEnforcementMode consent.EnforcementMode

	// nil means "use the public server's global config default".
	RateLimitRequestsPerSec *float64
	RateLimitBytesPerSec    *float64
	RateLimitBurst          *int

	// nil means "use the global Timescale retention policy window".
	RetentionTracesDays  *int
	RetentionLogsDays    *int
	RetentionMetricsDays *int

	// Vendor-metric caps; nil means "use the public server's global
	// value" (public.max_counter_names_per_tenant and so on).
	MetricCaps MetricCaps

	// StrictMetrics rejects vendor metrics the sending CLI's catalog
	// doesn't declare.
	StrictMetrics bool
}

// MetricCaps are a tenant's overrides of the vendor-metric caps. Each nil
// field falls back to the matching public.* config key.
type MetricCaps struct {
	MaxCounterNames         *int
	MaxCounterLabelsPerName *int
	MaxMeasureNames         *int
	MaxMeasureLabelsPerName *int
	MaxMetricDims           *int
	MaxDimValuesPerKey      *int
	MaxValueNames           *int
	MaxValueLabelsPerName   *int
}

// APIKey is a row from api_keys. RawKey is only ever populated at creation
// time (CreateAPIKey) — everywhere else, only KeyHash/KeyPrefix are known.
type APIKey struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	KeyHash    []byte
	KeyPrefix  string
	Scope      string // public_ingest | metrics_query | metrics_admin
	CreatedAt  time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
}

const (
	ScopePublicIngest = "public_ingest"
	ScopeMetricsQuery = "metrics_query"
	// ScopeMetricsAdmin may only replace a CLI's metric catalog on the
	// metrics server (PUT /v1/metric-definitions/{service}), for CI.
	ScopeMetricsAdmin = "metrics_admin"
)

// Resolved is what an API-key lookup returns: everything the public server
// needs to validate + enforce consent tiers + rate-limit a request, in one
// shape, so the hot path does exactly one cache lookup.
type Resolved struct {
	Tenant Tenant
	Config Config
	APIKey APIKey
}

// Active reports whether the tenant is allowed to send/query data at all.
func (r *Resolved) Active() bool {
	return r.Tenant.Status == StatusActive && r.APIKey.RevokedAt == nil
}

// Member is a row from tenant_members: an OIDC identity's access to a tenant.
type Member struct {
	TenantID  uuid.UUID
	Issuer    string
	Subject   string
	Role      string
	CreatedAt time.Time
}

// DefaultMemberRole is the role a new membership gets when none is given.
const DefaultMemberRole = role.Viewer

// Membership is one tenant an identity may use, with its role there.
type Membership struct {
	Tenant Tenant
	Role   string
}
