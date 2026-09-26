package metricsapi

import (
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

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
