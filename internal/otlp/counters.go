package otlp

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/getargvio/argvio/internal/storage"
)

// The vendor-defined counter metric and its attributes (allowlist
// cli.counter). Their format is enforced by the schema's pattern; their
// cardinality per tenant is enforced here.
const (
	metricCounter    = "cli.counter"
	attrCounterName  = "cli.counter.name"
	attrCounterLabel = "cli.counter.label"
)

// Defaults standing in for unset Bounds caps (config validation requires
// them, but a zero Bounds must not fold everything).
const (
	defaultMaxCounterNames  = 200
	defaultMaxCounterLabels = 50
)

// vendorInstrument is one generic vendor metric whose name and label
// attributes are capped per tenant: cli.counter, cli.measure.
type vendorInstrument struct {
	metric, nameAttr, labelAttr string
	nameLimit, labelLimit       int
	// Config keys of the two caps, for the log line when they fold.
	nameLimitKey, labelLimitKey string
	admit                       func(ctx context.Context, tenantID uuid.UUID, counts map[storage.CounterKey]int64, nameLimit, labelLimit int, at time.Time) (storage.CounterAdmission, error)
}

// capCounters rewrites the counter name or label of every cli.counter row
// that its tenant's vocabulary has no room for to storage.CounterOther, before
// the rows are stored: a folded name loses its label too, a folded label
// keeps its name. A name sent as "other" literally also loses its label, so
// labels under it can't grow without bound. Like capErrorReasons, an error
// fails the export before anything is stored.
func (p *Pipeline) capCounters(ctx context.Context, rows []storage.MetricRow) error {
	nameLimit := p.Bounds.MaxCounterNamesPerTenant
	if nameLimit <= 0 {
		nameLimit = defaultMaxCounterNames
	}
	labelLimit := p.Bounds.MaxCounterLabelsPerName
	if labelLimit <= 0 {
		labelLimit = defaultMaxCounterLabels
	}
	return p.capVendorInstrument(ctx, rows, vendorInstrument{
		metric: metricCounter, nameAttr: attrCounterName, labelAttr: attrCounterLabel,
		nameLimit: nameLimit, labelLimit: labelLimit,
		nameLimitKey: "max_counter_names_per_tenant", labelLimitKey: "max_counter_labels_per_name",
		admit: p.Writer.AdmitCounters,
	})
}

// capVendorInstrument is the cap shared by the vendor instruments, applied to
// the rows of in.metric.
func (p *Pipeline) capVendorInstrument(ctx context.Context, rows []storage.MetricRow, in vendorInstrument) error {
	counts := map[storage.CounterKey]int64{}
	for _, r := range rows {
		if r.MetricName != in.metric {
			continue
		}
		counts[vendorKey(r.DatapointAttributes, in)]++
	}
	if len(counts) == 0 {
		return nil
	}
	adm, err := in.admit(ctx, p.tenantUUID(), counts, in.nameLimit, in.labelLimit, p.now())
	if err != nil {
		return err
	}
	if adm.FoldedNames > 0 || adm.FoldedLabels > 0 {
		p.Log.Warn("otlp: "+in.metric+" names or labels stored as \"other\": tenant reached its caps",
			"tenant_id", p.tenantUUID(), "folded_names", adm.FoldedNames, "folded_labels", adm.FoldedLabels,
			in.nameLimitKey, in.nameLimit, in.labelLimitKey, in.labelLimit)
	}
	for _, r := range rows {
		if r.MetricName != in.metric {
			continue
		}
		a := r.DatapointAttributes
		k := vendorKey(a, in)
		switch {
		case !adm.Names[k.Name]:
			a[in.nameAttr] = storage.CounterOther
			delete(a, in.labelAttr)
		case k.Name == storage.CounterOther:
			delete(a, in.labelAttr)
		case k.Label != "" && k.Label != storage.CounterOther && !adm.Labels[k]:
			a[in.labelAttr] = storage.CounterOther
		}
	}
	return nil
}

// vendorKey reads a vendor row's name and label. The name is always present:
// the allowlist requires it.
func vendorKey(a storage.Attrs, in vendorInstrument) storage.CounterKey {
	name, _ := a[in.nameAttr].(string)
	label, _ := a[in.labelAttr].(string)
	return storage.CounterKey{Name: name, Label: label}
}
