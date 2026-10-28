package metricsapi

import (
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
	for _, param := range []string{"attr.cli.ci_provider=github_actions", "attr_contains.a=b", "attr_in.a=b,c"} {
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
