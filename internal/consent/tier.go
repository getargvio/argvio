// Package consent enforces the four-tier consent model (Anonymous, Basic,
// Full, Opt-in+) against incoming telemetry in the public ingest server.
//
// Two things are enforced here, both driven by internal/allowlist.Schema so
// the tier->field mapping stays data-driven rather than hardcoded:
//
//  1. A per-tenant ceiling: a record cannot claim a tier above what the
//     tenant is configured to send, regardless of what the CLI SDK declares.
//  2. Field stripping: regardless of the declared/effective tier, any
//     attribute whose schema-defined min_tier exceeds the effective tier is
//     dropped rather than trusted-and-stored, even if the client incorrectly
//     included it (e.g. a stack trace sent under an Anonymous declaration).
package consent

import (
	"fmt"

	"github.com/getargvio/argvio/internal/allowlist"
)

// TierAttrKey is the resource/span/log attribute the CLI SDK uses to declare
// the consent tier for a given record.
const TierAttrKey = "cli.analytics.tier"

// EnforcementMode controls what happens when a record's declared tier
// exceeds its tenant's configured ceiling. Set per-tenant in tenant_config.
type EnforcementMode string

const (
	// ModeStrip caps the effective tier at the ceiling and drops any
	// attribute above it, but keeps the rest of the record.
	ModeStrip EnforcementMode = "strip"
	// ModeReject discards the entire record.
	ModeReject EnforcementMode = "reject"
)

// SignalKind identifies which OTLP signal an event/attribute set came from,
// only used for error messages here (schema lookup is signal-specific and
// happens in the otlp package via Schema.MetricByName / MatchingSpanDef /
// LogEventByName).
type SignalKind string

const (
	SignalMetric SignalKind = "metric"
	SignalSpan   SignalKind = "span"
	SignalLog    SignalKind = "log"
)

// Decision is the outcome of evaluating one event (a metric data point, a
// span, or a log record) against the allowlist + tier rules.
type Decision struct {
	Accepted       bool
	RejectReason   string // set iff !Accepted
	EffectiveTier  string // tier actually applied, after ceiling capping
	KeptAttributes map[string]any
	DroppedKeys    []string // attribute keys removed (tier-stripped or invalid); logged by caller
}

// ResolveTier extracts the declared consent tier from an attribute set,
// preferring record-level attrs (span/log/datapoint) over resource-level
// attrs when both are present. Returns an error if the tier is missing or
// not a recognized tier name — this is a required-attribute structural
// failure (Layer 1), not a soft default, because silently defaulting would
// let a misconfigured SDK under- or over-declare sensitivity.
func ResolveTier(schema *allowlist.Schema, resourceAttrs, recordAttrs map[string]any) (string, error) {
	raw, ok := recordAttrs[TierAttrKey]
	if !ok {
		raw, ok = resourceAttrs[TierAttrKey]
	}
	if !ok {
		return "", fmt.Errorf("required attribute %q missing from both resource and record scope", TierAttrKey)
	}
	tier, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("attribute %q must be a string, got %T", TierAttrKey, raw)
	}
	if _, known := schema.TierRank(tier); !known {
		return "", fmt.Errorf("attribute %q has unrecognized value %q", TierAttrKey, tier)
	}
	return tier, nil
}

// EventDef is the allowlist definition for one event (metric/span/log),
// reduced to the parts EvaluateEvent needs. Callers build this from
// allowlist.Schema's per-signal lookups (MetricByName / MatchingSpanDef /
// LogEventByName) so this package doesn't need signal-specific matching
// logic duplicated here.
type EventDef struct {
	Found             bool
	AllowedAttributes []string
}

// EvaluateEvent applies the full Layer-2 + tier pipeline to one event:
//  1. If the event name itself isn't on the allowlist (def.Found == false),
//     reject the whole record — we do not accept arbitrary names.
//  2. Cap the effective tier at min(declared, tenant ceiling); if the
//     declared tier exceeds the ceiling and mode is ModeReject, reject.
//  3. For every attribute: drop it (not reject the record) if it isn't in
//     this event's allowed_attributes, isn't a standard resource attribute,
//     fails type/bounds validation, or requires a tier above the effective
//     tier. A single malformed/over-scoped field degrades the record, it
//     doesn't nuke otherwise-valid telemetry.
func EvaluateEvent(
	schema *allowlist.Schema,
	def EventDef,
	declaredTier string,
	tenantCeiling string,
	mode EnforcementMode,
	attrs map[string]any,
) Decision {
	if !def.Found {
		return Decision{Accepted: false, RejectReason: "event name not on allowlist"}
	}

	declaredRank, _ := schema.TierRank(declaredTier)
	ceilingRank, ceilingKnown := schema.TierRank(tenantCeiling)
	if !ceilingKnown {
		return Decision{Accepted: false, RejectReason: fmt.Sprintf("tenant ceiling %q is not a recognized tier", tenantCeiling)}
	}

	if declaredRank > ceilingRank && mode == ModeReject {
		return Decision{Accepted: false, RejectReason: fmt.Sprintf("declared tier %q exceeds tenant ceiling %q", declaredTier, tenantCeiling)}
	}

	effectiveRank := declaredRank
	if ceilingRank < effectiveRank {
		effectiveRank = ceilingRank
	}
	effectiveTier := tierNameForRank(schema, effectiveRank)

	allowed := allowlist.AllowedSet(def.AllowedAttributes)
	kept := make(map[string]any, len(attrs))
	var dropped []string

	for k, v := range attrs {
		if k == TierAttrKey {
			continue // handled separately, not stored as a generic attribute
		}
		if _, ok := allowed[k]; !ok {
			if schema.IsStandardResourceAttribute(k) {
				kept[k] = v
				continue
			}
			dropped = append(dropped, k)
			continue
		}
		attrDef, ok := schema.AttrDefFor(k)
		if !ok {
			dropped = append(dropped, k)
			continue
		}
		if err := allowlist.CheckAttribute(attrDef, v); err != nil {
			dropped = append(dropped, k)
			continue
		}
		minRank, _ := schema.TierRank(attrDef.MinTier)
		if minRank > effectiveRank {
			dropped = append(dropped, k)
			continue
		}
		kept[k] = v
	}

	return Decision{
		Accepted:       true,
		EffectiveTier:  effectiveTier,
		KeptAttributes: kept,
		DroppedKeys:    dropped,
	}
}

func tierNameForRank(schema *allowlist.Schema, rank int) string {
	for _, t := range schema.ConsentTiers {
		if t.Rank == rank {
			return t.Name
		}
	}
	return ""
}
