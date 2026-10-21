package storage

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// CounterOther is what a cli.counter.name or cli.counter.label is stored as
// once its vocabulary is full. Like ErrorReasonOther it is always admitted and
// never counts toward a cap.
const CounterOther = "other"

// CounterKey is one counter series in a batch: a cli.counter.name and its
// optional cli.counter.label ("" for none).
type CounterKey struct {
	Name  string
	Label string
}

// CounterAdmission is AdmitCounters' decision for one batch. A record whose
// name isn't in Names is stored under CounterOther with no label; a record
// whose name is admitted but whose (name, label) isn't in Labels keeps its
// name and has its label stored as CounterOther. FoldedNames and FoldedLabels
// count the records each case applied to.
type CounterAdmission struct {
	Names        map[string]bool
	Labels       map[CounterKey]bool
	FoldedNames  int64
	FoldedLabels int64
}

// vocabulary names the tables behind one capped (name, label) vocabulary, so
// vendor counters (migration 0013) and vendor measures (0014) share one
// admission implementation. All fields are constants, never user input.
type vocabulary struct {
	noun       string // singular; for error messages and the advisory lock key
	names      string // table: tenant_id, <nameCol>, first_seen, last_seen
	labels     string // table: tenant_id, <nameCol>, label, first_seen, last_seen
	overflow   string // table: tenant_id, folded_names, folded_labels, last_folded_at
	nameColumn string
}

// sql fills the {names}, {labels}, {overflow}, {name} and {noun} placeholders
// of a statement with v's identifiers.
func (v vocabulary) sql(stmt string) string {
	return strings.NewReplacer("{names}", v.names, "{labels}", v.labels, "{overflow}", v.overflow,
		"{name}", v.nameColumn, "{noun}", v.noun).Replace(stmt)
}

var counterVocabulary = vocabulary{
	noun: "counter", names: "counter_names", labels: "counter_labels", overflow: "counter_overflow", nameColumn: "counter_name",
}

// AdmitCounters decides which of one tenant's counter names and labels may be
// stored as-is. counts maps each distinct (name, label) in a batch to the
// number of data points carrying it. Names are capped at nameLimit per tenant
// (counter_names) and labels at labelLimit per name (counter_labels); new
// values are admitted while there is room, most frequent first, then by name,
// exactly as AdmitErrorReasons does. Folded data points are added to
// counter_overflow.
//
// The common case, every name and label already known, is two reads.
// Admitting new values takes a per-tenant advisory lock, so two concurrent
// batches can't both take the last free slot.
func (w *Writer) AdmitCounters(ctx context.Context, tenantID uuid.UUID, counts map[CounterKey]int64, nameLimit, labelLimit int, at time.Time) (CounterAdmission, error) {
	return w.admit(ctx, counterVocabulary, tenantID, counts, nameLimit, labelLimit, at)
}

