package otlp

import (
	"context"

	"github.com/getargvio/argvio/internal/storage"
)

// The vendor-defined per-run value metric and its attributes (allowlist
// cli.value). Format is enforced by the schema, cardinality per tenant here,
// as for counters and measures.
const (
	metricValue    = "cli.value"
	attrValueName  = "cli.value.name"
	attrValueLabel = "cli.value.label"
)

// Defaults standing in for unset Bounds caps, as for counters.
const (
	defaultMaxValueNames  = 100
	defaultMaxValueLabels = 50
)

// capValues is capCounters for cli.value rows.
func (p *Pipeline) capValues(ctx context.Context, rows []storage.MetricRow) error {
	caps := p.metricCaps()
	return p.capVendorInstrument(ctx, rows, vendorInstrument{
		metric: metricValue, nameAttr: attrValueName, labelAttr: attrValueLabel,
		nameLimit: caps.valueNames, labelLimit: caps.valueLabels,
		nameLimitKey: "max_value_names_per_tenant", labelLimitKey: "max_value_labels_per_name",
		admit:        p.Writer.AdmitValues,
		dimAdmitters: dimAdmitters{p.Writer.AdmitValueDims, p.Writer.RecordFoldedValueDims},
	})
}
