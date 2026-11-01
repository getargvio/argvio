// Package catalog parses and validates argvio.metrics.yaml, the file in a
// CLI's repository that defines what its product metrics mean: title,
// description, unit, dimensions, direction, ordering, minimum tier and, for
// ratios, how they are computed (docs/product-metrics-design.md, F3).
//
// The file is the only source of truth for definitions: `argvio metric apply`
// replaces a service's whole catalog with it, and nothing edits a definition
// field by field.
package catalog

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Version is the only file format version this package reads.
const Version = 1

// Metric kinds. Counter, duration and value are what a CLI sends
// (cli.counter, cli.measure, cli.value); a ratio is computed at query time
// from two other series.
const (
	KindCounter  = "counter"
	KindDuration = "duration"
	KindValue    = "value"
	KindRatio    = "ratio"
)

// Directions say whether a rising metric is good news, for colouring.
const (
	DirectionUpIsGood   = "up_is_good"
	DirectionDownIsGood = "down_is_good"
	DirectionNeutral    = "neutral"
)

// Built-in series a ratio may use as an operand.
const (
	BuiltinCommandInvocations = "command_invocations"
	BuiltinSessions           = "sessions"
	BuiltinActiveInstalls     = "active_installs"
)

// Value aggregations a ratio operand over a value metric may use.
const (
	AggSum   = "sum"
	AggCount = "count"
)

// MaxDims is the most dimensions a definition may declare: the highest
// public.max_metric_dims the server accepts. A tenant's own limit may be
// lower; ingest enforces it.
const MaxDims = 8

// Bounds on the free text, so a catalog stays a catalog.
const (
	maxTitleLength       = 100
	maxDescriptionLength = 1000
	maxMetrics           = 500
)

var (
	identifier  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	serviceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	units       = map[string]bool{"1": true, "By": true, "%": true, "ms": true}
	tiers       = map[string]bool{"anonymous": true, "basic": true, "full": true, "optin_plus": true}
	kinds       = map[string]bool{KindCounter: true, KindDuration: true, KindValue: true, KindRatio: true}
	directions  = map[string]bool{DirectionUpIsGood: true, DirectionDownIsGood: true, DirectionNeutral: true}
	builtins    = map[string]bool{BuiltinCommandInvocations: true, BuiltinSessions: true, BuiltinActiveInstalls: true}
)

// File is one argvio.metrics.yaml.
type File struct {
	Version int      `yaml:"version" json:"version"`
	Service string   `yaml:"service" json:"service"`
	Metrics []Metric `yaml:"metrics" json:"metrics"`
}

// Metric is one definition. Fields that don't apply to its kind must be
// left out.
type Metric struct {
	Name        string              `yaml:"name" json:"name"`
	Kind        string              `yaml:"kind" json:"kind"`
	Title       string              `yaml:"title" json:"title"`
	Description string              `yaml:"description,omitempty" json:"description,omitempty"`
	Unit        string              `yaml:"unit,omitempty" json:"unit,omitempty"`
	Dims        []string            `yaml:"dims,omitempty" json:"dims,omitempty"`
	Direction   string              `yaml:"direction,omitempty" json:"direction,omitempty"`
	DimOrder    map[string][]string `yaml:"dim_order,omitempty" json:"dim_order,omitempty"`
	MinTier     string              `yaml:"min_tier,omitempty" json:"min_tier,omitempty"`

	// Ratio only.
	Numerator   *Operand `yaml:"numerator,omitempty" json:"numerator,omitempty"`
	Denominator *Operand `yaml:"denominator,omitempty" json:"denominator,omitempty"`
	Scale       float64  `yaml:"scale,omitempty" json:"scale,omitempty"`
}

// Operand is one side of a ratio: another metric of the file, or a built-in
// series, optionally narrowed. For a metric, Where filters its dimensions;
// for command_invocations, Where may hold "command".
type Operand struct {
	Metric  string            `yaml:"metric,omitempty" json:"metric,omitempty"`
	Builtin string            `yaml:"builtin,omitempty" json:"builtin,omitempty"`
	Where   map[string]string `yaml:"where,omitempty" json:"where,omitempty"`
	Agg     string            `yaml:"agg,omitempty" json:"agg,omitempty"`
}

