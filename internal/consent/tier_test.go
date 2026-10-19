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

// The SDKs tie command outcomes, errors and session starts to the install
// at basic tier and above (journey analytics are built from them), and
// nothing identifying may survive at anonymous.
func TestEvaluateEvent_JourneyAttributesByTier(t *testing.T) {
	s := loadSchema(t)
	cases := []struct {
		event string
		attrs map[string]any
		// keys expected at basic; the same keys must all be dropped at anonymous
		basicOnly []string
		// keys kept at every tier
		always []string
	}{
		{
			"cli.command.completed",
			map[string]any{"cli.command.name": "deploy", "cli.command.exit_code": 1, "cli.command.duration_ms": 12, "cli.session_id": "s", "cli.install_id": "i"},
			[]string{"cli.session_id", "cli.install_id"},
			[]string{"cli.command.name", "cli.command.exit_code", "cli.command.duration_ms"},
		},
		{
			"cli.error.raised",
			map[string]any{"cli.command.name": "deploy", "cli.error.type": "network", "cli.session_id": "s", "cli.install_id": "i"},
			[]string{"cli.error.type", "cli.session_id", "cli.install_id"},
			[]string{"cli.command.name"},
		},
		{
			"cli.session.started",
			map[string]any{"cli.session_id": "s", "cli.install_id": "i", "cli.install.method": "brew"},
			[]string{"cli.session_id", "cli.install_id"},
			[]string{"cli.install.method"},
		},
	}
	for _, c := range cases {
		def := logEventDef(s, c.event)
		for tier, wantBasic := range map[string]bool{"basic": true, "anonymous": false} {
			dec := EvaluateEvent(s, def, tier, tier, ModeStrip, c.attrs)
			if !dec.Accepted {
				t.Fatalf("%s at %s: rejected: %s", c.event, tier, dec.RejectReason)
			}
			for _, k := range c.basicOnly {
				if _, kept := dec.KeptAttributes[k]; kept != wantBasic {
					t.Errorf("%s at %s: %s kept = %v, want %v", c.event, tier, k, kept, wantBasic)
				}
			}
			for _, k := range c.always {
				if _, kept := dec.KeptAttributes[k]; !kept {
					t.Errorf("%s at %s: %s should be kept at every tier", c.event, tier, k)
				}
			}
		}
	}
}

// A malformed error reason is dropped like any other invalid value; the
// error record itself is kept.
func TestEvaluateEvent_ErrorReasonPattern(t *testing.T) {
	s := loadSchema(t)
	def := logEventDef(s, "cli.error.raised")
	for value, keep := range map[string]bool{"no-token": true, "could not open /home/alice": false} {
		dec := EvaluateEvent(s, def, "basic", "basic", ModeStrip, map[string]any{"cli.error.reason": value})
		if !dec.Accepted {
			t.Fatalf("%q: record rejected: %s", value, dec.RejectReason)
		}
		if _, kept := dec.KeptAttributes["cli.error.reason"]; kept != keep {
			t.Errorf("%q: kept = %v, want %v", value, kept, keep)
		}
	}
	// Basic tier: an anonymous record loses it.
	dec := EvaluateEvent(s, def, "anonymous", "basic", ModeStrip, map[string]any{"cli.error.reason": "no-token"})
	if _, kept := dec.KeptAttributes["cli.error.reason"]; kept {
		t.Error("anonymous tier should strip cli.error.reason")
	}
}
