package storage

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Log event names install tracking is derived from.
const (
	eventSessionStarted   = "cli.session.started"
	eventCommandCompleted = "cli.command.completed"

	attrInstallID     = "cli.install_id"
	attrInstallMethod = "cli.install.method"
)

// InstallEvent is the part of one log record install tracking cares about.
type InstallEvent struct {
	TenantID    uuid.UUID
	InstallID   string
	At          time.Time
	Version     *string
	OS          *string
	Arch        *string
	Completed   bool   // a cli.command.completed event (otherwise a session start)
	ExitCode    *int32 // only meaningful when Completed
	InstallMeth string // only set on session starts
}

// InstallEventFromLog extracts an InstallEvent from a log row, reporting
// false for rows that don't take part in install tracking: other events, or
// records without an install ID (anonymous-tier sessions).
func InstallEventFromLog(r LogRow) (InstallEvent, bool) {
	if r.EventName != eventSessionStarted && r.EventName != eventCommandCompleted {
		return InstallEvent{}, false
	}
	id, _ := r.LogAttributes[attrInstallID].(string)
	if id == "" {
		return InstallEvent{}, false
	}
	method, _ := r.LogAttributes[attrInstallMethod].(string)
	return InstallEvent{
		TenantID: r.TenantID, InstallID: id, At: r.Time,
		Version: r.CLIVersion, OS: r.OS, Arch: r.Arch,
		Completed: r.EventName == eventCommandCompleted, ExitCode: r.ExitCode,
		InstallMeth: method,
	}, true
}

// installState mirrors one row of the installs table.
type installState struct {
	FirstSeen, LastSeen            time.Time
	FirstVersion, LastVersion      *string
	LastOS, LastArch               *string
	InstallMethod                  *string
	FirstCommandAt, FirstSuccessAt *time.Time
}

type versionChange struct {
	At       time.Time
	From, To string
	Upgrade  bool
}

func (c versionChange) direction() string {
	if c.Upgrade {
		return "upgrade"
	}
	return "downgrade"
}

func earliest(cur *time.Time, t time.Time) *time.Time {
	if cur == nil || t.Before(*cur) {
		return &t
	}
	return cur
}

// planInstall folds events for one install into its stored state (nil when
// the install is new) and reports the version changes they reveal. It is
// pure so the merge rules can be tested without a database.
//
// Events may arrive out of order and in any batching: only events newer than
// the stored last_seen can change last_* fields or produce a version change,
// and older ones can only move first_seen/first_version earlier.
func planInstall(existing *installState, events []InstallEvent) (installState, []versionChange) {
	sorted := append([]InstallEvent(nil), events...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].At.Before(sorted[j].At) })

	var st installState
	if existing != nil {
		st = *existing
	} else {
		first := sorted[0]
		st = installState{
			FirstSeen: first.At, LastSeen: first.At,
			FirstVersion: first.Version, LastVersion: first.Version,
			LastOS: first.OS, LastArch: first.Arch,
		}
	}

	var changes []versionChange
	for _, e := range sorted {
		if e.At.Before(st.FirstSeen) {
			st.FirstSeen = e.At
			if e.Version != nil {
				st.FirstVersion = e.Version
			}
		}
		if !e.At.Before(st.LastSeen) {
			if e.Version != nil {
				if st.LastVersion != nil && *st.LastVersion != *e.Version && e.At.After(st.LastSeen) {
					if cmp, ok := compareVersions(*st.LastVersion, *e.Version); ok && cmp != 0 {
						changes = append(changes, versionChange{At: e.At, From: *st.LastVersion, To: *e.Version, Upgrade: cmp < 0})
					}
				}
				st.LastVersion = e.Version
			}
			if e.OS != nil {
				st.LastOS = e.OS
			}
			if e.Arch != nil {
				st.LastArch = e.Arch
			}
			st.LastSeen = e.At
		}
		if e.InstallMeth != "" {
			m := e.InstallMeth
			st.InstallMethod = &m
		}
		if e.Completed {
			st.FirstCommandAt = earliest(st.FirstCommandAt, e.At)
			if e.ExitCode != nil && *e.ExitCode == 0 {
				st.FirstSuccessAt = earliest(st.FirstSuccessAt, e.At)
			}
		}
	}
	return st, changes
}

