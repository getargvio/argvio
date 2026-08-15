package otlp

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/getargvio/argvio/internal/consent"
	"github.com/getargvio/argvio/internal/storage"
)

// ProcessTraces runs every span in td through the full Layer 1 + Layer 2 +
// consent-tier pipeline and writes whatever's left to Postgres in one
// batch. accepted/rejected are span counts; rejectionSample is one example
// rejection reason (for the OTLP PartialSuccess error_message — the full
// detail for every rejection goes to the structured log, not the response,
// so we never echo attribute values back over the wire).
func (p *Pipeline) ProcessTraces(ctx context.Context, td ptrace.Traces) (accepted, rejected int64, rejectionSample string, err error) {
	if !p.Schema.IsKnownSignalType("traces") {
		return 0, 0, "", ErrUnknownSignalType
	}
	if td.SpanCount() > p.Bounds.MaxBatchSize {
		return 0, 0, "", fmt.Errorf("%w: %d spans exceeds max_batch_size %d", ErrBatchTooLarge, td.SpanCount(), p.Bounds.MaxBatchSize)
	}

	now := p.now()
	var rows []storage.TraceRow

	rs := td.ResourceSpans()
	for i := 0; i < rs.Len(); i++ {
		resSpans := rs.At(i)
		resource := resSpans.Resource()
		ss := resSpans.ScopeSpans()
		for j := 0; j < ss.Len(); j++ {
			scopeSpans := ss.At(j)
			scope := scopeSpans.Scope()
			spans := scopeSpans.Spans()
			for k := 0; k < spans.Len(); k++ {
				span := spans.At(k)

				if !checkTimestamp(span.StartTimestamp().AsTime(), now, p.Bounds.MaxTimestampSkewPast, p.Bounds.MaxTimestampSkewFuture) {
					rejected++
					rejectionSample = "span start_timestamp outside allowed skew"
					p.Log.Warn("otlp: span rejected", "reason", rejectionSample, "span_name", span.Name())
					continue
				}

				spanDef, found := p.Schema.MatchingSpanDef(span.Name())
				def := consent.EventDef{Found: found, AllowedAttributes: spanDef.AllowedAttributes}
				outcome := p.resolveAndEvaluate(resource.Attributes(), span.Attributes(), def)
				if outcome.rejected {
					rejected++
					rejectionSample = outcome.reason
					p.Log.Warn("otlp: span rejected", "reason", outcome.reason, "span_name", span.Name(), "tenant_id", p.tenantUUID())
					continue
				}

				rows = append(rows, p.buildTraceRow(span, scope, outcome.decision, now))
				accepted++
			}
		}
	}

	if len(rows) > 0 {
		if _, werr := p.Writer.InsertTraces(ctx, rows); werr != nil {
			return 0, int64(td.SpanCount()), "", werr
		}
	}
	return accepted, rejected, rejectionSample, nil
}

func (p *Pipeline) buildTraceRow(span ptrace.Span, scope pcommon.InstrumentationScope, decision consent.Decision, now time.Time) storage.TraceRow {
	kept := decision.KeptAttributes
	promo := popPromoted(kept)
	resourceJSON, spanJSON := p.splitRemainingAttrs(kept)

	var parentSpanID *string
	if psid := span.ParentSpanID(); !psid.IsEmpty() {
		s := psid.String()
		parentSpanID = &s
	}
	var statusMessage *string
	if msg := span.Status().Message(); msg != "" {
		statusMessage = &msg
	}
	var scopeName, scopeVersion *string
	if scope.Name() != "" {
		s := scope.Name()
		scopeName = &s
	}
	if scope.Version() != "" {
		s := scope.Version()
		scopeVersion = &s
	}

	start := span.StartTimestamp().AsTime()
	end := span.EndTimestamp().AsTime()

	return storage.TraceRow{
		Time:               start,
		TenantID:           p.tenantUUID(),
		TraceID:            span.TraceID().String(),
		SpanID:             span.SpanID().String(),
		ParentSpanID:       parentSpanID,
		SpanName:           span.Name(),
		SpanKind:           clampInt16(int32(span.Kind())),
		StartTime:          start,
		EndTime:            end,
		DurationMs:         float64(end.Sub(start).Microseconds()) / 1000.0,
		StatusCode:         clampInt16(int32(span.Status().Code())),
		StatusMessage:      statusMessage,
		CLIVersion:         promo.CLIVersion,
		OS:                 promo.OS,
		Arch:               promo.Arch,
		CommandName:        promo.CommandName,
		ExitCode:           promo.ExitCode,
		IsCI:               promo.IsCI,
		ConsentTier:        decision.EffectiveTier,
		ScopeName:          scopeName,
		ScopeVersion:       scopeVersion,
		ResourceAttributes: resourceJSON,
		SpanAttributes:     spanJSON,
	}
}
