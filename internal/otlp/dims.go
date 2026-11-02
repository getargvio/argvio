package otlp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/getargvio/argvio/internal/storage"
)

// attrDimPrefix starts every dimension attribute of a vendor counter or
// measure (allowlist family cli.dim.*).
const attrDimPrefix = "cli.dim."

// Defaults standing in for unset Bounds caps, as for counters.
const (
	defaultMaxMetricDims      = 3
	defaultMaxDimValuesPerKey = 50
)

// vendorLabelAttr is the label attribute of the vendor metrics that take
// dimensions, "" for any other metric.
func vendorLabelAttr(metric string) string {
	switch metric {
	case metricCounter:
		return attrCounterLabel
	case metricMeasure:
		return attrMeasureLabel
	}
	return ""
}

// foldDims turns the cli.dim.<key> attributes of a cli.counter or cli.measure
// data point into its label: the canonical tuple of storage.FormatDims, with
// a plain label read as the dimension storage.DimLabelKey. kept is the
// point's attributes after allowlist evaluation, and dropped the keys that
// evaluation removed. It returns why the point must be rejected, or "": a
// dimension that failed validation, more than the allowed number, or a
// cli.dim.label next to a label would otherwise be stored without it, and a
// point that silently lost a dimension would merge into another series.
// kept is rewritten in place; a point without dimensions is left as is.
func (p *Pipeline) foldDims(metric string, kept map[string]any, dropped []string) string {
	labelAttr := vendorLabelAttr(metric)
	if labelAttr == "" {
		return ""
	}
	for _, k := range dropped {
		if strings.HasPrefix(k, attrDimPrefix) {
			return fmt.Sprintf("dimension %s is invalid", k)
		}
	}
	dims := map[string]string{}
	for k, v := range kept {
		if key, ok := strings.CutPrefix(k, attrDimPrefix); ok {
			s, _ := v.(string)
			dims[key] = s
		}
	}
	if len(dims) == 0 {
		return ""
	}
	if label, ok := kept[labelAttr].(string); ok && label != "" {
		if _, clash := dims[storage.DimLabelKey]; clash {
			return fmt.Sprintf("%s%s and %s are the same dimension", attrDimPrefix, storage.DimLabelKey, labelAttr)
		}
		dims[storage.DimLabelKey] = label
	}
	limit := p.Bounds.MaxMetricDims
	if limit <= 0 {
		limit = defaultMaxMetricDims
	}
	if len(dims) > limit {
		return fmt.Sprintf("%d dimensions exceed max_metric_dims %d", len(dims), limit)
	}
	for k := range kept {
		if strings.HasPrefix(k, attrDimPrefix) {
			delete(kept, k)
		}
	}
	kept[labelAttr] = storage.FormatDims(dims)
	return ""
}

// capDimValues stores as "other" every dimension value of in.metric's rows
// that its (name, key) has no room for, before capVendorInstrument caps the
// resulting tuples. Plain labels are left to the label cap: they are a
// single dimension whose values that cap already bounds.
func (p *Pipeline) capDimValues(ctx context.Context, rows []storage.MetricRow, in vendorInstrument) error {
	counts := map[storage.DimValueKey]int64{}
	for _, r := range rows {
		if r.MetricName != in.metric {
			continue
		}
		k := vendorKey(r.DatapointAttributes, in)
		if !isTuple(k.Label) {
			continue
		}
		for key, value := range storage.ParseDims(k.Label) {
			counts[storage.DimValueKey{Name: k.Name, Key: key, Value: value}]++
		}
	}
	if len(counts) == 0 {
		return nil
	}
	limit := p.Bounds.MaxDimValuesPerKey
	if limit <= 0 {
		limit = defaultMaxDimValuesPerKey
	}
	adm, err := in.admitDims(ctx, p.tenantUUID(), counts, limit, p.now())
	if err != nil {
		return err
	}
	if adm.Folded == 0 {
		return nil
	}
	var foldedPoints int64
	for _, r := range rows {
		if r.MetricName != in.metric {
			continue
		}
		k := vendorKey(r.DatapointAttributes, in)
		if !isTuple(k.Label) {
			continue
		}
		dims := storage.ParseDims(k.Label)
		folded := false
		for key, value := range dims {
			if !adm.Values[storage.DimValueKey{Name: k.Name, Key: key, Value: value}] {
				dims[key] = storage.CounterOther
				folded = true
			}
		}
		if folded {
			r.DatapointAttributes[in.labelAttr] = storage.FormatDims(dims)
			foldedPoints++
		}
	}
	p.Log.Warn("otlp: "+in.metric+" dimension values stored as \"other\": tenant reached its cap",
		"tenant_id", p.tenantUUID(), "folded_points", foldedPoints, "max_dim_values_per_key", limit)
	return in.recordFoldedDims(ctx, p.tenantUUID(), foldedPoints, p.now())
}

// isTuple reports whether a stored label is a dimension tuple rather than a
// plain label.
func isTuple(label string) bool {
	return strings.Contains(label, "=")
}

// dimAdmitters are the storage calls behind capDimValues for one instrument.
type dimAdmitters struct {
	admitDims        func(ctx context.Context, tenantID uuid.UUID, counts map[storage.DimValueKey]int64, limit int, at time.Time) (storage.DimAdmission, error)
	recordFoldedDims func(ctx context.Context, tenantID uuid.UUID, n int64, at time.Time) error
}