type installKey struct {
	tenant uuid.UUID
	id     string
}

const selectInstallsForUpdate = `
SELECT first_seen, last_seen, first_version, last_version, last_os, last_arch,
       install_method, first_command_at, first_success_at
FROM installs
WHERE tenant_id = $1 AND install_id = $2
FOR UPDATE`

// The merge semantics here make the upsert safe against a concurrent writer
// creating the same row, and against replays: timestamps only move in their
// "natural" direction (LEAST ignores NULLs), and last_* fields only change
// for events at least as new as what is stored.
const upsertInstall = `
INSERT INTO installs (tenant_id, install_id, first_seen, last_seen, first_version, last_version,
                      last_os, last_arch, install_method, first_command_at, first_success_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (tenant_id, install_id) DO UPDATE SET
    first_version    = CASE WHEN EXCLUDED.first_seen < installs.first_seen
                            THEN COALESCE(EXCLUDED.first_version, installs.first_version)
                            ELSE installs.first_version END,
    last_version     = CASE WHEN EXCLUDED.last_seen >= installs.last_seen
                            THEN COALESCE(EXCLUDED.last_version, installs.last_version)
                            ELSE installs.last_version END,
    last_os          = CASE WHEN EXCLUDED.last_seen >= installs.last_seen
                            THEN COALESCE(EXCLUDED.last_os, installs.last_os)
                            ELSE installs.last_os END,
    last_arch        = CASE WHEN EXCLUDED.last_seen >= installs.last_seen
                            THEN COALESCE(EXCLUDED.last_arch, installs.last_arch)
                            ELSE installs.last_arch END,
    install_method   = COALESCE(EXCLUDED.install_method, installs.install_method),
    first_seen       = LEAST(installs.first_seen, EXCLUDED.first_seen),
    last_seen        = GREATEST(installs.last_seen, EXCLUDED.last_seen),
    first_command_at = LEAST(installs.first_command_at, EXCLUDED.first_command_at),
    first_success_at = LEAST(installs.first_success_at, EXCLUDED.first_success_at)`

const insertVersionChange = `
INSERT INTO install_version_changes (tenant_id, install_id, at, from_version, to_version, direction)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT DO NOTHING`

