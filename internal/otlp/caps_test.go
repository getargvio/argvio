package otlp

import (
	"testing"

	"github.com/getargvio/argvio/internal/tenant"
)

func TestMetricCapsResolution(t *testing.T) {
	names, dims := 7, 5
	p := &Pipeline{
		Bounds: Bounds{MaxCounterNamesPerTenant: 200, MaxCounterLabelsPerName: 50, MaxMetricDims: 3},
		Tenant: &tenant.Resolved{Config: tenant.Config{MetricCaps: tenant.MetricCaps{MaxCounterNames: &names, MaxMetricDims: &dims}}},
	}
	got := p.metricCaps()
	want := metricCaps{
		counterNames: 7, counterLabels: 50, // override, then global
		measureNames: defaultMaxMeasureNames, measureLabels: defaultMaxMeasureLabels, // neither: the default
		dims: 5, dimValues: defaultMaxDimValuesPerKey,
	}
	if got != want {
		t.Errorf("metricCaps() = %+v, want %+v", got, want)
	}
}
