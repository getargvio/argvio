package tenant

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a lookup finds no matching, usable row.
var ErrNotFound = errors.New("tenant: not found")

// Store is the Postgres-backed source of truth for tenants, their config,
// and API keys. It is safe for concurrent use (wraps a pgxpool.Pool).
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// LookupByRawAPIKey hashes rawKey and resolves it to a tenant + config,
// scoped to the given key scope (public_ingest or metrics_query). This is
// the query Cache falls back to on a cache miss — never call it directly
// from the public server's hot path.
func (s *Store) LookupByRawAPIKey(ctx context.Context, rawKey, scope string) (*Resolved, error) {
	sum := sha256.Sum256([]byte(rawKey))
	return s.LookupByKeyHash(ctx, sum[:], scope)
}

func (s *Store) LookupByKeyHash(ctx context.Context, keyHash []byte, scope string) (*Resolved, error) {
	const q = `
SELECT
    t.id, t.slug, t.name, t.status,
    tc.tier_ceiling, tc.tier_enforcement_mode,
    tc.rate_limit_requests_per_sec, tc.rate_limit_bytes_per_sec, tc.rate_limit_burst,
    tc.retention_traces_days, tc.retention_logs_days, tc.retention_metrics_days,
    k.id, k.key_hash, k.key_prefix, k.scope, k.created_at, k.revoked_at, k.last_used_at
FROM api_keys k
JOIN tenants t ON t.id = k.tenant_id
JOIN tenant_config tc ON tc.tenant_id = t.id
WHERE k.key_hash = $1 AND k.scope = $2 AND k.revoked_at IS NULL`

	row := s.pool.QueryRow(ctx, q, keyHash, scope)
	var r Resolved
	err := row.Scan(
		&r.Tenant.ID, &r.Tenant.Slug, &r.Tenant.Name, &r.Tenant.Status,
		&r.Config.TierCeiling, &r.Config.TierEnforcementMode,
		&r.Config.RateLimitRequestsPerSec, &r.Config.RateLimitBytesPerSec, &r.Config.RateLimitBurst,
		&r.Config.RetentionTracesDays, &r.Config.RetentionLogsDays, &r.Config.RetentionMetricsDays,
		&r.APIKey.ID, &r.APIKey.KeyHash, &r.APIKey.KeyPrefix, &r.APIKey.Scope,
		&r.APIKey.CreatedAt, &r.APIKey.RevokedAt, &r.APIKey.LastUsedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("tenant: lookup: %w", err)
	}
	r.Config.TenantID = r.Tenant.ID
	r.APIKey.TenantID = r.Tenant.ID
	return &r, nil
}

// TouchLastUsed updates api_keys.last_used_at. Best-effort — callers on the
// hot path should fire this off without blocking the request (see
// internal/tenant.Cache for the async wrapper) since it's purely for
// operator visibility, not correctness.
func (s *Store) TouchLastUsed(ctx context.Context, keyID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE api_keys SET last_used_at = now() WHERE id = $1`, keyID)
	if err != nil {
		return fmt.Errorf("tenant: touch last_used_at: %w", err)
	}
	return nil
}

// CreateTenant inserts a new tenant with a default tenant_config row
// (ceiling "basic", mode "strip" — the conservative default; raise the
// ceiling explicitly per-tenant via UpsertConfig once they're onboarded).
func (s *Store) CreateTenant(ctx context.Context, slug, name string) (*Tenant, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenant: create: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var t Tenant
	err = tx.QueryRow(ctx,
		`INSERT INTO tenants (slug, name) VALUES ($1, $2) RETURNING id, slug, name, status`,
		slug, name,
	).Scan(&t.ID, &t.Slug, &t.Name, &t.Status)
	if err != nil {
		return nil, fmt.Errorf("tenant: create: insert tenant: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO tenant_config (tenant_id, tier_ceiling, tier_enforcement_mode) VALUES ($1, 'basic', 'strip')`,
		t.ID,
	); err != nil {
		return nil, fmt.Errorf("tenant: create: insert config: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("tenant: create: commit: %w", err)
	}
	return &t, nil
}

