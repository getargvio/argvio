package allowlist

import (
	"fmt"
	"os"
	"slices"
	"strings"
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

// Binary size and startup overhead are basic-tier measures carried once per
// process on cli.session.started: size is a property of the build, so it is
// not repeated on every command span or log.
func TestSessionFootprintAttributes(t *testing.T) {
	s := loadRealSchema(t)
	basic, _ := s.TierRank("basic")
	attrs := []string{"cli.binary.size_bytes", "cli.startup.duration_ms"}

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

	session, ok := s.LogEventByName("cli.session.started")
	if !ok {
		t.Fatal("expected cli.session.started to be defined")
	}
	has := func(list []string, a string) bool {
		for _, x := range list {
			if x == a {
				return true
			}
		}
		return false
	}
	for _, a := range attrs {
		if !has(session.AllowedAttributes, a) {
			t.Errorf("cli.session.started should allow %s", a)
		}
	}

	for _, ev := range []string{"cli.command.invoked", "cli.command.completed"} {
		def, ok := s.LogEventByName(ev)
		if !ok {
			t.Fatalf("expected %s to be defined", ev)
		}
		for _, a := range attrs {
			if has(def.AllowedAttributes, a) {
				t.Errorf("%s should not allow %s", ev, a)
			}
		}
	}
	if span, ok := s.MatchingSpanDef("cli.command.deploy"); ok {
		for _, a := range attrs {
			if has(span.AllowedAttributes, a) {
				t.Errorf("cli.command.* span should not allow %s", a)
			}
		}
	}
}

// The error reason is a basic-tier identifier from a vendor's fixed
// vocabulary, allowed wherever the coarse error category is.
func TestErrorReasonAttribute(t *testing.T) {
	s := loadRealSchema(t)
	def, ok := s.AttrDefFor("cli.error.reason")
	if !ok {
		t.Fatal("cli.error.reason not defined")
	}
	basic, _ := s.TierRank("basic")
	if rank, _ := s.TierRank(def.MinTier); rank != basic || def.Type != AttrString || def.MaxLength != 64 || def.Sensitive {
		t.Errorf("cli.error.reason = %+v, want basic-tier, non-sensitive string of at most 64", def)
	}

	m, _ := s.MetricByName("cli.errors")
	ev, _ := s.LogEventByName("cli.error.raised")
	for name, allowed := range map[string][]string{"cli.errors": m.AllowedAttributes, "cli.error.raised": ev.AllowedAttributes} {
		if !slices.Contains(allowed, "cli.error.reason") {
			t.Errorf("%s should allow cli.error.reason", name)
		}
	}

	valid := []string{"no-token", "invalid_url", "config.read-failed", "e404", "0", strings.Repeat("a", 64)}
	invalid := []string{"", "No-Token", "no token", "-leading-dash", ".hidden", "path/to/x", "naïve", "x\n", strings.Repeat("a", 65)}
	for _, v := range valid {
		if err := CheckAttribute(def, v); err != nil {
			t.Errorf("CheckAttribute(%q) = %v, want nil", v, err)
		}
	}
	for _, v := range invalid {
		if err := CheckAttribute(def, v); err == nil {
			t.Errorf("CheckAttribute(%q) = nil, want an error", v)
		}
	}
}

func TestPatternValidatedAtLoad(t *testing.T) {
	base := `
schema_version: 1
consent_tiers: [{name: basic, rank: 0}]
attributes:
  a:
    type: %s
    pattern: %q
    min_tier: basic
`
	for _, c := range []struct {
		typ, pattern, wantErr string
	}{
		{"int", "^[0-9]+$", "only supported on type string"},
		{"string", "([", "invalid pattern"},
	} {
		_, err := Parse([]byte(fmt.Sprintf(base, c.typ, c.pattern)))
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("type %s pattern %q: err = %v, want %q", c.typ, c.pattern, err, c.wantErr)
		}
	}
	if _, err := Parse([]byte(fmt.Sprintf(base, "string", "^[a-z]+$"))); err != nil {
		t.Errorf("valid pattern: %v", err)
	}
}