// TrackInstalls folds events into the installs and install_version_changes
// tables in one transaction. Events for any number of installs and tenants
// may be mixed; installs are locked in a stable order so concurrent batches
// can't deadlock.
func (w *Writer) TrackInstalls(ctx context.Context, events []InstallEvent) error {
	if len(events) == 0 {
		return nil
	}
	byInstall := map[installKey][]InstallEvent{}
	for _, e := range events {
		k := installKey{e.TenantID, e.InstallID}
		byInstall[k] = append(byInstall[k], e)
	}
	keys := make([]installKey, 0, len(byInstall))
	for k := range byInstall {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if c := compareUUID(keys[i].tenant, keys[j].tenant); c != 0 {
			return c < 0
		}
		return keys[i].id < keys[j].id
	})

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("storage: track installs: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, k := range keys {
		var existing *installState
		var st installState
		err := tx.QueryRow(ctx, selectInstallsForUpdate, k.tenant, k.id).Scan(
			&st.FirstSeen, &st.LastSeen, &st.FirstVersion, &st.LastVersion, &st.LastOS, &st.LastArch,
			&st.InstallMethod, &st.FirstCommandAt, &st.FirstSuccessAt)
		switch {
		case err == nil:
			existing = &st
		case err != pgx.ErrNoRows:
			return fmt.Errorf("storage: track installs: read: %w", err)
		}

		next, changes := planInstall(existing, byInstall[k])
		if _, err := tx.Exec(ctx, upsertInstall, k.tenant, k.id, next.FirstSeen, next.LastSeen,
			next.FirstVersion, next.LastVersion, next.LastOS, next.LastArch, next.InstallMethod,
			next.FirstCommandAt, next.FirstSuccessAt); err != nil {
			return fmt.Errorf("storage: track installs: upsert: %w", err)
		}
		for _, c := range changes {
			if _, err := tx.Exec(ctx, insertVersionChange, k.tenant, k.id, c.At, c.From, c.To, c.direction()); err != nil {
				return fmt.Errorf("storage: track installs: version change: %w", err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("storage: track installs: commit: %w", err)
	}
	return nil
}

func compareUUID(a, b uuid.UUID) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// backfillChunk is how many events BackfillInstalls hands to TrackInstalls
// at a time.
const backfillChunk = 2000

// BackfillInstalls rebuilds install state from raw logs at or after since,
// replaying them oldest first through the same path as live ingest. Run it
// once after migrating (before or while ingest is live); rerunning is safe
// but can't recover version changes older than what ingest has since stored.
// It returns how many events it replayed.
func BackfillInstalls(ctx context.Context, pool *pgxpool.Pool, since time.Time) (int64, error) {
	rows, err := pool.Query(ctx, `
SELECT tenant_id, log_attributes ->> 'cli.install_id', time, cli_version, os, arch,
       event_name = 'cli.command.completed', exit_code, coalesce(log_attributes ->> 'cli.install.method', '')
FROM logs
WHERE time >= $1
  AND event_name IN ('cli.session.started', 'cli.command.completed')
  AND log_attributes ? 'cli.install_id'
ORDER BY time ASC`, since)
	if err != nil {
		return 0, fmt.Errorf("storage: backfill installs: %w", err)
	}
	defer rows.Close()

	w := NewWriter(pool)
	var total int64
	chunk := make([]InstallEvent, 0, backfillChunk)
	flush := func() error {
		if err := w.TrackInstalls(ctx, chunk); err != nil {
			return err
		}
		total += int64(len(chunk))
		chunk = chunk[:0]
		return nil
	}
	for rows.Next() {
		var e InstallEvent
		if err := rows.Scan(&e.TenantID, &e.InstallID, &e.At, &e.Version, &e.OS, &e.Arch,
			&e.Completed, &e.ExitCode, &e.InstallMeth); err != nil {
			return total, fmt.Errorf("storage: backfill installs: scan: %w", err)
		}
		if e.InstallID == "" {
			continue
		}
		chunk = append(chunk, e)
		if len(chunk) == backfillChunk {
			if err := flush(); err != nil {
				return total, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return total, fmt.Errorf("storage: backfill installs: %w", err)
	}
	if err := flush(); err != nil {
		return total, err
	}
	return total, nil
}

// SweepTenantInstalls deletes one tenant's installs not seen for more than
// days, and their version changes. It returns the number of installs deleted.
func SweepTenantInstalls(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, days int) (int64, error) {
	return sweepInstalls(ctx, pool, days, `tenant_id = $2`, tenantID)
}

// SweepInstallsExcept applies the global retention window to every tenant
// except those with their own override (which SweepTenantInstalls handles).
func SweepInstallsExcept(ctx context.Context, pool *pgxpool.Pool, days int, except []uuid.UUID) (int64, error) {
	if except == nil {
		except = []uuid.UUID{}
	}
	return sweepInstalls(ctx, pool, days, `NOT (tenant_id = ANY($2))`, except)
}

func sweepInstalls(ctx context.Context, pool *pgxpool.Pool, days int, tenantPred string, tenantArg any) (int64, error) {
	if days <= 0 {
		return 0, fmt.Errorf("storage: sweep installs: days must be positive, got %d", days)
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("storage: sweep installs: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM install_version_changes WHERE at < $1 AND `+tenantPred, cutoff, tenantArg); err != nil {
		return 0, fmt.Errorf("storage: sweep install version changes: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM installs WHERE last_seen < $1 AND `+tenantPred, cutoff, tenantArg)
	if err != nil {
		return 0, fmt.Errorf("storage: sweep installs: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("storage: sweep installs: %w", err)
	}
	return tag.RowsAffected(), nil
}
