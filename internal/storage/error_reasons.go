package storage

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrorReasonOther is what a cli.error.reason is stored as once its tenant's
// vocabulary is full. It is always admitted and doesn't count toward the cap.
const ErrorReasonOther = "other"

// vocabularyTouchInterval is how stale a capped vocabulary's last_seen
// (error_reasons, counter_names, counter_labels) may get before ingest moves
// it forward; finer precision isn't needed to age values out on a retention
// window measured in days.
const vocabularyTouchInterval = 24 * time.Hour

// AdmitErrorReasons decides which of one tenant's error reasons may be stored
// as-is. counts maps each distinct reason in a batch to the number of records
// carrying it. A reason is admitted if it is already in the tenant's
// vocabulary (error_reasons) or there is room for it under limit; new
// reasons are admitted most frequent first, then by name. Records whose
// reason isn't admitted are added to error_reason_overflow, and their count
// is returned as folded.
//
// The common case, every reason already known, is a single read. Admitting
// new reasons takes a per-tenant advisory lock, so two concurrent batches
// can't both take the last free slot.
func (w *Writer) AdmitErrorReasons(ctx context.Context, tenantID uuid.UUID, counts map[string]int64, limit int, at time.Time) (admitted map[string]bool, folded int64, err error) {
	admitted = map[string]bool{ErrorReasonOther: true}
	var names []string
	for r := range counts {
		if r != ErrorReasonOther {
			names = append(names, r)
		}
	}
	if len(names) == 0 {
		return admitted, 0, nil
	}
	sort.Strings(names)
	staleBefore := at.Add(-vocabularyTouchInterval)

	known, stale, err := knownErrorReasons(ctx, w.pool, tenantID, names, staleBefore)
	if err != nil {
		return nil, 0, err
	}
	if len(known) == len(names) {
		if stale {
			if _, err := w.pool.Exec(ctx, touchErrorReasons, tenantID, names, at, staleBefore); err != nil {
				return nil, 0, fmt.Errorf("storage: touch error reasons: %w", err)
			}
		}
		for _, r := range names {
			admitted[r] = true
		}
		return admitted, 0, nil
	}

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("storage: admit error reasons: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('error_reasons:' || $1::text, 0))`, tenantID); err != nil {
		return nil, 0, fmt.Errorf("storage: lock error reasons: %w", err)
	}
	// Re-read under the lock: another batch may have admitted some since.
	known, _, err = knownErrorReasons(ctx, tx, tenantID, names, staleBefore)
	if err != nil {
		return nil, 0, err
	}
	var size int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM error_reasons WHERE tenant_id = $1`, tenantID).Scan(&size); err != nil {
		return nil, 0, fmt.Errorf("storage: count error reasons: %w", err)
	}

	add, folded := planVocabulary(names, known, counts, limit-size)
	for _, r := range names {
		if known[r] {
			admitted[r] = true
		}
	}
	for _, r := range add {
		admitted[r] = true
	}

	if len(add) > 0 {
		if _, err := tx.Exec(ctx, `
INSERT INTO error_reasons (tenant_id, reason, first_seen, last_seen)
SELECT $1, r, $3, $3 FROM unnest($2::text[]) AS r
ON CONFLICT (tenant_id, reason) DO NOTHING`, tenantID, add, at); err != nil {
			return nil, 0, fmt.Errorf("storage: insert error reasons: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, touchErrorReasons, tenantID, names, at, staleBefore); err != nil {
		return nil, 0, fmt.Errorf("storage: touch error reasons: %w", err)
	}
	if folded > 0 {
		if _, err := tx.Exec(ctx, `
INSERT INTO error_reason_overflow (tenant_id, folded_count, last_folded_at) VALUES ($1, $2, $3)
ON CONFLICT (tenant_id) DO UPDATE SET
    folded_count   = error_reason_overflow.folded_count + EXCLUDED.folded_count,
    last_folded_at = greatest(error_reason_overflow.last_folded_at, EXCLUDED.last_folded_at)`, tenantID, folded, at); err != nil {
			return nil, 0, fmt.Errorf("storage: record error reason overflow: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, fmt.Errorf("storage: admit error reasons: %w", err)
	}
	return admitted, folded, nil
}

// planVocabulary picks which values not yet in a capped vocabulary fit in
// room free slots, most frequent first and then by name (names must be
// sorted), and how many records carry the rest.
func planVocabulary(names []string, known map[string]bool, counts map[string]int64, room int) (add []string, folded int64) {
	var fresh []string
	for _, r := range names {
		if !known[r] {
			fresh = append(fresh, r)
		}
	}
	sort.SliceStable(fresh, func(i, j int) bool { return counts[fresh[i]] > counts[fresh[j]] })
	for i, r := range fresh {
		if i < room {
			add = append(add, r)
		} else {
			folded += counts[r]
		}
	}
	return add, folded
}

const touchErrorReasons = `
UPDATE error_reasons SET last_seen = $3
WHERE tenant_id = $1 AND reason = ANY($2) AND last_seen < $4`

// querier is the read side shared by a pool and a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// knownErrorReasons returns which of names are in the tenant's vocabulary,
// and whether any of them was last seen before staleBefore.
func knownErrorReasons(ctx context.Context, q querier, tenantID uuid.UUID, names []string, staleBefore time.Time) (known map[string]bool, stale bool, err error) {
	rows, err := q.Query(ctx, `SELECT reason, last_seen FROM error_reasons WHERE tenant_id = $1 AND reason = ANY($2)`, tenantID, names)
	if err != nil {
		return nil, false, fmt.Errorf("storage: read error reasons: %w", err)
	}
	defer rows.Close()
	known = map[string]bool{}
	for rows.Next() {
		var r string
		var lastSeen time.Time
		if err := rows.Scan(&r, &lastSeen); err != nil {
			return nil, false, fmt.Errorf("storage: scan error reasons: %w", err)
		}
		known[r] = true
		stale = stale || lastSeen.Before(staleBefore)
	}
	return known, stale, rows.Err()
}