// Vendor counters are anonymous tier, so they are reported even without an
// install id; the pattern and length are what keep free text out.
func TestCounterMetric(t *testing.T) {
	s := loadRealSchema(t)
	m, ok := s.MetricByName("cli.counter")
	if !ok {
		t.Fatal("cli.counter not defined")
	}
	if m.Type != "sum" || !m.Monotonic || m.Unit != "1" || m.Temporality != TemporalityDelta {
		t.Errorf("cli.counter = %+v, want a monotonic delta sum with unit 1", m)
	}
	if !slices.Equal(m.RequiredAttributes, []string{"cli.counter.name"}) {
		t.Errorf("required attributes = %v, want cli.counter.name", m.RequiredAttributes)
	}
	for _, k := range []string{"cli.counter.name", "cli.counter.label", "cli.command.name", "cli.version", "cli.os", "cli.arch", "cli.is_ci"} {
		if !slices.Contains(m.AllowedAttributes, k) {
			t.Errorf("cli.counter should allow %s", k)
		}
	}
	if slices.Contains(m.AllowedAttributes, "cli.install_id") || slices.Contains(m.AllowedAttributes, "cli.session_id") {
		t.Error("cli.counter must not carry install or session ids")
	}

	anonymous, _ := s.TierRank("anonymous")
	valid := []string{"openapi.security-schemes", "rules_generated", "get", "0", strings.Repeat("a", 64)}
	invalid := []string{"", "Bearer", "two words", "-x", ".x", "/etc/passwd", "https://example.com", "a=b", "naïve", "x\n", strings.Repeat("a", 65)}
	for _, key := range []string{"cli.counter.name", "cli.counter.label"} {
		def, ok := s.AttrDefFor(key)
		if !ok {
			t.Fatalf("%s not defined", key)
		}
		if rank, _ := s.TierRank(def.MinTier); rank != anonymous || def.Type != AttrString || def.MaxLength != 64 || def.Sensitive {
			t.Errorf("%s = %+v, want anonymous-tier, non-sensitive string of at most 64", key, def)
		}
		for _, v := range valid {
			if err := CheckAttribute(def, v); err != nil {
				t.Errorf("%s: CheckAttribute(%q) = %v, want nil", key, v, err)
			}
		}
		for _, v := range invalid {
			if err := CheckAttribute(def, v); err == nil {
				t.Errorf("%s: CheckAttribute(%q) = nil, want an error", key, v)
			}
		}
	}
}

func TestMetricShapeValidatedAtLoad(t *testing.T) {
	base := `
schema_version: 1
consent_tiers: [{name: basic, rank: 0}]
attributes:
  a: {type: string, min_tier: basic}
  b: {type: string, min_tier: basic}
metrics:
  - name: m
    type: %s
    temporality: %s
    required_attributes: [%s]
    allowed_attributes: [a]
`
	for _, c := range []struct {
		typ, temporality, required, wantErr string
	}{
		{"gauge", "delta", "a", "temporality is only supported on types sum and histogram"},
		{"sum", "sometimes", "a", "unknown temporality"},
		{"sum", "delta", "b", `required attribute "b" is not in allowed_attributes`},
	} {
		_, err := Parse([]byte(fmt.Sprintf(base, c.typ, c.temporality, c.required)))
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%+v: err = %v, want %q", c, err, c.wantErr)
		}
	}
	if _, err := Parse([]byte(fmt.Sprintf(base, "sum", "cumulative", "a"))); err != nil {
		t.Errorf("valid metric: %v", err)
	}
}

// Vendor measures are counters' counterpart for durations: the same
// anonymous-tier name and label, in a histogram whose buckets are fixed.
func TestMeasureMetric(t *testing.T) {
	s := loadRealSchema(t)
	m, ok := s.MetricByName("cli.measure")
	if !ok {
		t.Fatal("cli.measure not defined")
	}
	if m.Type != "histogram" || m.Unit != "ms" || m.Temporality != TemporalityDelta {
		t.Errorf("cli.measure = %+v, want a delta histogram in ms", m)
	}
	wantBounds := []float64{1, 5, 10, 50, 100, 500, 1000, 5000, 10000, 60000, 300000}
	if !slices.Equal(m.ExplicitBounds, wantBounds) {
		t.Errorf("explicit bounds = %v, want %v", m.ExplicitBounds, wantBounds)
	}
	if !slices.Equal(m.RequiredAttributes, []string{"cli.measure.name"}) {
		t.Errorf("required attributes = %v, want cli.measure.name", m.RequiredAttributes)
	}
	for _, k := range []string{"cli.measure.name", "cli.measure.label", "cli.command.name", "cli.version", "cli.os", "cli.arch", "cli.is_ci"} {
		if !slices.Contains(m.AllowedAttributes, k) {
			t.Errorf("cli.measure should allow %s", k)
		}
	}
	if slices.Contains(m.AllowedAttributes, "cli.install_id") || slices.Contains(m.AllowedAttributes, "cli.session_id") {
		t.Error("cli.measure must not carry install or session ids")
	}

	anonymous, _ := s.TierRank("anonymous")
	valid := []string{"john.run", "scan_duration", "hmac-sha256", "0", strings.Repeat("a", 64)}
	invalid := []string{"", "John", "two words", "-x", ".x", "/etc/passwd", "https://example.com", "a=b", "naïve", "x\n", strings.Repeat("a", 65)}
	for _, key := range []string{"cli.measure.name", "cli.measure.label"} {
		def, ok := s.AttrDefFor(key)
		if !ok {
			t.Fatalf("%s not defined", key)
		}
		if rank, _ := s.TierRank(def.MinTier); rank != anonymous || def.Type != AttrString || def.MaxLength != 64 || def.Sensitive {
			t.Errorf("%s = %+v, want anonymous-tier, non-sensitive string of at most 64", key, def)
		}
		for _, v := range valid {
			if err := CheckAttribute(def, v); err != nil {
				t.Errorf("%s: CheckAttribute(%q) = %v, want nil", key, v, err)
			}
		}
		for _, v := range invalid {
			if err := CheckAttribute(def, v); err == nil {
				t.Errorf("%s: CheckAttribute(%q) = nil, want an error", key, v)
			}
		}
	}
}

