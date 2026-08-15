// Package allowlist loads and evaluates the versioned, data-driven taxonomy
// schema (schema/allowlist/v1.yaml) that defines every metric/span/log event
// the public ingest server accepts, the attributes each may carry, and the
// consent tier each attribute belongs to.
//
// This is Layer 2 semantic validation (see docs/allowlist.md) plus the field
// map used by the consent-tier enforcement in internal/consent. Nothing in
// the ingest path hardcodes event names or attribute keys — everything is
// resolved through a *Schema loaded from this file.
package allowlist

import "fmt"

// AttrType is the accepted value type for an attribute.
type AttrType string

const (
	AttrString      AttrType = "string"
	AttrInt         AttrType = "int"
	AttrBool        AttrType = "bool"
	AttrDouble      AttrType = "double"
	AttrStringArray AttrType = "string_array"
)

// Tier is a consent tier name, ordered by Rank (higher = more sensitive).
type Tier struct {
	Name        string `yaml:"name"`
	Rank        int    `yaml:"rank"`
	Description string `yaml:"description"`
}

// AttrDef describes one attribute key: its type, bounds, and the minimum
// consent tier a record must declare for the attribute to be retained.
type AttrDef struct {
	Key         string   `yaml:"-"` // populated from the map key
	Type        AttrType `yaml:"type"`
	Enum        []string `yaml:"enum,omitempty"`
	MaxLength   int      `yaml:"max_length,omitempty"`
	MaxItems    int      `yaml:"max_items,omitempty"`
	MinTier     string   `yaml:"min_tier"`
	Sensitive   bool     `yaml:"sensitive"`
	Description string   `yaml:"description,omitempty"`
}

// MetricDef describes one allowed metric name.
type MetricDef struct {
	Name              string   `yaml:"name"`
	Type              string   `yaml:"type"` // sum | gauge | histogram
	Unit              string   `yaml:"unit"`
	Monotonic         bool     `yaml:"monotonic,omitempty"`
	AllowedAttributes []string `yaml:"allowed_attributes"`
}

// SpanDef describes one allowed span name pattern (glob-style "*" suffix
// wildcard only — see MatchesSpanName).
type SpanDef struct {
	NamePattern       string   `yaml:"name_pattern"`
	AllowedAttributes []string `yaml:"allowed_attributes"`
}

// LogDef describes one allowed log record event name.
type LogDef struct {
	EventName         string   `yaml:"event_name"`
	AllowedAttributes []string `yaml:"allowed_attributes"`
}

// Schema is the fully parsed, validated allowlist/tier definition.
type Schema struct {
	SchemaVersion              int                `yaml:"schema_version"`
	SupportedSignalTypes       []string           `yaml:"supported_signal_types"`
	StandardResourceAttributes []string           `yaml:"standard_resource_attributes"`
	ConsentTiers               []Tier             `yaml:"consent_tiers"`
	Attributes                 map[string]AttrDef `yaml:"attributes"`
	Metrics                    []MetricDef        `yaml:"metrics"`
	Spans                      []SpanDef          `yaml:"spans"`
	Logs                       []LogDef           `yaml:"logs"`

	// derived indexes, built by finalize()
	tierRank       map[string]int
	standardResSet map[string]struct{}
	signalTypeSet  map[string]struct{}
	metricsByName  map[string]MetricDef
	logsByName     map[string]LogDef
}

// TierRank returns the numeric rank for a tier name and whether it is known.
func (s *Schema) TierRank(name string) (int, bool) {
	r, ok := s.tierRank[name]
	return r, ok
}

// IsKnownSignalType reports whether signalType (e.g. "metrics", "logs",
// "traces") is one this deployment accepts at all.
func (s *Schema) IsKnownSignalType(signalType string) bool {
	_, ok := s.signalTypeSet[signalType]
	return ok
}

