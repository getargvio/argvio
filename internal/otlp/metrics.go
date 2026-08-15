package otlp

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/getargvio/argvio/internal/consent"
	"github.com/getargvio/argvio/internal/storage"
)

// ProcessMetrics mirrors ProcessTraces for the metrics signal. A metric's
// "name" for allowlist purposes is the Metric itself (not each data point);
// an unsupported OTLP metric type (ExponentialHistogram, Summary) rejects
// every data point under that metric — we don't have a taxonomy definition
// to validate them against, and Layer 1 requires rejecting what isn't
// explicitly supported rather than best-effort storing it.
func (p *Pipeline) ProcessMetrics(ctx context.Context, md pmetric.Metrics) (accepted, rejected int64, rejectionSample string, err error) {
	if !p.Schema.IsKnownSignalType("metrics") {
		return 0, 0, "", ErrUnknownSignalType
	}
	if md.DataPointCount() > p.Bounds.MaxBatchSize {
		return 0, 0, "", fmt.Errorf("%w: %d data points exceeds max_batch_size %d", ErrBatchTooLarge, md.DataPointCount(), p.Bounds.MaxBatchSize)
	}

	now := p.now()
	var rows []storage.MetricRow

	rms := md.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		resMetrics := rms.At(i)
		resource := resMetrics.Resource()
		sms := resMetrics.ScopeMetrics()
		for j := 0; j < sms.Len(); j++ {
			scopeMetrics := sms.At(j)
			scope := scopeMetrics.Scope()
			metrics := scopeMetrics.Metrics()
			for k := 0; k < metrics.Len(); k++ {
				metric := metrics.At(k)

				metricDef, found := p.Schema.MetricByName(metric.Name())
				def := consent.EventDef{Found: found, AllowedAttributes: metricDef.AllowedAttributes}

				switch metric.Type() {
				case pmetric.MetricTypeSum:
					dps := metric.Sum().DataPoints()
					for d := 0; d < dps.Len(); d++ {
						dp := dps.At(d)
						row, ok := p.processNumberDataPoint(resource, scope, metric, "sum", dp, def, now, &rejected, &rejectionSample)
						if ok {
							rows = append(rows, row)
							accepted++
						}
					}
				case pmetric.MetricTypeGauge:
					dps := metric.Gauge().DataPoints()
					for d := 0; d < dps.Len(); d++ {
						dp := dps.At(d)
						row, ok := p.processNumberDataPoint(resource, scope, metric, "gauge", dp, def, now, &rejected, &rejectionSample)
						if ok {
							rows = append(rows, row)
							accepted++
						}
					}
				case pmetric.MetricTypeHistogram:
					dps := metric.Histogram().DataPoints()
					for d := 0; d < dps.Len(); d++ {
						dp := dps.At(d)
						row, ok := p.processHistogramDataPoint(resource, scope, metric, dp, def, now, &rejected, &rejectionSample)
						if ok {
							rows = append(rows, row)
							accepted++
						}
					}
				default:
					n := int64(metricPointCount(metric))
					rejected += n
					rejectionSample = fmt.Sprintf("unsupported metric type %s", metric.Type())
					p.Log.Warn("otlp: metric rejected", "reason", rejectionSample, "metric_name", metric.Name())
				}
			}
		}
	}

	if len(rows) > 0 {
		if _, werr := p.Writer.InsertMetrics(ctx, rows); werr != nil {
			return 0, int64(md.DataPointCount()), "", werr
		}
	}
	return accepted, rejected, rejectionSample, nil
}

func metricPointCount(m pmetric.Metric) int {
	switch m.Type() {
	case pmetric.MetricTypeExponentialHistogram:
		return m.ExponentialHistogram().DataPoints().Len()
	case pmetric.MetricTypeSummary:
		return m.Summary().DataPoints().Len()
	default:
		return 0
	}
}

