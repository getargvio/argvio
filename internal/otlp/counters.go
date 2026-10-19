package otlp

import (
	"context"

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

// capCounters rewrites the counter name or label of every cli.counter row
// that its tenant's vocabulary has no room for to storage.CounterOther, before
// the rows are stored: a folded name loses its label too, a folded label
// keeps its name. A name sent as "other" literally also loses its label, so
// labels under it can't grow without bound. Like capErrorReasons, an error
// fails the export before anything is stored.
func (p *Pipeline) capCounters(ctx context.Context, rows []storage.MetricRow) error {
	counts := map[storage.CounterKey]int64{}
	for _, r := range rows {
		if r.MetricName != metricCounter {
			continue
		}
		counts[counterKey(r.DatapointAttributes)]++
	}
	if len(counts) == 0 {
		return nil
	}
	nameLimit := p.Bounds.MaxCounterNamesPerTenant
	if nameLimit <= 0 {
		nameLimit = defaultMaxCounterNames
	}
	labelLimit := p.Bounds.MaxCounterLabelsPerName
	if labelLimit <= 0 {
		labelLimit = defaultMaxCounterLabels
	}
	adm, err := p.Writer.AdmitCounters(ctx, p.tenantUUID(), counts, nameLimit, labelLimit, p.now())
	if err != nil {
		return err
	}
	if adm.FoldedNames > 0 || adm.FoldedLabels > 0 {
		p.Log.Warn("otlp: counter names or labels stored as \"other\": tenant reached its counter caps",
			"tenant_id", p.tenantUUID(), "folded_names", adm.FoldedNames, "folded_labels", adm.FoldedLabels,
			"max_counter_names_per_tenant", nameLimit, "max_counter_labels_per_name", labelLimit)
	}
	for _, r := range rows {
		if r.MetricName != metricCounter {
			continue
		}
		a := r.DatapointAttributes
		k := counterKey(a)
		switch {
		case !adm.Names[k.Name]:
			a[attrCounterName] = storage.CounterOther
			delete(a, attrCounterLabel)
		case k.Name == storage.CounterOther:
			delete(a, attrCounterLabel)
		case k.Label != "" && k.Label != storage.CounterOther && !adm.Labels[k]:
			a[attrCounterLabel] = storage.CounterOther
		}
	}
	return nil
}

// counterKey reads a cli.counter row's name and label. The name is always
// present: the allowlist requires it.
func counterKey(a storage.Attrs) storage.CounterKey {
	name, _ := a[attrCounterName].(string)
	label, _ := a[attrCounterLabel].(string)
	return storage.CounterKey{Name: name, Label: label}
}
