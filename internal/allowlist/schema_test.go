package allowlist

import (
	"os"
	"testing"
)

func loadRealSchema(t *testing.T) *Schema {
	t.Helper()
	data, err := os.ReadFile("../../schema/allowlist/v1.yaml")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	s, err := Parse(data)
	if err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	return s
}

func TestRealSchemaLoadsAndValidates(t *testing.T) {
	s := loadRealSchema(t)
	if s.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", s.SchemaVersion)
	}
	if !s.IsKnownSignalType("metrics") || !s.IsKnownSignalType("logs") || !s.IsKnownSignalType("traces") {
		t.Errorf("expected metrics/logs/traces to be known signal types")
	}
	if s.IsKnownSignalType("profiles") {
		t.Errorf("profiles must not be a known signal type")
	}
	if !s.IsStandardResourceAttribute("service.name") {
		t.Errorf("service.name should be a standard resource attribute")
	}
}

func TestMatchesPattern(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"cli.command.*", "cli.command.deploy", true},
		{"cli.command.*", "cli.subcommand.deploy", false},
		{"cli.command.*", "cli.command.", true},
		{"cli.help.viewed", "cli.help.viewed", true},
		{"cli.help.viewed", "cli.help.viewed.extra", false},
	}
	for _, c := range cases {
		got := matchesPattern(c.pattern, c.name)
		if got != c.want {
			t.Errorf("matchesPattern(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestMetricAndLogLookup(t *testing.T) {
	s := loadRealSchema(t)
	if _, ok := s.MetricByName("cli.command.invocations"); !ok {
		t.Errorf("expected cli.command.invocations to be defined")
	}
	if _, ok := s.MetricByName("cli.nonexistent"); ok {
		t.Errorf("cli.nonexistent should not resolve")
	}
	if _, ok := s.LogEventByName("cli.error.raised"); !ok {
		t.Errorf("expected cli.error.raised to be defined")
	}
	if _, ok := s.MatchingSpanDef("cli.command.deploy"); !ok {
		t.Errorf("expected cli.command.deploy to match span pattern")
	}
}

func TestTierRankOrdering(t *testing.T) {
	s := loadRealSchema(t)
	anon, _ := s.TierRank("anonymous")
	basic, _ := s.TierRank("basic")
	full, _ := s.TierRank("full")
	optin, _ := s.TierRank("optin_plus")
	if !(anon < basic && basic < full && full < optin) {
		t.Errorf("tier ranks not strictly increasing: anon=%d basic=%d full=%d optin=%d", anon, basic, full, optin)
	}
}
