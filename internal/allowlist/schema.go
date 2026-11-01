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

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

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
	Key       string   `yaml:"-"` // populated from the map key
	Type      AttrType `yaml:"type"`
	Enum      []string `yaml:"enum,omitempty"`
	MaxLength int      `yaml:"max_length,omitempty"`
	MaxItems  int      `yaml:"max_items,omitempty"`
	// Pattern, if set, is a regular expression a string value must match
	// in full (anchor it with ^...$). Compiled once by finalize.
	Pattern string `yaml:"pattern,omitempty"`
	// KeyPattern is required on, and only allowed on, an attribute family:
	// a key ending in ".*" (e.g. "cli.dim.*") stands for every key with that
	// prefix whose remainder matches KeyPattern in full. A metric, span or
	// log lists the family key in allowed_attributes to accept all of them.
	KeyPattern  string `yaml:"key_pattern,omitempty"`
	MinTier     string `yaml:"min_tier"`
	Sensitive   bool   `yaml:"sensitive"`
	Description string `yaml:"description,omitempty"`

	pattern    *regexp.Regexp
	keyPattern *regexp.Regexp
}

// familySuffix marks an attribute family key.
const familySuffix = ".*"

// MetricDef describes one allowed metric name.
type MetricDef struct {
	Name      string `yaml:"name"`
	Type      string `yaml:"type"` // sum | gauge | histogram
	Unit      string `yaml:"unit"`
	Monotonic bool   `yaml:"monotonic,omitempty"`
	// Temporality, if set (delta | cumulative), is the only aggregation
	// temporality accepted for this metric, which must then be a sum or a
	// histogram.
	Temporality string `yaml:"temporality,omitempty"`
	// ExplicitBounds, if set, are the only bucket boundaries accepted for
	// this metric, which must then be a histogram. Fixing them server-side
	// makes the buckets of different SDKs and data points addable.
	ExplicitBounds []float64 `yaml:"explicit_bounds,omitempty"`
	// RequiredAttributes must survive attribute filtering for a data point
	// to be kept; each must also be in AllowedAttributes.
	RequiredAttributes []string `yaml:"required_attributes,omitempty"`
	AllowedAttributes  []string `yaml:"allowed_attributes"`
}

// Aggregation temporalities a MetricDef may require.
const (
	TemporalityDelta      = "delta"
	TemporalityCumulative = "cumulative"
)

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
	// DropBody discards the record body at every tier. The body is free text
	// that full tier and above would otherwise persist, so an event whose
	// content must be an identifier from the CLI's code, never user input,
	// sets it.
	DropBody bool `yaml:"drop_body"`
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
	// attribute families by prefix ("cli.dim."), sorted longest first so the
	// most specific family wins.
	families []AttrDef
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

// AttrDefFor looks up the shared attribute definition for key: its own, or
// that of the attribute family it belongs to (with Key set to key).
func (s *Schema) AttrDefFor(key string) (AttrDef, bool) {
	if d, ok := s.Attributes[key]; ok {
		return d, true
	}
	if fam, ok := s.FamilyOf(key); ok {
		d := s.Attributes[fam]
		d.Key = key
		return d, true
	}
	return AttrDef{}, false
}

// FamilyOf returns the attribute family key (e.g. "cli.dim.*") that key
// belongs to: it has the family's prefix and the rest matches the family's
// key_pattern. A key that is itself defined belongs to no family.
func (s *Schema) FamilyOf(key string) (string, bool) {
	if _, ok := s.Attributes[key]; ok {
		return "", false
	}
	for _, f := range s.families {
		prefix := strings.TrimSuffix(f.Key, "*")
		if rest, ok := strings.CutPrefix(key, prefix); ok && f.keyPattern.MatchString(rest) {
			return f.Key, true
		}
	}
	return "", false
}

// IsAllowed reports whether key is in allowed (an AllowedSet) by name or
// through the attribute family it belongs to.
func (s *Schema) IsAllowed(allowed map[string]struct{}, key string) bool {
	if _, ok := allowed[key]; ok {
		return true
	}
	fam, ok := s.FamilyOf(key)
	if !ok {
		return false
	}
	_, ok = allowed[fam]
	return ok
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
		if def.Pattern != "" {
			if def.Type != AttrString {
				return fmt.Errorf("allowlist: attribute %q: pattern is only supported on type string", k)
			}
			re, err := regexp.Compile(def.Pattern)
			if err != nil {
				return fmt.Errorf("allowlist: attribute %q: invalid pattern: %w", k, err)
			}
			def.pattern = re
		}
		isFamily := strings.HasSuffix(k, familySuffix)
		switch {
		case isFamily && def.KeyPattern == "":
			return fmt.Errorf("allowlist: attribute family %q requires key_pattern", k)
		case !isFamily && def.KeyPattern != "":
			return fmt.Errorf("allowlist: attribute %q: key_pattern is only supported on a family (a key ending in %q)", k, familySuffix)
		case isFamily:
			re, err := regexp.Compile(def.KeyPattern)
			if err != nil {
				return fmt.Errorf("allowlist: attribute family %q: invalid key_pattern: %w", k, err)
			}
			def.keyPattern = re
			s.families = append(s.families, def)
		}
		s.Attributes[k] = def
	}
	sort.Slice(s.families, func(i, j int) bool {
		if len(s.families[i].Key) != len(s.families[j].Key) {
			return len(s.families[i].Key) > len(s.families[j].Key)
		}
		return s.families[i].Key < s.families[j].Key
	})

	s.metricsByName = make(map[string]MetricDef, len(s.Metrics))
	for _, m := range s.Metrics {
		if err := s.checkAttrRefs(m.AllowedAttributes); err != nil {
			return fmt.Errorf("allowlist: metric %q: %w", m.Name, err)
		}
		switch m.Temporality {
		case "":
		case TemporalityDelta, TemporalityCumulative:
			if m.Type != "sum" && m.Type != "histogram" {
				return fmt.Errorf("allowlist: metric %q: temporality is only supported on types sum and histogram", m.Name)
			}
		default:
			return fmt.Errorf("allowlist: metric %q: unknown temporality %q", m.Name, m.Temporality)
		}
		if len(m.ExplicitBounds) > 0 {
			if m.Type != "histogram" {
				return fmt.Errorf("allowlist: metric %q: explicit_bounds is only supported on type histogram", m.Name)
			}
			for i, b := range m.ExplicitBounds {
				if math.IsNaN(b) || math.IsInf(b, 0) || (i > 0 && b <= m.ExplicitBounds[i-1]) {
					return fmt.Errorf("allowlist: metric %q: explicit_bounds must be finite and strictly increasing", m.Name)
				}
			}
		}
		allowed := AllowedSet(m.AllowedAttributes)
		for _, k := range m.RequiredAttributes {
			if _, ok := allowed[k]; !ok {
				return fmt.Errorf("allowlist: metric %q: required attribute %q is not in allowed_attributes", m.Name, k)
			}
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