// Parse reads and validates a catalog file. Unknown fields are errors, so a
// typo can't silently drop a setting. Defaults are filled in: direction
// neutral, unit "1" for counters, values and ratios and "ms" for durations, scale 1
// for ratios.
func Parse(data []byte) (*File, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("catalog: parse: %w", err)
	}
	f.applyDefaults()
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

func (f *File) applyDefaults() {
	for i := range f.Metrics {
		m := &f.Metrics[i]
		if m.Direction == "" {
			m.Direction = DirectionNeutral
		}
		switch m.Kind {
		case KindCounter, KindValue, KindRatio:
			if m.Unit == "" {
				m.Unit = "1"
			}
		case KindDuration:
			if m.Unit == "" {
				m.Unit = "ms"
			}
		}
		if m.Kind == KindRatio && m.Scale == 0 {
			m.Scale = 1
		}
	}
}

// ValidationError lists every problem found in a file, so one run of
// `argvio metric validate` reports them all.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "catalog: invalid file:\n  - " + strings.Join(e.Problems, "\n  - ")
}

// Validate checks f against the format rules. It returns a *ValidationError
// listing every problem, or nil.
func (f *File) Validate() error {
	var p []string
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }

	if f.Version != Version {
		add("version must be %d, got %d", Version, f.Version)
	}
	if !serviceName.MatchString(f.Service) {
		add("service %q must be the CLI's name as its SDK sends it (service.name): letters, digits, '.', '_' or '-', at most 128", f.Service)
	}
	if len(f.Metrics) > maxMetrics {
		add("at most %d metrics per file, got %d", maxMetrics, len(f.Metrics))
	}

	byName := map[string]Metric{}
	for i, m := range f.Metrics {
		at := fmt.Sprintf("metrics[%d]", i)
		if m.Name != "" {
			at = fmt.Sprintf("metric %q", m.Name)
		}
		if !identifier.MatchString(m.Name) {
			add("%s: name must match ^[a-z0-9][a-z0-9._-]*$ and be at most 64 characters", at)
		} else if _, dup := byName[m.Name]; dup {
			add("%s: defined twice", at)
		}
		byName[m.Name] = m
		p = append(p, m.problems(at)...)
	}

	// Ratio operands may only point at non-ratio metrics of the same file.
	for _, m := range f.Metrics {
		if m.Kind != KindRatio {
			continue
		}
		for side, o := range map[string]*Operand{"numerator": m.Numerator, "denominator": m.Denominator} {
			if o == nil || o.Metric == "" {
				continue
			}
			ref, ok := byName[o.Metric]
			switch {
			case !ok:
				add("metric %q: %s refers to %q, which the file doesn't define", m.Name, side, o.Metric)
			case ref.Kind == KindRatio:
				add("metric %q: %s refers to ratio %q; a ratio can only use counters, durations or values", m.Name, side, o.Metric)
			default:
				for k := range o.Where {
					if !contains(ref.Dims, k) {
						add("metric %q: %s filters on %q, which %q doesn't declare in dims", m.Name, side, k, o.Metric)
					}
				}
				if o.Agg != "" && ref.Kind != KindValue {
					add("metric %q: %s sets agg, which only applies to a value metric", m.Name, side)
				}
			}
		}
	}

	sort.Strings(p)
	if len(p) > 0 {
		return &ValidationError{Problems: p}
	}
	return nil
}

