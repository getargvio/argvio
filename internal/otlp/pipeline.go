package otlp

import (
	"log/slog"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/collector/pdata/pcommon"

	"github.com/getargvio/argvio/internal/allowlist"
	"github.com/getargvio/argvio/internal/consent"
	"github.com/getargvio/argvio/internal/storage"
	"github.com/getargvio/argvio/internal/tenant"
)

// Pipeline is the shared per-request context for all three signal
// converters (metrics.go/logs.go/traces.go): the active allowlist schema,
// the resolved tenant, structural bounds, and where finished rows go.
type Pipeline struct {
	Schema *allowlist.Schema
	Tenant *tenant.Resolved
	Bounds Bounds
	Writer *storage.Writer
	Log    *slog.Logger
	Now    func() time.Time // overridable for tests; defaults to time.Now
}

func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// recordOutcome is the result of running one span/log record/data point
// through resolveAndEvaluate: either a consent.Decision to build a storage
// row from, or a reason it was rejected.
type recordOutcome struct {
	decision consent.Decision
	rejected bool
	reason   string
}

// resolveAndEvaluate runs the full per-record pipeline shared by all three
// signals: extract resource+record attributes (Layer 1 bounds), resolve the
// declared consent tier, look up the event on the allowlist, and evaluate
// tier enforcement + Layer 2 filtering (internal/consent.EvaluateEvent).
func (p *Pipeline) resolveAndEvaluate(
	resourceAttrsRaw pcommon.Map,
	recordAttrsRaw pcommon.Map,
	def consent.EventDef,
) recordOutcome {
	b := p.Bounds
	resourceAttrs, resourceDropped, resourceTooMany := extractAttrs(resourceAttrsRaw, b.MaxAttributeCount, b.MaxAttributeKeyLength, b.MaxAttributeStringValueLength)
	if resourceTooMany {
		return recordOutcome{rejected: true, reason: "resource attribute count exceeds max_attribute_count"}
	}
	recordAttrs, recordDropped, recordTooMany := extractAttrs(recordAttrsRaw, b.MaxAttributeCount, b.MaxAttributeKeyLength, b.MaxAttributeStringValueLength)
	if recordTooMany {
		return recordOutcome{rejected: true, reason: "record attribute count exceeds max_attribute_count"}
	}
	p.logDropped(resourceDropped, recordDropped)

	tier, err := consent.ResolveTier(p.Schema, resourceAttrs, recordAttrs)
	if err != nil {
		return recordOutcome{rejected: true, reason: err.Error()}
	}

	merged := mergeAttrs(resourceAttrs, recordAttrs)
	decision := consent.EvaluateEvent(p.Schema, def, tier, p.Tenant.Config.TierCeiling, p.Tenant.Config.TierEnforcementMode, merged)
	if !decision.Accepted {
		return recordOutcome{rejected: true, reason: decision.RejectReason}
	}
	if len(decision.DroppedKeys) > 0 {
		p.Log.Info("otlp: attributes stripped by consent-tier enforcement",
			"tenant_id", p.Tenant.Tenant.ID, "dropped_keys", decision.DroppedKeys)
	}
	return recordOutcome{decision: decision}
}

func (p *Pipeline) logDropped(resourceDropped, recordDropped []string) {
	if len(resourceDropped) == 0 && len(recordDropped) == 0 {
		return
	}
	p.Log.Warn("otlp: attributes dropped by Layer 1 bounds",
		"tenant_id", p.Tenant.Tenant.ID,
		"resource_dropped_keys", resourceDropped,
		"record_dropped_keys", recordDropped,
	)
}

// promoted holds the columns every signal table promotes out of JSONB.
// popPromoted extracts and removes them from kept so the remainder can go
// straight into the record-level JSONB column without duplication.
type promoted struct {
	CLIVersion  *string
	OS          *string
	Arch        *string
	CommandName *string
	ExitCode    *int32
	IsCI        *bool
}

func popPromoted(kept map[string]any) promoted {
	return promoted{
		CLIVersion:  popStr(kept, "cli.version"),
		OS:          popStr(kept, "cli.os"),
		Arch:        popStr(kept, "cli.arch"),
		CommandName: popStr(kept, "cli.command.name"),
		ExitCode:    popInt32(kept, "cli.command.exit_code"),
		IsCI:        popBool(kept, "cli.is_ci"),
	}
}

// splitRemainingAttrs partitions whatever's left in kept (after
// popPromoted) into the resource_attributes JSONB bucket (standard OTel SDK
// resource attributes) and the record-level JSONB bucket (everything else
// — allowlisted cli.* attributes that don't have their own column).
func (p *Pipeline) splitRemainingAttrs(kept map[string]any) (resourceJSON, recordJSON storage.Attrs) {
	resourceJSON = storage.Attrs{}
	recordJSON = storage.Attrs{}
	for k, v := range kept {
		if p.Schema.IsStandardResourceAttribute(k) {
			resourceJSON[k] = v
		} else {
			recordJSON[k] = v
		}
	}
	return resourceJSON, recordJSON
}

func popStr(m map[string]any, k string) *string {
	if v, ok := m[k]; ok {
		if s, ok2 := v.(string); ok2 {
			delete(m, k)
			return &s
		}
	}
	return nil
}

func popInt32(m map[string]any, k string) *int32 {
	if v, ok := m[k]; ok {
		if n, ok2 := v.(int64); ok2 {
			delete(m, k)
			i := clampInt32(n)
			return &i
		}
	}
	return nil
}

func popBool(m map[string]any, k string) *bool {
	if v, ok := m[k]; ok {
		if b, ok2 := v.(bool); ok2 {
			delete(m, k)
			return &b
		}
	}
	return nil
}

// tenantUUID is a tiny convenience so signal converters don't repeat
// p.Tenant.Tenant.ID everywhere.
func (p *Pipeline) tenantUUID() uuid.UUID {
	return p.Tenant.Tenant.ID
}