func (p *Pipeline) processNumberDataPoint(
	resource pcommon.Resource,
	scope pcommon.InstrumentationScope,
	metric pmetric.Metric,
	metricType string,
	dp pmetric.NumberDataPoint,
	def consent.EventDef,
	now time.Time,
	rejected *int64,
	rejectionSample *string,
) (storage.MetricRow, bool) {
	if !checkTimestamp(dp.Timestamp().AsTime(), now, p.Bounds.MaxTimestampSkewPast, p.Bounds.MaxTimestampSkewFuture) {
		*rejected++
		*rejectionSample = "metric data point timestamp outside allowed skew"
		p.Log.Warn("otlp: metric data point rejected", "reason", *rejectionSample, "metric_name", metric.Name())
		return storage.MetricRow{}, false
	}

	outcome := p.resolveAndEvaluate(resource.Attributes(), dp.Attributes(), def)
	if outcome.rejected {
		*rejected++
		*rejectionSample = outcome.reason
		p.Log.Warn("otlp: metric data point rejected", "reason", outcome.reason, "metric_name", metric.Name())
		return storage.MetricRow{}, false
	}

	kept := outcome.decision.KeptAttributes
	promo := popPromoted(kept)
	resourceJSON, dpJSON := p.splitRemainingAttrs(kept)

	var unit *string
	if metric.Unit() != "" {
		u := metric.Unit()
		unit = &u
	}
	var value float64
	if dp.ValueType() == pmetric.NumberDataPointValueTypeInt {
		value = float64(dp.IntValue())
	} else {
		value = dp.DoubleValue()
	}

	return storage.MetricRow{
		Time:                dp.Timestamp().AsTime(),
		TenantID:            p.tenantUUID(),
		MetricName:          metric.Name(),
		MetricType:          metricType,
		Unit:                unit,
		Value:               &value,
		CLIVersion:          promo.CLIVersion,
		OS:                  promo.OS,
		Arch:                promo.Arch,
		CommandName:         promo.CommandName,
		ExitCode:            promo.ExitCode,
		IsCI:                promo.IsCI,
		ConsentTier:         outcome.decision.EffectiveTier,
		ScopeName:           strPtrOrNil(scope.Name()),
		ScopeVersion:        strPtrOrNil(scope.Version()),
		ResourceAttributes:  resourceJSON,
		DatapointAttributes: dpJSON,
	}, true
}

func (p *Pipeline) processHistogramDataPoint(
	resource pcommon.Resource,
	scope pcommon.InstrumentationScope,
	metric pmetric.Metric,
	dp pmetric.HistogramDataPoint,
	def consent.EventDef,
	now time.Time,
	rejected *int64,
	rejectionSample *string,
) (storage.MetricRow, bool) {
	if !checkTimestamp(dp.Timestamp().AsTime(), now, p.Bounds.MaxTimestampSkewPast, p.Bounds.MaxTimestampSkewFuture) {
		*rejected++
		*rejectionSample = "metric data point timestamp outside allowed skew"
		p.Log.Warn("otlp: histogram data point rejected", "reason", *rejectionSample, "metric_name", metric.Name())
		return storage.MetricRow{}, false
	}

	outcome := p.resolveAndEvaluate(resource.Attributes(), dp.Attributes(), def)
	if outcome.rejected {
		*rejected++
		*rejectionSample = outcome.reason
		p.Log.Warn("otlp: histogram data point rejected", "reason", outcome.reason, "metric_name", metric.Name())
		return storage.MetricRow{}, false
	}

	kept := outcome.decision.KeptAttributes
	promo := popPromoted(kept)
	resourceJSON, dpJSON := p.splitRemainingAttrs(kept)

	var unit *string
	if metric.Unit() != "" {
		u := metric.Unit()
		unit = &u
	}
	count := clampInt64(dp.Count())
	var sum, min, max *float64
	if dp.HasSum() {
		s := dp.Sum()
		sum = &s
	}
	if dp.HasMin() {
		m := dp.Min()
		min = &m
	}
	if dp.HasMax() {
		m := dp.Max()
		max = &m
	}
	bounds := append([]float64(nil), dp.ExplicitBounds().AsRaw()...)
	counts := make([]int64, dp.BucketCounts().Len())
	for i, c := range dp.BucketCounts().AsRaw() {
		counts[i] = clampInt64(c)
	}

	return storage.MetricRow{
		Time:                  dp.Timestamp().AsTime(),
		TenantID:              p.tenantUUID(),
		MetricName:            metric.Name(),
		MetricType:            "histogram",
		Unit:                  unit,
		HistogramCount:        &count,
		HistogramSum:          sum,
		HistogramMin:          min,
		HistogramMax:          max,
		HistogramBucketBounds: bounds,
		HistogramBucketCounts: counts,
		CLIVersion:            promo.CLIVersion,
		OS:                    promo.OS,
		Arch:                  promo.Arch,
		CommandName:           promo.CommandName,
		ExitCode:              promo.ExitCode,
		IsCI:                  promo.IsCI,
		ConsentTier:           outcome.decision.EffectiveTier,
		ScopeName:             strPtrOrNil(scope.Name()),
		ScopeVersion:          strPtrOrNil(scope.Version()),
		ResourceAttributes:    resourceJSON,
		DatapointAttributes:   dpJSON,
	}, true
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
