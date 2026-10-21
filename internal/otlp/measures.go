package otlp

import (
	"context"

	"github.com/getargvio/argvio/internal/storage"
)

// The vendor-defined duration metric and its attributes (allowlist
// cli.measure). Like counters, their format is enforced by the schema's
// pattern and their cardinality per tenant here.
const (
	metricMeasure    = "cli.measure"
	attrMeasureName  = "cli.measure.name"
	attrMeasureLabel = "cli.measure.label"
)

// Defaults standing in for unset Bounds caps, as for counters.
const (
	defaultMaxMeasureNames  = 100
	defaultMaxMeasureLabels = 50
)

// capMeasures is capCounters for cli.measure rows: names and labels past the
// tenant's caps are stored as storage.MeasureOther.
func (p *Pipeline) capMeasures(ctx context.Context, rows []storage.MetricRow) error {
	nameLimit := p.Bounds.MaxMeasureNamesPerTenant
	if nameLimit <= 0 {
		nameLimit = defaultMaxMeasureNames
	}
	labelLimit := p.Bounds.MaxMeasureLabelsPerName
	if labelLimit <= 0 {
		labelLimit = defaultMaxMeasureLabels
	}
	return p.capVendorInstrument(ctx, rows, vendorInstrument{
		metric: metricMeasure, nameAttr: attrMeasureName, labelAttr: attrMeasureLabel,
		nameLimit: nameLimit, labelLimit: labelLimit,
		nameLimitKey: "max_measure_names_per_tenant", labelLimitKey: "max_measure_labels_per_name",
		admit: p.Writer.AdmitMeasures,
	})
}
