package catalog

import (
	"strings"
	"testing"
)

const vulnapi = `
version: 1
service: vulnapi
metrics:
  - name: vulns.found
    kind: counter
    title: Vulnerabilities found
    description: Findings reported by a scan, after deduplication.
    dims: [severity, check]
    direction: up_is_good
    dim_order:
      severity: [critical, high, medium, low, info]
  - name: scan.findings
    kind: value
    title: Findings per scan
    dims: [severity]
  - name: scan.operations
    kind: value
    title: Operations scanned per run
  - name: scan.run
    kind: duration
    title: Scan time
    direction: down_is_good
  - name: vulns.per_100_ops
    kind: ratio
    title: Vulnerabilities per 100 operations
    numerator: { metric: vulns.found, where: { severity: critical } }
    denominator: { metric: scan.operations, agg: sum }
    scale: 100
  - name: vulns.per_scan
    kind: ratio
    title: Vulnerabilities per scan
    numerator: { metric: vulns.found }
    denominator: { builtin: command_invocations, where: { command: scan } }
`

func TestParseValidFile(t *testing.T) {
	f, err := Parse([]byte(vulnapi))
	if err != nil {
		t.Fatal(err)
	}
	if f.Service != "vulnapi" || len(f.Metrics) != 6 {
		t.Fatalf("parsed %+v", f)
	}
	byName := map[string]Metric{}
	for _, m := range f.Metrics {
		byName[m.Name] = m
	}
	if m := byName["vulns.found"]; m.Unit != "1" || m.Direction != DirectionUpIsGood || len(m.DimOrder["severity"]) != 5 {
		t.Errorf("vulns.found = %+v", m)
	}
	if m := byName["scan.run"]; m.Unit != "ms" {
		t.Errorf("a duration defaults to ms, got %q", m.Unit)
	}
	if m := byName["scan.findings"]; m.Direction != DirectionNeutral || m.Unit != "1" {
		t.Errorf("a value defaults to unit 1 and a neutral direction, got %+v", m)
	}
	if m := byName["vulns.per_scan"]; m.Scale != 1 || m.Unit != "1" || m.Denominator.Builtin != BuiltinCommandInvocations {
		t.Errorf("vulns.per_scan = %+v", m)
	}
}

func TestParseRejects(t *testing.T) {
	header := "version: 1\nservice: vulnapi\nmetrics:\n"
	for _, c := range []struct {
		name, body, want string
	}{
		{"unknown field", "  - {name: a, kind: counter, title: A, colour: red}", "field colour not found"},
		{"bad version", "", "version must be 1"},
		{"bad kind", "  - {name: a, kind: gauge, title: A}", "kind must be"},
		{"missing title", "  - {name: a, kind: counter}", "title is required"},
		{"bad name", "  - {name: A/b, kind: counter, title: A}", "name must match"},
		{"duplicate", "  - {name: a, kind: counter, title: A}\n  - {name: a, kind: counter, title: B}", "defined twice"},
		{"bad unit", "  - {name: a, kind: counter, title: A, unit: kg}", "unit must be"},
		{"duration unit", "  - {name: a, kind: duration, title: A, unit: \"1\"}", "a duration's unit is ms"},
		{"value in ms", "  - {name: a, kind: value, title: A, unit: ms}", "time a duration instead"},
		{"too many dims", "  - {name: a, kind: counter, title: A, dims: [a, b, c, d, e, f, g, h, i]}", "at most 8 dims"},
		{"repeated dim", "  - {name: a, kind: counter, title: A, dims: [a, a]}", "listed twice"},
		{"dim_order outside dims", "  - {name: a, kind: counter, title: A, dims: [a], dim_order: {b: [x]}}", "isn't in dims"},
		{"bad direction", "  - {name: a, kind: counter, title: A, direction: sideways}", "direction must be"},
		{"bad tier", "  - {name: a, kind: counter, title: A, min_tier: secret}", "min_tier must be"},
		{"ratio fields on a counter", "  - {name: a, kind: counter, title: A, scale: 2}", "only apply to a ratio"},
		{"ratio without sides", "  - {name: r, kind: ratio, title: R}", "numerator is required"},
		{"ratio to missing metric", "  - {name: r, kind: ratio, title: R, numerator: {metric: x}, denominator: {builtin: sessions}}", "doesn't define"},
		{"ratio of ratio", "  - {name: a, kind: counter, title: A}\n  - {name: r1, kind: ratio, title: R, numerator: {metric: a}, denominator: {builtin: sessions}}\n  - {name: r2, kind: ratio, title: R, numerator: {metric: r1}, denominator: {builtin: sessions}}", "can only use counters"},
		{"operand with both", "  - {name: a, kind: counter, title: A}\n  - {name: r, kind: ratio, title: R, numerator: {metric: a, builtin: sessions}, denominator: {builtin: sessions}}", "exactly one of metric and builtin"},
		{"bad builtin", "  - {name: r, kind: ratio, title: R, numerator: {builtin: users}, denominator: {builtin: sessions}}", "builtin must be"},
		{"builtin where", "  - {name: r, kind: ratio, title: R, numerator: {builtin: sessions, where: {command: x}}, denominator: {builtin: sessions}}", "can't be filtered"},
		{"where on undeclared dim", "  - {name: a, kind: counter, title: A, dims: [s]}\n  - {name: r, kind: ratio, title: R, numerator: {metric: a, where: {t: x}}, denominator: {builtin: sessions}}", "doesn't declare in dims"},
		{"agg on counter", "  - {name: a, kind: counter, title: A}\n  - {name: r, kind: ratio, title: R, numerator: {metric: a, agg: sum}, denominator: {builtin: sessions}}", "only applies to a value metric"},
		{"negative scale", "  - {name: a, kind: counter, title: A}\n  - {name: r, kind: ratio, title: R, numerator: {metric: a}, denominator: {builtin: sessions}, scale: -1}", "scale must be a positive"},
		{"ratio with dims", "  - {name: a, kind: counter, title: A}\n  - {name: r, kind: ratio, title: R, dims: [x], numerator: {metric: a}, denominator: {builtin: sessions}}", "no dims of its own"},
	} {
		t.Run(c.name, func(t *testing.T) {
			data := header + c.body
			if c.name == "bad version" {
				data = "version: 2\nservice: x\nmetrics: []\n"
			}
			_, err := Parse([]byte(data))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestValidationListsEveryProblem(t *testing.T) {
	_, err := Parse([]byte("version: 1\nservice: \"bad name!\"\nmetrics:\n  - {name: a, kind: gauge}\n"))
	if !IsValidation(err) {
		t.Fatalf("want a validation error, got %v", err)
	}
	problems := err.(*ValidationError).Problems
	if len(problems) != 3 {
		t.Errorf("problems = %q, want service, kind and title", problems)
	}
	if _, err := Parse([]byte("{not yaml")); err == nil || IsValidation(err) {
		t.Errorf("a parse failure is not a validation error: %v", err)
	}
}
