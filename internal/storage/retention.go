package storage

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sweepableTables is the fixed allowlist of tables SweepTenantRetention may
// target — the table name is never taken from arbitrary input, only these
// three constants, since it can't be parameterized as a bind variable.
var sweepableTables = map[string]bool{TableTraces: true, TableLogs: true, TableMetrics: true}

// SweepTenantRetention deletes rows for one tenant older than
// retentionDays. This exists because a per-tenant retention window shorter
// than the hypertable's global Timescale retention policy can't be
// expressed as a chunk-drop policy once tenant_id is a hash-partitioned
// dimension (a chunk holds a slice of many tenants for a given time range,
// so Timescale's automatic retention can only enforce one — the longest —
// window per hypertable). See docs/schema.md "Per-tenant retention".
//
// Intended to run periodically (e.g. daily cron calling
// `argvio-admin retention sweep`), not on any request path.
func SweepTenantRetention(ctx context.Context, pool *pgxpool.Pool, table string, tenantID uuid.UUID, retentionDays int) (rowsDeleted int64, err error) {
	if !sweepableTables[table] {
		return 0, fmt.Errorf("storage: %q is not a sweepable table", table)
	}
	if retentionDays <= 0 {
		return 0, fmt.Errorf("storage: retentionDays must be > 0")
	}
	sql := fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = $1 AND time < now() - make_interval(days => $2)`, table)
	tag, err := pool.Exec(ctx, sql, tenantID, retentionDays)
	if err != nil {
		return 0, fmt.Errorf("storage: sweep %s for tenant %s: %w", table, tenantID, err)
	}
	return tag.RowsAffected(), nil
}
