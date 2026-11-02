package storage

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ValueBounds are the explicit histogram bucket boundaries of cli.value, so
// len(ValueBounds)+1 buckets: the first is (-inf, 0], counting the runs that
// recorded nothing, the last (100000, +inf). They must equal explicit_bounds
// in schema/allowlist/v1.yaml, and the bkt_0..bkt_14 columns of
// cagg_values_* (migration 0021) are one per bucket.
var ValueBounds = []float64{0, 1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 5000, 10000, 100000}

// ValueBucketCount is how many buckets ValueBounds makes.
const ValueBucketCount = 15

// Value units cli.value.unit may hold.
var ValueUnits = []string{"1", "By", "%"}

var valueVocabulary = vocabulary{
	noun: "value", names: "value_names", labels: "value_labels", overflow: "value_overflow", dims: "value_dim_values", nameColumn: "value_name",
}

// AdmitValues is AdmitCounters for cli.value names and labels (migration
// 0021).
func (w *Writer) AdmitValues(ctx context.Context, tenantID uuid.UUID, counts map[CounterKey]int64, nameLimit, labelLimit int, at time.Time) (CounterAdmission, error) {
	return w.admit(ctx, valueVocabulary, tenantID, counts, nameLimit, labelLimit, at)
}

// AdmitValueDims is AdmitCounterDims for values.
func (w *Writer) AdmitValueDims(ctx context.Context, tenantID uuid.UUID, counts map[DimValueKey]int64, limit int, at time.Time) (DimAdmission, error) {
	return w.admitDims(ctx, valueVocabulary, tenantID, counts, limit, at)
}

// RecordFoldedValueDims is RecordFoldedCounterDims for values.
func (w *Writer) RecordFoldedValueDims(ctx context.Context, tenantID uuid.UUID, n int64, at time.Time) error {
	return w.recordFoldedDims(ctx, valueVocabulary, tenantID, n, at)
}
