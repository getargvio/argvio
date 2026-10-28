package metricsapi

import (
	"maps"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getargvio/argvio/internal/storage"
)

func TestMultiParam(t *testing.T) {
	cases := []struct {
		query string
		want  []string
	}{
		{"", nil},
		{"os=", nil},
		{"os=linux", []string{"linux"}},
		{"os=linux&os=darwin", []string{"linux", "darwin"}},
		{"os=linux,darwin", []string{"linux", "darwin"}},
		{"os=linux,+darwin,&os=windows", []string{"linux", "darwin", "windows"}},
	}
	for _, c := range cases {
		q, err := url.ParseQuery(c.query)
		if err != nil {
			t.Fatalf("parse %q: %v", c.query, err)
		}
		if got := multiParam(q, "os"); !slices.Equal(got, c.want) {
			t.Errorf("multiParam(%q) = %v, want %v", c.query, got, c.want)
		}
	}
}

func TestParseBucket(t *testing.T) {
	cases := []struct {
		query   string
		allowed []storage.Bucket
		want    storage.Bucket
		wantErr bool
	}{
		{"", anyBucket, storage.BucketHour, false},
		{"bucket=week", anyBucket, storage.BucketWeek, false},
		{"bucket=month", dailyBucket, storage.BucketMonth, false},
		{"bucket=hour", dailyBucket, "", true},
		{"bucket=year", anyBucket, "", true},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/?"+c.query, nil)
		got, err := parseBucket(r, storage.BucketHour, c.allowed...)
		if (err != nil) != c.wantErr {
			t.Errorf("parseBucket(%q) err = %v, wantErr %v", c.query, err, c.wantErr)
		}
		if got != c.want {
			t.Errorf("parseBucket(%q) = %q, want %q", c.query, got, c.want)
		}
	}
}

func TestParseFiltersRejectsAttrFilters(t *testing.T) {
	s := &Server{MaxTimeRangeSpan: 24 * time.Hour, DefaultResultPageSize: 10, MaxResultPageSize: 100}
	for _, param := range []string{"attr.cli.ci_provider=github_actions", "attr_contains.a=b", "attr_in.a=b,c", "attr_regex.a=^b"} {
		r := httptest.NewRequest("GET", "/?from=2026-01-01T00:00:00Z&to=2026-01-01T01:00:00Z&"+param, nil)
		_, err := s.parseFilters(r)
		if err == nil {
			t.Errorf("parseFilters(%q) = nil error, want rejection", param)
			continue
		}
		if key, _, _ := strings.Cut(param, "="); !strings.Contains(err.Error(), key) {
			t.Errorf("parseFilters(%q) error %q does not name %q", param, err, key)
		}
	}
}

func TestParseFiltersAttrOperators(t *testing.T) {
	s := &Server{MaxTimeRangeSpan: 24 * time.Hour, DefaultResultPageSize: 10, MaxResultPageSize: 100}
	r := httptest.NewRequest("GET", "/?from=2026-01-01T00:00:00Z&to=2026-01-01T01:00:00Z"+
		"&attr.a.b=x&attr_contains.c.d=git&attr_contains.e=&attr_in.f=p,q&attr_in.f=r&attr_in.g=", nil)
	f, err := s.parseFiltersWithAttrs(r)
	if err != nil {
		t.Fatal(err)
	}
	if f.Attrs["a.b"] != "x" || len(f.Attrs) != 1 {
		t.Errorf("Attrs = %v", f.Attrs)
	}
	if f.AttrContains["c.d"] != "git" || len(f.AttrContains) != 1 {
		t.Errorf("AttrContains = %v (empty needle must be dropped)", f.AttrContains)
	}
	if !slices.Equal(f.AttrIn["f"], []string{"p", "q", "r"}) || len(f.AttrIn) != 1 {
		t.Errorf("AttrIn = %v (empty list must be dropped)", f.AttrIn)
	}
}

func TestParseFiltersAttrRegex(t *testing.T) {
	s := &Server{MaxTimeRangeSpan: 24 * time.Hour, DefaultResultPageSize: 10, MaxResultPageSize: 100}
	base := "/?from=2026-01-01T00:00:00Z&to=2026-01-01T01:00:00Z"
	maxLen := strings.Repeat("a", maxAttrRegexLen)
	// Multi-byte runes count as one character, not their byte length.
	maxRunes := strings.Repeat("é", maxAttrRegexLen)

	cases := []struct {
		name    string
		query   string
		want    map[string]string
		wantErr string
	}{
		{"parsed", "&attr_regex.cli.ci_provider=" + url.QueryEscape("^git(hub|lab)_"), map[string]string{"cli.ci_provider": "^git(hub|lab)_"}, ""},
		{"empty pattern dropped", "&attr_regex.a=", map[string]string{}, ""},
		{"at length limit", "&attr_regex.a=" + maxLen, map[string]string{"a": maxLen}, ""},
		{"at length limit in runes", "&attr_regex.a=" + maxRunes, map[string]string{"a": maxRunes}, ""},
		{"over length limit", "&attr_regex.a=" + maxLen + "a", nil, "attr_regex.a must be at most 128 characters"},
		{"at count limit", "&attr_regex.a=x&attr_regex.b=x&attr_regex.c=x", map[string]string{"a": "x", "b": "x", "c": "x"}, ""},
		{"over count limit", "&attr_regex.a=x&attr_regex.b=x&attr_regex.c=x&attr_regex.d=x", nil, "at most 3 attr_regex.* filters"},
		{"empty patterns do not count", "&attr_regex.a=x&attr_regex.b=x&attr_regex.c=x&attr_regex.d=", map[string]string{"a": "x", "b": "x", "c": "x"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := s.parseFiltersWithAttrs(httptest.NewRequest("GET", base+c.query, nil))
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(f.AttrRegex, c.want) {
				t.Errorf("AttrRegex = %v, want %v", f.AttrRegex, c.want)
			}
		})
	}
}
