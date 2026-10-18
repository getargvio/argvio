package otlp

import (
	"context"
	"fmt"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"

	"github.com/getargvio/argvio/internal/consent"
	"github.com/getargvio/argvio/internal/storage"
)

// ProcessLogs mirrors ProcessTraces for the logs signal. A log record's
// name for allowlist purposes is LogRecord.EventName() (the OTLP field
// reserved for exactly this — a stable machine-readable event identifier,
// distinct from the free-text Body). Body is only persisted when the
// record's effective consent tier is "full" or "optin_plus" — below that it
// is nulled out unconditionally, even if the client sent it, since body
// text is exactly the kind of free-form content the lower tiers exist to
// exclude (docs/allowlist.md).
func (p *Pipeline) ProcessLogs(ctx context.Context, ld plog.Logs) (accepted, rejected int64, rejectionSample string, err error) {
	if !p.Schema.IsKnownSignalType("logs") {
		return 0, 0, "", ErrUnknownSignalType
	}
	if ld.LogRecordCount() > p.Bounds.MaxBatchSize {
		return 0, 0, "", fmt.Errorf("%w: %d log records exceeds max_batch_size %d", ErrBatchTooLarge, ld.LogRecordCount(), p.Bounds.MaxBatchSize)
	}

	now := p.now()
	var rows []storage.LogRow

	rls := ld.ResourceLogs()
	for i := 0; i < rls.Len(); i++ {
		resLogs := rls.At(i)
		resource := resLogs.Resource()
		sls := resLogs.ScopeLogs()
		for j := 0; j < sls.Len(); j++ {
			scopeLogs := sls.At(j)
			scope := scopeLogs.Scope()
			records := scopeLogs.LogRecords()
			for k := 0; k < records.Len(); k++ {
				rec := records.At(k)

				if !checkTimestamp(rec.Timestamp().AsTime(), now, p.Bounds.MaxTimestampSkewPast, p.Bounds.MaxTimestampSkewFuture) {
					rejected++
					rejectionSample = "log record timestamp outside allowed skew"
					p.Log.Warn("otlp: log record rejected", "reason", rejectionSample, "event_name", rec.EventName())
					continue
				}

				logDef, found := p.Schema.LogEventByName(rec.EventName())
				def := consent.EventDef{Found: found, AllowedAttributes: logDef.AllowedAttributes}
				outcome := p.resolveAndEvaluate(resource.Attributes(), rec.Attributes(), def)
				if outcome.rejected {
					rejected++
					rejectionSample = outcome.reason
					p.Log.Warn("otlp: log record rejected", "reason", outcome.reason, "event_name", rec.EventName(), "tenant_id", p.tenantUUID())
					continue
				}

				rows = append(rows, p.buildLogRow(rec, scope, outcome.decision))
				accepted++
			}
		}
	}

	if len(rows) > 0 {
		if _, werr := p.Writer.InsertLogs(ctx, rows); werr != nil {
			return 0, int64(ld.LogRecordCount()), "", werr
		}
		p.trackInstalls(ctx, rows)
		p.trackDiscoveries(ctx, rows)
	}
	return accepted, rejected, rejectionSample, nil
}

func (p *Pipeline) buildLogRow(rec plog.LogRecord, scope pcommon.InstrumentationScope, decision consent.Decision) storage.LogRow {
	kept := decision.KeptAttributes
	promo := popPromoted(kept)
	resourceJSON, logJSON := p.splitRemainingAttrs(kept)

	var traceID, spanID *string
	if tid := rec.TraceID(); !tid.IsEmpty() {
		s := tid.String()
		traceID = &s
	}
	if sid := rec.SpanID(); !sid.IsEmpty() {
		s := sid.String()
		spanID = &s
	}

	var severityNumber *int16
	if n := clampInt16(int32(rec.SeverityNumber())); n != 0 {
		severityNumber = &n
	}
	severityText := strPtrOrNil(rec.SeverityText())

	minRank, _ := p.Schema.TierRank(decision.EffectiveTier)
	fullRank, _ := p.Schema.TierRank("full")
	var body *string
	if minRank >= fullRank {
		if s := rec.Body().AsString(); s != "" {
			body = &s
		}
	}

	return storage.LogRow{
		Time:               rec.Timestamp().AsTime(),
		TenantID:           p.tenantUUID(),
		TraceID:            traceID,
		SpanID:             spanID,
		SeverityNumber:     severityNumber,
		SeverityText:       severityText,
		EventName:          rec.EventName(),
		Body:               body,
		CLIVersion:         promo.CLIVersion,
		OS:                 promo.OS,
		Arch:               promo.Arch,
		CommandName:        promo.CommandName,
		ExitCode:           promo.ExitCode,
		IsCI:               promo.IsCI,
		ConsentTier:        decision.EffectiveTier,
		ScopeName:          strPtrOrNil(scope.Name()),
		ScopeVersion:       strPtrOrNil(scope.Version()),
		ResourceAttributes: resourceJSON,
		LogAttributes:      logJSON,
	}
}

// trackInstalls folds the batch's install-bearing records into the installs
// table. The logs are already stored by now, so a failure here is logged and
// not returned: rejecting the export would make the client retry (and
// duplicate) telemetry that was in fact accepted. `argvio installs backfill`
// rebuilds the table from stored logs if it ever falls behind.
func (p *Pipeline) trackInstalls(ctx context.Context, rows []storage.LogRow) {
	var events []storage.InstallEvent
	for _, r := range rows {
		if e, ok := storage.InstallEventFromLog(r); ok {
			events = append(events, e)
		}
	}
	if err := p.Writer.TrackInstalls(ctx, events); err != nil {
		p.Log.Error("otlp: tracking installs failed", "error", err, "tenant_id", p.tenantUUID())
	}
}

// trackDiscoveries records first feature and flag use per install, failing
// soft like trackInstalls; `argvio installs backfill` rebuilds it too.
func (p *Pipeline) trackDiscoveries(ctx context.Context, rows []storage.LogRow) {
	var events []storage.DiscoveryEvent
	for _, r := range rows {
		events = append(events, storage.DiscoveryEventsFromLog(r)...)
	}
	if err := p.Writer.TrackDiscoveries(ctx, events); err != nil {
		p.Log.Error("otlp: tracking discoveries failed", "error", err, "tenant_id", p.tenantUUID())
	}
}
