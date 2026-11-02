package storage

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Dimensions of vendor counters and measures. A data point's cli.dim.<key>
// attributes are stored as one canonical tuple in its label: the keys sorted,
// each written key=value, joined with ";" ("check=bola;severity=high").
// Keys and values are identifiers (^[a-z0-9][a-z0-9._-]*$), so neither
// separator can appear inside them.
const (
	dimPairSep = ";"
	dimKVSep   = "="
	// DimLabelKey is the dimension a plain label reads as: a label without
	// "=" ("bearer", from SDKs that predate dimensions) is {label: bearer}.
	DimLabelKey = "label"
)

// FormatDims returns the canonical tuple of dims; "" for none.
func FormatDims(dims map[string]string) string {
	keys := make([]string, 0, len(dims))
	for k := range dims {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString(dimPairSep)
		}
		b.WriteString(k)
		b.WriteString(dimKVSep)
		b.WriteString(dims[k])
	}
	return b.String()
}

// ParseDims reads a stored label back into dimensions. A label without "="
// is the single dimension DimLabelKey; "" is none. The label "other" (a whole
// tuple folded by the label cap) is {label: other}: which dimensions it had
// is not known, so a query grouping by any key sees it under "other" (see
// DimValue).
func ParseDims(label string) map[string]string {
	if label == "" {
		return map[string]string{}
	}
	if !strings.Contains(label, dimKVSep) {
		return map[string]string{DimLabelKey: label}
	}
	dims := map[string]string{}
	for _, pair := range strings.Split(label, dimPairSep) {
		k, v, ok := strings.Cut(pair, dimKVSep)
		if ok {
			dims[k] = v
		}
	}
	return dims
}

// DimValue is the value of key in a stored label, for grouping and
// filtering. ok is false when the label doesn't carry key. A label folded
// whole into "other" carries every key, as "other".
func DimValue(label, key string) (value string, ok bool) {
	if label == CounterOther {
		return CounterOther, true
	}
	value, ok = ParseDims(label)[key]
	return value, ok
}

// DimValueKey is one value of one dimension key under a counter or measure
// name.
type DimValueKey struct {
	Name, Key, Value string
}

// DimAdmission is AdmitCounterDims' decision for one batch: the values that
// may be stored as-is. Folded is how many of the batch's (name, key, value)
// occurrences were past the cap.
type DimAdmission struct {
	Values map[DimValueKey]bool
	Folded int64
}

// AdmitCounterDims decides which dimension values of a tenant's counters may
// be stored as-is: at most limit distinct values per (name, key), new ones
// admitted while there is room, most frequent first. counts maps each
// distinct (name, key, value) in a batch to the number of data points
// carrying it; "other" values are always admitted. foldedPoints, the number
// of data points that will have at least one value folded, is added to
// counter_overflow.folded_dim_values by the caller through
// RecordFoldedCounterDims, since only the caller knows which points those
// are.
func (w *Writer) AdmitCounterDims(ctx context.Context, tenantID uuid.UUID, counts map[DimValueKey]int64, limit int, at time.Time) (DimAdmission, error) {
	return w.admitDims(ctx, counterVocabulary, tenantID, counts, limit, at)
}

// AdmitMeasureDims is AdmitCounterDims for measures.
func (w *Writer) AdmitMeasureDims(ctx context.Context, tenantID uuid.UUID, counts map[DimValueKey]int64, limit int, at time.Time) (DimAdmission, error) {
	return w.admitDims(ctx, measureVocabulary, tenantID, counts, limit, at)
}

// RecordFoldedCounterDims adds n data points to the tenant's
// counter_overflow.folded_dim_values.
func (w *Writer) RecordFoldedCounterDims(ctx context.Context, tenantID uuid.UUID, n int64, at time.Time) error {
	return w.recordFoldedDims(ctx, counterVocabulary, tenantID, n, at)
}

