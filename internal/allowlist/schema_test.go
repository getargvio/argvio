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

// Feature discovery over time since install needs an install id on the
// activation event, and that id is only retained at basic tier and above.
func TestFeatureActivatedCarriesInstallAndSessionIDs(t *testing.T) {
	s := loadRealSchema(t)
	def, ok := s.LogEventByName("cli.feature.activated")
	if !ok {
		t.Fatal("expected cli.feature.activated to be defined")
	}
	allowed := map[string]bool{}
	for _, a := range def.AllowedAttributes {
		allowed[a] = true
	}
	for _, a := range []string{"cli.activation.feature", "cli.install_id", "cli.session_id"} {
		if !allowed[a] {
			t.Errorf("cli.feature.activated should allow %s", a)
		}
	}
	basic, _ := s.TierRank("basic")
	for _, a := range []string{"cli.install_id", "cli.session_id", "cli.activation.feature"} {
		attr, ok := s.AttrDefFor(a)
		if !ok {
			t.Fatalf("attribute %s not defined", a)
		}
		if rank, _ := s.TierRank(attr.MinTier); rank != basic {
			t.Errorf("%s min_tier = %s, want basic", a, attr.MinTier)
		}
	}
}

// Process resource usage is a basic-tier measure, carried on the command span
// only: spans feed the continuous aggregates, and the attributes are not
// allowed on any log event.
func TestProcessResourceUsageAttributes(t *testing.T) {
	s := loadRealSchema(t)
	basic, _ := s.TierRank("basic")
	attrs := []string{"cli.process.peak_rss_bytes", "cli.process.cpu_time_ms"}

	for _, a := range attrs {
		def, ok := s.AttrDefFor(a)
		if !ok {
			t.Fatalf("attribute %s not defined", a)
		}
		if def.Type != AttrInt {
			t.Errorf("%s type = %v, want int", a, def.Type)
		}
		if def.Sensitive {
			t.Errorf("%s must not be sensitive", a)
		}
		if rank, _ := s.TierRank(def.MinTier); rank != basic {
			t.Errorf("%s min_tier = %s, want basic", a, def.MinTier)
		}
	}

	span, ok := s.MatchingSpanDef("cli.command.deploy")
	if !ok {
		t.Fatal("expected cli.command.* span to be defined")
	}
	allowed := map[string]bool{}
	for _, a := range span.AllowedAttributes {
		allowed[a] = true
	}
	for _, a := range attrs {
		if !allowed[a] {
			t.Errorf("cli.command.* span should allow %s", a)
		}
	}

	for _, ev := range []string{"cli.command.invoked", "cli.command.completed"} {
		def, ok := s.LogEventByName(ev)
		if !ok {
			t.Fatalf("expected %s to be defined", ev)
		}
		for _, a := range def.AllowedAttributes {
			for _, u := range attrs {
				if a == u {
					t.Errorf("%s should not allow %s", ev, u)
				}
			}
		}
	}
}