// IsStandardResourceAttribute reports whether key is an always-allowed OTel
// SDK resource attribute (service.name etc.), independent of per-event
// allowlists.
func (s *Schema) IsStandardResourceAttribute(key string) bool {
	_, ok := s.standardResSet[key]
	return ok
}

// MetricByName looks up an allowed metric definition by exact name.
func (s *Schema) MetricByName(name string) (MetricDef, bool) {
	m, ok := s.metricsByName[name]
	return m, ok
}

// LogEventByName looks up an allowed log event definition by exact name.
func (s *Schema) LogEventByName(name string) (LogDef, bool) {
	l, ok := s.logsByName[name]
	return l, ok
}

// MatchingSpanDef returns the first span allowlist entry whose pattern
// matches name, if any. Patterns support a single trailing "*" wildcard
// (e.g. "cli.command.*"); anything else must match exactly.
func (s *Schema) MatchingSpanDef(name string) (SpanDef, bool) {
	for _, sp := range s.Spans {
		if matchesPattern(sp.NamePattern, name) {
			return sp, true
		}
	}
	return SpanDef{}, false
}

func matchesPattern(pattern, name string) bool {
	if pattern == name {
		return true
	}
	if len(pattern) > 0 && pattern[len(pattern)-1] == '*' {
		prefix := pattern[:len(pattern)-1]
		return len(name) >= len(prefix) && name[:len(prefix)] == prefix
	}
	return false
}

// AttrDefFor looks up the shared attribute definition for key.
func (s *Schema) AttrDefFor(key string) (AttrDef, bool) {
	d, ok := s.Attributes[key]
	return d, ok
}

// finalize builds derived lookup indexes and validates internal consistency
// (referenced attribute keys exist, min_tier values are known tiers, etc).
// Called once by Load/Parse; never mutates a Schema already handed out to
// callers (the loader swaps the pointer atomically instead).
func (s *Schema) finalize() error {
	s.tierRank = make(map[string]int, len(s.ConsentTiers))
	for _, t := range s.ConsentTiers {
		s.tierRank[t.Name] = t.Rank
	}
	if len(s.tierRank) == 0 {
		return fmt.Errorf("allowlist: no consent_tiers defined")
	}

	s.standardResSet = make(map[string]struct{}, len(s.StandardResourceAttributes))
	for _, k := range s.StandardResourceAttributes {
		s.standardResSet[k] = struct{}{}
	}

	s.signalTypeSet = make(map[string]struct{}, len(s.SupportedSignalTypes))
	for _, t := range s.SupportedSignalTypes {
		s.signalTypeSet[t] = struct{}{}
	}

	for k, def := range s.Attributes {
		def.Key = k
		if _, ok := s.tierRank[def.MinTier]; !ok {
			return fmt.Errorf("allowlist: attribute %q references unknown min_tier %q", k, def.MinTier)
		}
		s.Attributes[k] = def
	}

	s.metricsByName = make(map[string]MetricDef, len(s.Metrics))
	for _, m := range s.Metrics {
		if err := s.checkAttrRefs(m.AllowedAttributes); err != nil {
			return fmt.Errorf("allowlist: metric %q: %w", m.Name, err)
		}
		s.metricsByName[m.Name] = m
	}

	for _, sp := range s.Spans {
		if err := s.checkAttrRefs(sp.AllowedAttributes); err != nil {
			return fmt.Errorf("allowlist: span pattern %q: %w", sp.NamePattern, err)
		}
	}

	s.logsByName = make(map[string]LogDef, len(s.Logs))
	for _, l := range s.Logs {
		if err := s.checkAttrRefs(l.AllowedAttributes); err != nil {
			return fmt.Errorf("allowlist: log event %q: %w", l.EventName, err)
		}
		s.logsByName[l.EventName] = l
	}

	return nil
}

func (s *Schema) checkAttrRefs(keys []string) error {
	for _, k := range keys {
		if _, ok := s.Attributes[k]; !ok {
			return fmt.Errorf("references undefined attribute %q", k)
		}
	}
	return nil
}
