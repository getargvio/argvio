package storage

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// MeasureOther is what a cli.measure.name or cli.measure.label is stored as
// once its vocabulary is full, like CounterOther.
const MeasureOther = "other"

// MeasureKey, MeasureAdmission: a measure's (name, label) vocabulary works
// exactly like a counter's.
type (
	MeasureKey       = CounterKey
	MeasureAdmission = CounterAdmission
)

var measureVocabulary = vocabulary{
	noun: "measure", names: "measure_names", labels: "measure_labels", overflow: "measure_overflow", nameColumn: "measure_name",
}

// AdmitMeasures is AdmitCounters for cli.measure names and labels, against
// measure_names, measure_labels and measure_overflow (migration 0014).
func (w *Writer) AdmitMeasures(ctx context.Context, tenantID uuid.UUID, counts map[MeasureKey]int64, nameLimit, labelLimit int, at time.Time) (MeasureAdmission, error) {
	return w.admit(ctx, measureVocabulary, tenantID, counts, nameLimit, labelLimit, at)
}
