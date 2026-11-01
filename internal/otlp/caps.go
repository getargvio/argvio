package otlp

// metricCaps are the vendor-metric caps in force for the pipeline's tenant.
type metricCaps struct {
	counterNames, counterLabels int
	measureNames, measureLabels int
	valueNames, valueLabels     int
	dims, dimValues             int
}

// metricCaps resolves each cap from the tenant's override
// (tenant_config.max_*), else the public server's config (Bounds), else the
// built-in default standing in for an unset Bounds.
func (p *Pipeline) metricCaps() metricCaps {
	o := p.Tenant.Config.MetricCaps
	return metricCaps{
		counterNames:  pickCap(o.MaxCounterNames, p.Bounds.MaxCounterNamesPerTenant, defaultMaxCounterNames),
		counterLabels: pickCap(o.MaxCounterLabelsPerName, p.Bounds.MaxCounterLabelsPerName, defaultMaxCounterLabels),
		measureNames:  pickCap(o.MaxMeasureNames, p.Bounds.MaxMeasureNamesPerTenant, defaultMaxMeasureNames),
		measureLabels: pickCap(o.MaxMeasureLabelsPerName, p.Bounds.MaxMeasureLabelsPerName, defaultMaxMeasureLabels),
		valueNames:    pickCap(o.MaxValueNames, p.Bounds.MaxValueNamesPerTenant, defaultMaxValueNames),
		valueLabels:   pickCap(o.MaxValueLabelsPerName, p.Bounds.MaxValueLabelsPerName, defaultMaxValueLabels),
		dims:          pickCap(o.MaxMetricDims, p.Bounds.MaxMetricDims, defaultMaxMetricDims),
		dimValues:     pickCap(o.MaxDimValuesPerKey, p.Bounds.MaxDimValuesPerKey, defaultMaxDimValuesPerKey),
	}
}

func pickCap(override *int, global, def int) int {
	switch {
	case override != nil && *override > 0:
		return *override
	case global > 0:
		return global
	default:
		return def
	}
}