// UpsertConfig writes tenant_config for an existing tenant, overwriting
// whatever was there. Used by cmd/admin and (eventually) a tenant-settings
// API, not by the ingest/query hot paths.
func (s *Store) UpsertConfig(ctx context.Context, cfg Config) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO tenant_config (
    tenant_id, tier_ceiling, tier_enforcement_mode,
    rate_limit_requests_per_sec, rate_limit_bytes_per_sec, rate_limit_burst,
    retention_traces_days, retention_logs_days, retention_metrics_days, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
ON CONFLICT (tenant_id) DO UPDATE SET
    tier_ceiling = EXCLUDED.tier_ceiling,
    tier_enforcement_mode = EXCLUDED.tier_enforcement_mode,
    rate_limit_requests_per_sec = EXCLUDED.rate_limit_requests_per_sec,
    rate_limit_bytes_per_sec = EXCLUDED.rate_limit_bytes_per_sec,
    rate_limit_burst = EXCLUDED.rate_limit_burst,
    retention_traces_days = EXCLUDED.retention_traces_days,
    retention_logs_days = EXCLUDED.retention_logs_days,
    retention_metrics_days = EXCLUDED.retention_metrics_days,
    updated_at = now()`,
		cfg.TenantID, cfg.TierCeiling, cfg.TierEnforcementMode,
		cfg.RateLimitRequestsPerSec, cfg.RateLimitBytesPerSec, cfg.RateLimitBurst,
		cfg.RetentionTracesDays, cfg.RetentionLogsDays, cfg.RetentionMetricsDays,
	)
	if err != nil {
		return fmt.Errorf("tenant: upsert config: %w", err)
	}
	return nil
}

// ListRetentionOverrides returns every tenant_config row that sets a
// shorter-than-global retention window for at least one signal — the input
// to `argvio-admin retention sweep` (internal/storage.SweepTenantRetention).
func (s *Store) ListRetentionOverrides(ctx context.Context) ([]Config, error) {
	rows, err := s.pool.Query(ctx, `
SELECT tenant_id, retention_traces_days, retention_logs_days, retention_metrics_days
FROM tenant_config
WHERE retention_traces_days IS NOT NULL
   OR retention_logs_days IS NOT NULL
   OR retention_metrics_days IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("tenant: list retention overrides: %w", err)
	}
	defer rows.Close()

	var out []Config
	for rows.Next() {
		var c Config
		if err := rows.Scan(&c.TenantID, &c.RetentionTracesDays, &c.RetentionLogsDays, &c.RetentionMetricsDays); err != nil {
			return nil, fmt.Errorf("tenant: scan retention override: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateAPIKey generates a new random key, stores only its hash, and
// returns the raw key exactly once — callers (cmd/admin) must display/hand
// it off immediately; it is never recoverable from storage afterward.
func (s *Store) CreateAPIKey(ctx context.Context, tenantID uuid.UUID, scope string) (rawKey string, key *APIKey, err error) {
	raw, err := generateRawKey(scope)
	if err != nil {
		return "", nil, fmt.Errorf("tenant: generate key: %w", err)
	}
	sum := sha256.Sum256([]byte(raw))
	prefix := raw[:min(len(raw), 12)]

	var rec APIKey
	err = s.pool.QueryRow(ctx, `
INSERT INTO api_keys (tenant_id, key_hash, key_prefix, scope)
VALUES ($1, $2, $3, $4)
RETURNING id, tenant_id, key_hash, key_prefix, scope, created_at, revoked_at, last_used_at`,
		tenantID, sum[:], prefix, scope,
	).Scan(&rec.ID, &rec.TenantID, &rec.KeyHash, &rec.KeyPrefix, &rec.Scope, &rec.CreatedAt, &rec.RevokedAt, &rec.LastUsedAt)
	if err != nil {
		return "", nil, fmt.Errorf("tenant: create api key: %w", err)
	}
	return raw, &rec, nil
}

// RevokeAPIKey marks a key unusable immediately (the public/metrics
// servers' cache will keep accepting it for up to the configured cache TTL
// — see docs/configuration.md — since this is a Postgres write, not a
// cache invalidation broadcast).
func (s *Store) RevokeAPIKey(ctx context.Context, keyID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, keyID)
	if err != nil {
		return fmt.Errorf("tenant: revoke api key: %w", err)
	}
	return nil
}

func generateRawKey(scope string) (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	envTag := "live"
	scopeTag := "pub"
	if scope == ScopeMetricsQuery {
		scopeTag = "qry"
	}
	return fmt.Sprintf("argv_%s_%s_%s", envTag, scopeTag, base64.RawURLEncoding.EncodeToString(buf)), nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
