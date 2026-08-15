package consent

import (
	"os"
	"testing"

	"github.com/getargvio/argvio/internal/allowlist"
)

func loadSchema(t *testing.T) *allowlist.Schema {
	t.Helper()
	data, err := os.ReadFile("../../schema/allowlist/v1.yaml")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	s, err := allowlist.Parse(data)
	if err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	return s
}

func logEventDef(s *allowlist.Schema, name string) EventDef {
	d, ok := s.LogEventByName(name)
	if !ok {
		return EventDef{Found: false}
	}
	return EventDef{Found: true, AllowedAttributes: d.AllowedAttributes}
}

func TestResolveTier_PrefersRecordOverResource(t *testing.T) {
	s := loadSchema(t)
	tier, err := ResolveTier(s,
		map[string]any{TierAttrKey: "anonymous"},
		map[string]any{TierAttrKey: "full"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tier != "full" {
		t.Errorf("tier = %q, want full", tier)
	}
}

func TestResolveTier_MissingIsError(t *testing.T) {
	s := loadSchema(t)
	if _, err := ResolveTier(s, map[string]any{}, map[string]any{}); err == nil {
		t.Errorf("expected error for missing tier attribute")
	}
}

func TestResolveTier_UnknownValueIsError(t *testing.T) {
	s := loadSchema(t)
	if _, err := ResolveTier(s, map[string]any{TierAttrKey: "godmode"}, nil); err == nil {
		t.Errorf("expected error for unrecognized tier value")
	}
}

func TestEvaluateEvent_UnrecognizedNameRejectsWhole(t *testing.T) {
	s := loadSchema(t)
	def := logEventDef(s, "cli.does.not.exist")
	dec := EvaluateEvent(s, def, "full", "full", ModeStrip, map[string]any{})
	if dec.Accepted {
		t.Errorf("expected rejection for unrecognized event name")
	}
}

func TestEvaluateEvent_StripsFieldsAboveEffectiveTier(t *testing.T) {
	s := loadSchema(t)
	def := logEventDef(s, "cli.error.raised")
	// Declared "full" is fine, but tenant ceiling caps at "basic" with strip
	// mode: full-tier fields (error message/stacktrace) must be dropped.
	attrs := map[string]any{
		"cli.command.name":     "deploy",
		"cli.error.type":       "TimeoutError",
		"cli.error.message":    "connection timed out after 30s",
		"cli.error.stacktrace": "at foo()\nat bar()",
	}
	dec := EvaluateEvent(s, def, "full", "basic", ModeStrip, attrs)
	if !dec.Accepted {
		t.Fatalf("expected acceptance with stripping, got reject: %s", dec.RejectReason)
	}
	if dec.EffectiveTier != "basic" {
		t.Errorf("effective tier = %q, want basic", dec.EffectiveTier)
	}
	if _, ok := dec.KeptAttributes["cli.error.message"]; ok {
		t.Errorf("cli.error.message should have been stripped at basic tier")
	}
	if _, ok := dec.KeptAttributes["cli.error.stacktrace"]; ok {
		t.Errorf("cli.error.stacktrace should have been stripped at basic tier")
	}
	if _, ok := dec.KeptAttributes["cli.command.name"]; !ok {
		t.Errorf("cli.command.name should have been kept (anonymous tier)")
	}
	foundDropped := map[string]bool{}
	for _, k := range dec.DroppedKeys {
		foundDropped[k] = true
	}
	if !foundDropped["cli.error.message"] || !foundDropped["cli.error.stacktrace"] {
		t.Errorf("expected both stripped keys to be reported in DroppedKeys, got %v", dec.DroppedKeys)
	}
}

func TestEvaluateEvent_RejectModeRejectsWholeRecordAboveCeiling(t *testing.T) {
	s := loadSchema(t)
	def := logEventDef(s, "cli.error.raised")
	attrs := map[string]any{"cli.command.name": "deploy"}
	dec := EvaluateEvent(s, def, "full", "basic", ModeReject, attrs)
	if dec.Accepted {
		t.Errorf("expected reject-mode to reject the whole record when declared tier exceeds ceiling")
	}
}

func TestEvaluateEvent_UnknownAttributeIsDroppedNotRejected(t *testing.T) {
	s := loadSchema(t)
	def := logEventDef(s, "cli.error.raised")
	attrs := map[string]any{
		"cli.command.name": "deploy",
		"totally.unknown":  "value",
	}
	dec := EvaluateEvent(s, def, "full", "full", ModeStrip, attrs)
	if !dec.Accepted {
		t.Fatalf("expected acceptance despite one unknown attribute")
	}
	if _, ok := dec.KeptAttributes["totally.unknown"]; ok {
		t.Errorf("unknown attribute should not be kept")
	}
	if _, ok := dec.KeptAttributes["cli.command.name"]; !ok {
		t.Errorf("known attribute should be kept")
	}
}

func TestEvaluateEvent_InvalidValueTypeIsDropped(t *testing.T) {
	s := loadSchema(t)
	def := logEventDef(s, "cli.error.raised")
	attrs := map[string]any{
		"cli.command.name": 12345, // wrong type: should be string
	}
	dec := EvaluateEvent(s, def, "full", "full", ModeStrip, attrs)
	if !dec.Accepted {
		t.Fatalf("expected acceptance (record-level), got reject: %s", dec.RejectReason)
	}
	if _, ok := dec.KeptAttributes["cli.command.name"]; ok {
		t.Errorf("wrongly-typed attribute should have been dropped")
	}
}

func TestEvaluateEvent_StandardResourceAttributeAlwaysKept(t *testing.T) {
	s := loadSchema(t)
	def := logEventDef(s, "cli.error.raised")
	attrs := map[string]any{"service.name": "my-cli"}
	dec := EvaluateEvent(s, def, "anonymous", "anonymous", ModeStrip, attrs)
	if !dec.Accepted {
		t.Fatalf("expected acceptance")
	}
	if _, ok := dec.KeptAttributes["service.name"]; !ok {
		t.Errorf("service.name should always be kept as a standard resource attribute")
	}
}
