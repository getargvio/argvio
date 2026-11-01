package storage

import (
	"maps"
	"strings"
	"testing"
)

func TestFormatAndParseDims(t *testing.T) {
	for _, c := range []struct {
		dims  map[string]string
		label string
	}{
		{map[string]string{}, ""},
		{map[string]string{"severity": "high"}, "severity=high"},
		{map[string]string{"severity": "high", "check": "bola", "label": "x"}, "check=bola;label=x;severity=high"},
	} {
		if got := FormatDims(c.dims); got != c.label {
			t.Errorf("FormatDims(%v) = %q, want %q", c.dims, got, c.label)
		}
		if got := ParseDims(c.label); !maps.Equal(got, c.dims) {
			t.Errorf("ParseDims(%q) = %v, want %v", c.label, got, c.dims)
		}
	}
	if got := ParseDims("bearer"); !maps.Equal(got, map[string]string{DimLabelKey: "bearer"}) {
		t.Errorf("a plain label should read as the label dimension, got %v", got)
	}
}

func TestDimValue(t *testing.T) {
	for _, c := range []struct {
		label, key, want string
		ok               bool
	}{
		{"check=bola;severity=high", "severity", "high", true},
		{"check=bola;severity=high", "check", "bola", true},
		{"check=bola;severity=high", "target", "", false},
		{"bearer", "label", "bearer", true},
		{"bearer", "severity", "", false},
		{"other", "severity", "other", true},
		{"", "severity", "", false},
	} {
		got, ok := DimValue(c.label, c.key)
		if got != c.want || ok != c.ok {
			t.Errorf("DimValue(%q, %q) = %q, %v; want %q, %v", c.label, c.key, got, ok, c.want, c.ok)
		}
	}
}

func TestDimQueryValidate(t *testing.T) {
	valid := []DimQuery{
		{},
		{GroupBy: []string{"severity"}},
		{GroupBy: []string{"severity", "check"}, Where: map[string][]string{"target": {"openapi", "graphql"}}},
	}
	for _, dq := range valid {
		if err := dq.Validate(); err != nil {
			t.Errorf("%+v: %v", dq, err)
		}
	}
	for _, c := range []struct {
		dq      DimQuery
		wantErr string
	}{
		{DimQuery{GroupBy: []string{"a", "b", "c"}}, "at most 2"},
		{DimQuery{GroupBy: []string{"Sev"}}, "not a valid dimension key"},
		{DimQuery{GroupBy: []string{"a", "a"}}, "repeated"},
		{DimQuery{Where: map[string][]string{"a;b": {"x"}}}, "not a valid dimension key"},
		{DimQuery{Where: map[string][]string{"a": {"x=y"}}}, "not a valid dimension value"},
	} {
		if err := c.dq.Validate(); err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%+v: err = %v, want %q", c.dq, err, c.wantErr)
		}
	}
}