// problems checks one definition on its own.
func (m Metric) problems(at string) []string {
	var p []string
	add := func(format string, args ...any) { p = append(p, at+": "+fmt.Sprintf(format, args...)) }

	if !kinds[m.Kind] {
		add("kind must be counter, duration, value or ratio, got %q", m.Kind)
	}
	if strings.TrimSpace(m.Title) == "" || len(m.Title) > maxTitleLength {
		add("title is required, at most %d characters", maxTitleLength)
	}
	if len(m.Description) > maxDescriptionLength {
		add("description must be at most %d characters", maxDescriptionLength)
	}
	switch {
	case !kinds[m.Kind]:
		// The kind is already reported; its unit can't be judged.
	case !units[m.Unit]:
		add("unit must be one of 1, By, %%, ms, got %q", m.Unit)
	case m.Kind == KindDuration && m.Unit != "ms":
		add("a duration's unit is ms")
	case m.Kind == KindValue && m.Unit == "ms":
		add("a value's unit is 1, By or %%; time a duration instead")
	}
	if !directions[m.Direction] {
		add("direction must be up_is_good, down_is_good or neutral, got %q", m.Direction)
	}
	if m.MinTier != "" && !tiers[m.MinTier] {
		add("min_tier must be anonymous, basic, full or optin_plus, got %q", m.MinTier)
	}

	if len(m.Dims) > MaxDims {
		add("at most %d dims, got %d", MaxDims, len(m.Dims))
	}
	seen := map[string]bool{}
	for _, d := range m.Dims {
		if !identifier.MatchString(d) {
			add("dim %q must match ^[a-z0-9][a-z0-9._-]*$ and be at most 64 characters", d)
		}
		if seen[d] {
			add("dim %q is listed twice", d)
		}
		seen[d] = true
	}
	for d, values := range m.DimOrder {
		if !seen[d] {
			add("dim_order has %q, which isn't in dims", d)
		}
		for _, v := range values {
			if !identifier.MatchString(v) {
				add("dim_order[%s] value %q is not a valid dimension value", d, v)
			}
		}
	}

	if m.Kind == KindRatio {
		if len(m.Dims) > 0 || len(m.DimOrder) > 0 {
			add("a ratio has no dims of its own")
		}
		if math.IsNaN(m.Scale) || math.IsInf(m.Scale, 0) || m.Scale <= 0 {
			add("scale must be a positive number")
		}
		for side, o := range map[string]*Operand{"numerator": m.Numerator, "denominator": m.Denominator} {
			if o == nil {
				add("%s is required", side)
				continue
			}
			p = append(p, o.problems(at+": "+side)...)
		}
	} else if m.Numerator != nil || m.Denominator != nil || m.Scale != 0 {
		add("numerator, denominator and scale only apply to a ratio")
	}
	return p
}

func (o Operand) problems(at string) []string {
	var p []string
	add := func(format string, args ...any) { p = append(p, at+": "+fmt.Sprintf(format, args...)) }
	switch {
	case (o.Metric == "") == (o.Builtin == ""):
		add("set exactly one of metric and builtin")
	case o.Builtin != "" && !builtins[o.Builtin]:
		add("builtin must be command_invocations, sessions or active_installs, got %q", o.Builtin)
	case o.Builtin != "":
		for k := range o.Where {
			if o.Builtin != BuiltinCommandInvocations || k != "command" {
				add("builtin %s can't be filtered on %q (only command_invocations, on command)", o.Builtin, k)
			}
		}
		if o.Agg != "" {
			add("agg only applies to a value metric")
		}
	}
	if o.Agg != "" && o.Agg != AggSum && o.Agg != AggCount {
		add("agg must be sum or count, got %q", o.Agg)
	}
	for k, v := range o.Where {
		if o.Builtin != "" && k == "command" {
			if strings.TrimSpace(v) == "" {
				add("where command must not be empty")
			}
			continue
		}
		if !identifier.MatchString(k) || !identifier.MatchString(v) {
			add("where %s=%s: key and value must be dimension identifiers", k, v)
		}
	}
	return p
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// IsValidation reports whether err is a *ValidationError (an invalid file)
// rather than an I/O or parse failure.
func IsValidation(err error) bool {
	var v *ValidationError
	return errors.As(err, &v)
}