// RecordFoldedMeasureDims is RecordFoldedCounterDims for measures.
func (w *Writer) RecordFoldedMeasureDims(ctx context.Context, tenantID uuid.UUID, n int64, at time.Time) error {
	return w.recordFoldedDims(ctx, measureVocabulary, tenantID, n, at)
}

func (w *Writer) recordFoldedDims(ctx context.Context, v vocabulary, tenantID uuid.UUID, n int64, at time.Time) error {
	if n <= 0 {
		return nil
	}
	if _, err := w.pool.Exec(ctx, v.sql(`
INSERT INTO {overflow} (tenant_id, folded_names, folded_labels, folded_dim_values, last_folded_at) VALUES ($1, 0, 0, $2, $3)
ON CONFLICT (tenant_id) DO UPDATE SET
    folded_dim_values = {overflow}.folded_dim_values + EXCLUDED.folded_dim_values,
    last_folded_at    = greatest({overflow}.last_folded_at, EXCLUDED.last_folded_at)`), tenantID, n, at); err != nil {
		return fmt.Errorf("storage: record %s dimension overflow: %w", v.noun, err)
	}
	return nil
}

func (w *Writer) admitDims(ctx context.Context, v vocabulary, tenantID uuid.UUID, counts map[DimValueKey]int64, limit int, at time.Time) (DimAdmission, error) {
	adm := DimAdmission{Values: map[DimValueKey]bool{}}
	var keys []DimValueKey
	for k := range counts {
		if k.Value == CounterOther || k.Name == CounterOther {
			adm.Values[k] = true
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return adm, nil
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		return a.Value < b.Value
	})
	staleBefore := at.Add(-vocabularyTouchInterval)

	known, stale, err := knownDims(ctx, v, w.pool, tenantID, keys, staleBefore)
	if err != nil {
		return DimAdmission{}, err
	}
	if len(known) == len(keys) {
		if stale {
			if err := touchDims(ctx, v, w.pool, tenantID, keys, at, staleBefore); err != nil {
				return DimAdmission{}, err
			}
		}
		for _, k := range keys {
			adm.Values[k] = true
		}
		return adm, nil
	}

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return DimAdmission{}, fmt.Errorf("storage: admit %s dimensions: %w", v.noun, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, v.sql(`SELECT pg_advisory_xact_lock(hashtextextended('{noun} dims:' || $1::text, 0))`), tenantID); err != nil {
		return DimAdmission{}, fmt.Errorf("storage: lock %s dimensions: %w", v.noun, err)
	}
	if known, _, err = knownDims(ctx, v, tx, tenantID, keys, staleBefore); err != nil {
		return DimAdmission{}, err
	}

	// Group the batch per (name, key): each group has its own cap.
	type group struct{ name, key string }
	values := map[group][]string{}
	valueCounts := map[group]map[string]int64{}
	knownValues := map[group]map[string]bool{}
	var groups []group
	for _, k := range keys {
		g := group{k.Name, k.Key}
		if _, ok := values[g]; !ok {
			groups = append(groups, g)
			valueCounts[g] = map[string]int64{}
			knownValues[g] = map[string]bool{}
		}
		values[g] = append(values[g], k.Value)
		valueCounts[g][k.Value] = counts[k]
		if known[k] {
			knownValues[g][k.Value] = true
			adm.Values[k] = true
		}
	}

	groupNames, groupKeys := make([]string, len(groups)), make([]string, len(groups))
	for i, g := range groups {
		groupNames[i], groupKeys[i] = g.name, g.key
	}
	sizes := map[group]int{}
	rows, err := tx.Query(ctx, v.sql(`
SELECT {name}, dim_key, count(*) FROM {dims}
WHERE tenant_id = $1 AND ({name}, dim_key) IN (SELECT * FROM unnest($2::text[], $3::text[]))
GROUP BY 1, 2`), tenantID, groupNames, groupKeys)
	if err != nil {
		return DimAdmission{}, fmt.Errorf("storage: count %s dimension values: %w", v.noun, err)
	}
	for rows.Next() {
		var g group
		var n int
		if err := rows.Scan(&g.name, &g.key, &n); err != nil {
			rows.Close()
			return DimAdmission{}, fmt.Errorf("storage: scan %s dimension values: %w", v.noun, err)
		}
		sizes[g] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return DimAdmission{}, fmt.Errorf("storage: count %s dimension values: %w", v.noun, err)
	}

	var add []DimValueKey
	for _, g := range groups {
		admitted, folded := planVocabulary(values[g], knownValues[g], valueCounts[g], limit-sizes[g])
		for _, value := range admitted {
			k := DimValueKey{g.name, g.key, value}
			adm.Values[k] = true
			add = append(add, k)
		}
		adm.Folded += folded
	}
	if len(add) > 0 {
		names, dimKeys, dimValues := splitDimKeys(add)
		if _, err := tx.Exec(ctx, v.sql(`
INSERT INTO {dims} (tenant_id, {name}, dim_key, dim_value, first_seen, last_seen)
SELECT $1, n, k, v, $5, $5 FROM unnest($2::text[], $3::text[], $4::text[]) AS t(n, k, v)
ON CONFLICT (tenant_id, {name}, dim_key, dim_value) DO NOTHING`), tenantID, names, dimKeys, dimValues, at); err != nil {
			return DimAdmission{}, fmt.Errorf("storage: insert %s dimension values: %w", v.noun, err)
		}
	}
	if err := touchDims(ctx, v, tx, tenantID, keys, at, staleBefore); err != nil {
		return DimAdmission{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DimAdmission{}, fmt.Errorf("storage: admit %s dimensions: %w", v.noun, err)
	}
	return adm, nil
}

func knownDims(ctx context.Context, v vocabulary, q querier, tenantID uuid.UUID, keys []DimValueKey, staleBefore time.Time) (map[DimValueKey]bool, bool, error) {
	names, dimKeys, dimValues := splitDimKeys(keys)
	rows, err := q.Query(ctx, v.sql(`
SELECT {name}, dim_key, dim_value, last_seen FROM {dims}
WHERE tenant_id = $1 AND ({name}, dim_key, dim_value) IN (SELECT * FROM unnest($2::text[], $3::text[], $4::text[]))`),
		tenantID, names, dimKeys, dimValues)
	if err != nil {
		return nil, false, fmt.Errorf("storage: read %s dimension values: %w", v.noun, err)
	}
	defer rows.Close()
	known := map[DimValueKey]bool{}
	stale := false
	for rows.Next() {
		var k DimValueKey
		var lastSeen time.Time
		if err := rows.Scan(&k.Name, &k.Key, &k.Value, &lastSeen); err != nil {
			return nil, false, fmt.Errorf("storage: scan %s dimension values: %w", v.noun, err)
		}
		known[k] = true
		stale = stale || lastSeen.Before(staleBefore)
	}
	return known, stale, rows.Err()
}

func touchDims(ctx context.Context, v vocabulary, e execer, tenantID uuid.UUID, keys []DimValueKey, at, staleBefore time.Time) error {
	names, dimKeys, dimValues := splitDimKeys(keys)
	if _, err := e.Exec(ctx, v.sql(`
UPDATE {dims} SET last_seen = $5
WHERE tenant_id = $1 AND ({name}, dim_key, dim_value) IN (SELECT * FROM unnest($2::text[], $3::text[], $4::text[])) AND last_seen < $6`),
		tenantID, names, dimKeys, dimValues, at, staleBefore); err != nil {
		return fmt.Errorf("storage: touch %s dimension values: %w", v.noun, err)
	}
	return nil
}

func splitDimKeys(keys []DimValueKey) (names, dimKeys, values []string) {
	names = make([]string, len(keys))
	dimKeys = make([]string, len(keys))
	values = make([]string, len(keys))
	for i, k := range keys {
		names[i], dimKeys[i], values[i] = k.Name, k.Key, k.Value
	}
	return names, dimKeys, values
}