func TestExplicitBoundsValidatedAtLoad(t *testing.T) {
	base := `
schema_version: 1
consent_tiers: [{name: basic, rank: 0}]
attributes:
  a: {type: string, min_tier: basic}
metrics:
  - name: m
    type: %s
    explicit_bounds: %s
    allowed_attributes: [a]
`
	for _, c := range []struct {
		typ, bounds, wantErr string
	}{
		{"sum", "[1, 2]", "explicit_bounds is only supported on type histogram"},
		{"histogram", "[1, 1]", "strictly increasing"},
		{"histogram", "[5, 1]", "strictly increasing"},
		{"histogram", "[.inf]", "finite"},
		{"histogram", "[.nan]", "finite"},
	} {
		_, err := Parse([]byte(fmt.Sprintf(base, c.typ, c.bounds)))
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%+v: err = %v, want %q", c, err, c.wantErr)
		}
	}
	if _, err := Parse([]byte(fmt.Sprintf(base, "histogram", "[1, 5, 10]"))); err != nil {
		t.Errorf("valid metric: %v", err)
	}
}

// The session environment attributes are basic-tier, non-sensitive, and only
// carried once per process on cli.session.started.
func TestSessionEnvironmentAttributes(t *testing.T) {
	s := loadRealSchema(t)
	basic, _ := s.TierRank("basic")
	types := map[string]AttrType{
		"cli.shell":                   AttrString,
		"cli.terminal":                AttrString,
		"cli.stdout.is_tty":           AttrBool,
		"cli.runtime.name":            AttrString,
		"cli.runtime.version":         AttrString,
		"cli.config.customized_count": AttrInt,
		"cli.auth.state":              AttrString,
	}
	session, ok := s.LogEventByName("cli.session.started")
	if !ok {
		t.Fatal("expected cli.session.started to be defined")
	}
	for a, typ := range types {
		def, ok := s.AttrDefFor(a)
		if !ok {
			t.Fatalf("attribute %s not defined", a)
		}
		if def.Type != typ || def.Sensitive {
			t.Errorf("%s = %+v, want a non-sensitive %s", a, def, typ)
		}
		if rank, _ := s.TierRank(def.MinTier); rank != basic {
			t.Errorf("%s min_tier = %s, want basic", a, def.MinTier)
		}
		if !slices.Contains(session.AllowedAttributes, a) {
			t.Errorf("cli.session.started should allow %s", a)
		}
		for _, ev := range []string{"cli.command.invoked", "cli.command.completed"} {
			def, _ := s.LogEventByName(ev)
			if slices.Contains(def.AllowedAttributes, a) {
				t.Errorf("%s should not allow %s", ev, a)
			}
		}
	}

	// Free text is bounded.
	for _, a := range []string{"cli.runtime.name", "cli.runtime.version"} {
		def, _ := s.AttrDefFor(a)
		long := strings.Repeat("a", 33)
		if err := CheckAttribute(def, "go"); err != nil {
			t.Errorf("%s: CheckAttribute(go) = %v, want nil", a, err)
		}
		if err := CheckAttribute(def, long); err == nil {
			t.Errorf("%s: a 33-byte value should be rejected", a)
		}
	}

	auth, _ := s.AttrDefFor("cli.auth.state")
	for _, v := range []string{"authenticated", "anonymous", "unknown"} {
		if err := CheckAttribute(auth, v); err != nil {
			t.Errorf("CheckAttribute(%q) = %v, want nil", v, err)
		}
	}
	for _, v := range []string{"", "Authenticated", "alice@example.com", "signed-in"} {
		if err := CheckAttribute(auth, v); err == nil {
			t.Errorf("CheckAttribute(%q) = nil, want an error", v)
		}
	}
}