func (w *Writer) admit(ctx context.Context, v vocabulary, tenantID uuid.UUID, counts map[CounterKey]int64, nameLimit, labelLimit int, at time.Time) (CounterAdmission, error) {
	adm := CounterAdmission{Names: map[string]bool{CounterOther: true}, Labels: map[CounterKey]bool{}}
	nameCounts := map[string]int64{}
	labelCounts := map[string]map[string]int64{}
	for k, n := range counts {
		if k.Name == CounterOther {
			continue
		}
		nameCounts[k.Name] += n
		if k.Label == "" || k.Label == CounterOther {
			continue
		}
		if labelCounts[k.Name] == nil {
			labelCounts[k.Name] = map[string]int64{}
		}
		labelCounts[k.Name][k.Label] += n
	}
	if len(nameCounts) == 0 {
		return adm, nil
	}
	names := sortedKeys(nameCounts)
	var pairs []CounterKey
	for _, name := range names {
		for _, label := range sortedKeys(labelCounts[name]) {
			pairs = append(pairs, CounterKey{name, label})
		}
	}
	staleBefore := at.Add(-vocabularyTouchInterval)

	known, err := knownVocabulary(ctx, v, w.pool, tenantID, names, pairs, staleBefore)
	if err != nil {
		return CounterAdmission{}, err
	}
	if len(known.names) == len(names) && len(known.labels) == len(pairs) {
		if known.stale {
			if err := touchVocabulary(ctx, v, w.pool, tenantID, names, pairs, at, staleBefore); err != nil {
				return CounterAdmission{}, err
			}
		}
		for _, name := range names {
			adm.Names[name] = true
		}
		for _, k := range pairs {
			adm.Labels[k] = true
		}
		return adm, nil
	}

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return CounterAdmission{}, fmt.Errorf("storage: admit %ss: %w", v.noun, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, v.sql(`SELECT pg_advisory_xact_lock(hashtextextended('{noun}s:' || $1::text, 0))`), tenantID); err != nil {
		return CounterAdmission{}, fmt.Errorf("storage: lock %ss: %w", v.noun, err)
	}
	// Re-read under the lock: another batch may have admitted some since.
	known, err = knownVocabulary(ctx, v, tx, tenantID, names, pairs, staleBefore)
	if err != nil {
		return CounterAdmission{}, err
	}
	var nameSize int
	if err := tx.QueryRow(ctx, v.sql(`SELECT count(*) FROM {names} WHERE tenant_id = $1`), tenantID).Scan(&nameSize); err != nil {
		return CounterAdmission{}, fmt.Errorf("storage: count %s names: %w", v.noun, err)
	}
	addNames, foldedNames := planVocabulary(names, known.names, nameCounts, nameLimit-nameSize)
	for _, name := range names {
		if known.names[name] {
			adm.Names[name] = true
		}
	}
	for _, name := range addNames {
		adm.Names[name] = true
	}
	adm.FoldedNames = foldedNames

	// Labels only matter under a name that is kept.
	var keptNames []string
	for _, name := range names {
		if adm.Names[name] && len(labelCounts[name]) > 0 {
			keptNames = append(keptNames, name)
		}
	}
	labelSize := map[string]int{}
	if len(keptNames) > 0 {
		rows, err := tx.Query(ctx, v.sql(`SELECT {name}, count(*) FROM {labels} WHERE tenant_id = $1 AND {name} = ANY($2) GROUP BY 1`), tenantID, keptNames)
		if err != nil {
			return CounterAdmission{}, fmt.Errorf("storage: count %s labels: %w", v.noun, err)
		}
		for rows.Next() {
			var name string
			var n int
			if err := rows.Scan(&name, &n); err != nil {
				rows.Close()
				return CounterAdmission{}, fmt.Errorf("storage: scan %s labels: %w", v.noun, err)
			}
			labelSize[name] = n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return CounterAdmission{}, fmt.Errorf("storage: count %s labels: %w", v.noun, err)
		}
	}
	var addLabels []CounterKey
	for _, name := range keptNames {
		labels := sortedKeys(labelCounts[name])
		knownLabels := map[string]bool{}
		for _, label := range labels {
			if known.labels[CounterKey{name, label}] {
				knownLabels[label] = true
				adm.Labels[CounterKey{name, label}] = true
			}
		}
		add, folded := planVocabulary(labels, knownLabels, labelCounts[name], labelLimit-labelSize[name])
		for _, label := range add {
			adm.Labels[CounterKey{name, label}] = true
			addLabels = append(addLabels, CounterKey{name, label})
		}
		adm.FoldedLabels += folded
	}

	if len(addNames) > 0 {
		if _, err := tx.Exec(ctx, v.sql(`
INSERT INTO {names} (tenant_id, {name}, first_seen, last_seen)
SELECT $1, n, $3, $3 FROM unnest($2::text[]) AS n
ON CONFLICT (tenant_id, {name}) DO NOTHING`), tenantID, addNames, at); err != nil {
			return CounterAdmission{}, fmt.Errorf("storage: insert %s names: %w", v.noun, err)
		}
	}
	if len(addLabels) > 0 {
		addNamesCol, addLabelsCol := splitCounterKeys(addLabels)
		if _, err := tx.Exec(ctx, v.sql(`
INSERT INTO {labels} (tenant_id, {name}, label, first_seen, last_seen)
SELECT $1, n, l, $4, $4 FROM unnest($2::text[], $3::text[]) AS t(n, l)
ON CONFLICT (tenant_id, {name}, label) DO NOTHING`), tenantID, addNamesCol, addLabelsCol, at); err != nil {
			return CounterAdmission{}, fmt.Errorf("storage: insert %s labels: %w", v.noun, err)
		}
	}
	if err := touchVocabulary(ctx, v, tx, tenantID, names, pairs, at, staleBefore); err != nil {
		return CounterAdmission{}, err
	}
	if adm.FoldedNames > 0 || adm.FoldedLabels > 0 {
		if _, err := tx.Exec(ctx, v.sql(`
INSERT INTO {overflow} (tenant_id, folded_names, folded_labels, last_folded_at) VALUES ($1, $2, $3, $4)
ON CONFLICT (tenant_id) DO UPDATE SET
    folded_names   = {overflow}.folded_names + EXCLUDED.folded_names,
    folded_labels  = {overflow}.folded_labels + EXCLUDED.folded_labels,
    last_folded_at = greatest({overflow}.last_folded_at, EXCLUDED.last_folded_at)`),
			tenantID, adm.FoldedNames, adm.FoldedLabels, at); err != nil {
			return CounterAdmission{}, fmt.Errorf("storage: record %s overflow: %w", v.noun, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return CounterAdmission{}, fmt.Errorf("storage: admit %ss: %w", v.noun, err)
	}
	return adm, nil
}

// knownCounterSet is which of a batch's names and (name, label) pairs are
// already in the tenant's vocabulary, and whether any was last seen before
// the touch threshold.
type knownCounterSet struct {
	names  map[string]bool
	labels map[CounterKey]bool
	stale  bool
}

func knownVocabulary(ctx context.Context, v vocabulary, q querier, tenantID uuid.UUID, names []string, pairs []CounterKey, staleBefore time.Time) (knownCounterSet, error) {
	known := knownCounterSet{names: map[string]bool{}, labels: map[CounterKey]bool{}}
	rows, err := q.Query(ctx, v.sql(`SELECT {name}, last_seen FROM {names} WHERE tenant_id = $1 AND {name} = ANY($2)`), tenantID, names)
	if err != nil {
		return known, fmt.Errorf("storage: read %s names: %w", v.noun, err)
	}
	for rows.Next() {
		var name string
		var lastSeen time.Time
		if err := rows.Scan(&name, &lastSeen); err != nil {
			rows.Close()
			return known, fmt.Errorf("storage: scan %s names: %w", v.noun, err)
		}
		known.names[name] = true
		known.stale = known.stale || lastSeen.Before(staleBefore)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return known, fmt.Errorf("storage: read %s names: %w", v.noun, err)
	}
	if len(pairs) == 0 {
		return known, nil
	}

	nameCol, labelCol := splitCounterKeys(pairs)
	rows, err = q.Query(ctx, v.sql(`
SELECT {name}, label, last_seen FROM {labels}
WHERE tenant_id = $1 AND ({name}, label) IN (SELECT * FROM unnest($2::text[], $3::text[]))`), tenantID, nameCol, labelCol)
	if err != nil {
		return known, fmt.Errorf("storage: read %s labels: %w", v.noun, err)
	}
	defer rows.Close()
	for rows.Next() {
		var k CounterKey
		var lastSeen time.Time
		if err := rows.Scan(&k.Name, &k.Label, &lastSeen); err != nil {
			return known, fmt.Errorf("storage: scan %s labels: %w", v.noun, err)
		}
		known.labels[k] = true
		known.stale = known.stale || lastSeen.Before(staleBefore)
	}
	return known, rows.Err()
}

// execer is the write side shared by a pool and a transaction.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// touchVocabulary moves last_seen forward on the batch's names and labels that
// were last seen before staleBefore. Values not in the vocabulary are no-ops.
func touchVocabulary(ctx context.Context, v vocabulary, e execer, tenantID uuid.UUID, names []string, pairs []CounterKey, at, staleBefore time.Time) error {
	if _, err := e.Exec(ctx, v.sql(`
UPDATE {names} SET last_seen = $3
WHERE tenant_id = $1 AND {name} = ANY($2) AND last_seen < $4`), tenantID, names, at, staleBefore); err != nil {
		return fmt.Errorf("storage: touch %s names: %w", v.noun, err)
	}
	if len(pairs) == 0 {
		return nil
	}
	nameCol, labelCol := splitCounterKeys(pairs)
	if _, err := e.Exec(ctx, v.sql(`
UPDATE {labels} SET last_seen = $4
WHERE tenant_id = $1 AND ({name}, label) IN (SELECT * FROM unnest($2::text[], $3::text[])) AND last_seen < $5`),
		tenantID, nameCol, labelCol, at, staleBefore); err != nil {
		return fmt.Errorf("storage: touch %s labels: %w", v.noun, err)
	}
	return nil
}

func splitCounterKeys(keys []CounterKey) (names, labels []string) {
	names = make([]string, len(keys))
	labels = make([]string, len(keys))
	for i, k := range keys {
		names[i], labels[i] = k.Name, k.Label
	}
	return names, labels
}

func sortedKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
