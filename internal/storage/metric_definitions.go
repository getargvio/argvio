package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MetricDefinitionInput is one definition to apply: its name, its kind and
// the whole definition as JSON (internal/catalog.Metric).
type MetricDefinitionInput struct {
	Name string
	Kind string
	Spec json.RawMessage
}

// ApplyResult says what ApplyMetricDefinitions changed, by metric name.
type ApplyResult struct {
	Created    []string `json:"created"`
	Updated    []string `json:"updated"`
	Unchanged  []string `json:"unchanged"`
	Deprecated []string `json:"deprecated"`
}

// ApplyMetricDefinitions makes defs the catalog of one tenant's CLI
// (service): a new name is inserted, a changed or previously deprecated one
// updated, an identical one left alone, and a name of the service missing
// from defs gets deprecated_at (if it hasn't already). Applying the same
// definitions twice changes nothing. actor is recorded in updated_by on the
// rows it changes.
func (w *Writer) ApplyMetricDefinitions(ctx context.Context, tenantID uuid.UUID, service string, defs []MetricDefinitionInput, actor string, at time.Time) (ApplyResult, error) {
	res := ApplyResult{Created: []string{}, Updated: []string{}, Unchanged: []string{}, Deprecated: []string{}}
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("storage: apply metric definitions: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('metric definitions:' || $1::text || ':' || $2, 0))`, tenantID, service); err != nil {
		return res, fmt.Errorf("storage: lock metric definitions: %w", err)
	}

	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
		var inserted bool
		err := tx.QueryRow(ctx, `
INSERT INTO metric_definitions (tenant_id, service_name, name, kind, spec, created_at, updated_at, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $6, $7)
ON CONFLICT (tenant_id, service_name, name) DO UPDATE SET
    kind = EXCLUDED.kind, spec = EXCLUDED.spec, updated_at = EXCLUDED.updated_at,
    updated_by = EXCLUDED.updated_by, deprecated_at = NULL
WHERE metric_definitions.spec IS DISTINCT FROM EXCLUDED.spec OR metric_definitions.deprecated_at IS NOT NULL
RETURNING (xmax = 0)`, tenantID, service, d.Name, d.Kind, []byte(d.Spec), at, actor).Scan(&inserted)
		switch {
		case err == nil && inserted:
			res.Created = append(res.Created, d.Name)
		case err == nil:
			res.Updated = append(res.Updated, d.Name)
		case isNoRows(err):
			res.Unchanged = append(res.Unchanged, d.Name)
		default:
			return res, fmt.Errorf("storage: apply metric definition %s: %w", d.Name, err)
		}
	}

	rows, err := tx.Query(ctx, `
UPDATE metric_definitions SET deprecated_at = $4, updated_at = $4, updated_by = $5
WHERE tenant_id = $1 AND service_name = $2 AND deprecated_at IS NULL AND NOT (name = ANY($3))
RETURNING name`, tenantID, service, names, at, actor)
	if err != nil {
		return res, fmt.Errorf("storage: deprecate metric definitions: %w", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return res, fmt.Errorf("storage: scan deprecated metric definitions: %w", err)
		}
		res.Deprecated = append(res.Deprecated, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("storage: deprecate metric definitions: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("storage: apply metric definitions: %w", err)
	}
	sort.Strings(res.Deprecated)
	return res, nil
}

// MetricDefinition is one metric of a tenant's CLI, as the catalog declares
// it or, when Declared is false, as discovered from the data the CLI sent
// (Definition, UpdatedAt and UpdatedBy are then empty).
type MetricDefinition struct {
	Service      string          `json:"service"`
	Name         string          `json:"name"`
	Kind         string          `json:"kind"`
	Declared     bool            `json:"declared"`
	Definition   json.RawMessage `json:"definition,omitempty"`
	DeprecatedAt *time.Time      `json:"deprecated_at"`
	UpdatedAt    *time.Time      `json:"updated_at"`
	UpdatedBy    *string         `json:"updated_by"`
}

// MetricDefinitionDiscoveryWindow is how far back MetricDefinitions looks for
// metrics a CLI sent without declaring them.
const MetricDefinitionDiscoveryWindow = 30 * 24 * time.Hour

// MetricDefinitions lists a tenant's declared metric definitions, narrowed to
// services when non-empty, merged with the counters and measures its CLIs
// sent over the discovery window without declaring them (kind counter and
// duration; "other" is never listed). Ordered by service, then name.
func (q *QueryBuilder) MetricDefinitions(ctx context.Context, scope TenantScope, services []string, now time.Time) ([]MetricDefinition, error) {
	out := []MetricDefinition{}
	pb := newScopedPredicates(scope, "tenant_id")
	pb.addIfAnyStr("service_name", services)
	rows, err := q.pool.Query(ctx, fmt.Sprintf(`
SELECT service_name, name, kind, spec, deprecated_at, updated_at, updated_by
FROM metric_definitions WHERE %s`, pb.where()), pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: metric definitions: %w", err)
	}
	type key struct{ service, name, kind string }
	declared := map[key]bool{}
	for rows.Next() {
		d := MetricDefinition{Declared: true}
		var spec []byte
		var updatedAt time.Time
		var updatedBy string
		if err := rows.Scan(&d.Service, &d.Name, &d.Kind, &spec, &d.DeprecatedAt, &updatedAt, &updatedBy); err != nil {
			rows.Close()
			return nil, fmt.Errorf("storage: scan metric definitions: %w", err)
		}
		d.Definition, d.UpdatedAt, d.UpdatedBy = spec, &updatedAt, &updatedBy
		declared[key{d.Service, d.Name, d.Kind}] = true
		out = append(out, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: metric definitions: %w", err)
	}

	pb = newScopedPredicates(scope, "tenant_id")
	pb.add("bucket >= $%d", now.Add(-MetricDefinitionDiscoveryWindow))
	pb.addIfAnyStr("service_name", services)
	where := pb.where() + " AND service_name IS NOT NULL"
	rows, err = q.pool.Query(ctx, fmt.Sprintf(`
SELECT service_name, counter_name, 'counter' FROM %[1]s WHERE %[5]s AND counter_name <> 'other'
UNION
SELECT service_name, counter_name, 'counter' FROM %[2]s WHERE %[5]s AND counter_name <> 'other'
UNION
SELECT service_name, measure_name, 'duration' FROM %[3]s WHERE %[5]s AND measure_name <> 'other'
UNION
SELECT service_name, measure_name, 'duration' FROM %[4]s WHERE %[5]s AND measure_name <> 'other'`,
		caggCountersHourly, caggCountersDaily, caggMeasuresHourly, caggMeasuresDaily, where), pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: discovered metrics: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d MetricDefinition
		if err := rows.Scan(&d.Service, &d.Name, &d.Kind); err != nil {
			return nil, fmt.Errorf("storage: scan discovered metrics: %w", err)
		}
		if !declared[key{d.Service, d.Name, d.Kind}] {
			out = append(out, d)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: discovered metrics: %w", err)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Kind < out[j].Kind
	})
	return out, nil
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// CatalogKey identifies one declared metric: the CLI (service.name) and the
// metric name.
type CatalogKey struct {
	Service, Name string
}

// CatalogRule is what ingest checks a declared metric's data points against.
type CatalogRule struct {
	Kind    string
	Dims    []string
	MinTier string
}

// MetricCatalogRules returns the tenant's declared, non-deprecated metrics,
// for ingest. A deprecated metric is treated as undeclared.
func (w *Writer) MetricCatalogRules(ctx context.Context, tenantID uuid.UUID) (map[CatalogKey]CatalogRule, error) {
	rows, err := w.pool.Query(ctx, `
SELECT service_name, name, kind,
       coalesce(ARRAY(SELECT jsonb_array_elements_text(coalesce(spec -> 'dims', '[]'::jsonb))), '{}'),
       coalesce(spec ->> 'min_tier', '')
FROM metric_definitions WHERE tenant_id = $1 AND deprecated_at IS NULL`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("storage: metric catalog rules: %w", err)
	}
	defer rows.Close()
	out := map[CatalogKey]CatalogRule{}
	for rows.Next() {
		var k CatalogKey
		var r CatalogRule
		if err := rows.Scan(&k.Service, &k.Name, &r.Kind, &r.Dims, &r.MinTier); err != nil {
			return nil, fmt.Errorf("storage: scan metric catalog rules: %w", err)
		}
		out[k] = r
	}
	return out, rows.Err()
}
