package storage

import (
	"math"
	"os"
	"slices"
	"testing"

	"github.com/getargvio/argvio/internal/allowlist"
)

func TestValueBoundsMatchAllowlist(t *testing.T) {
	data, err := os.ReadFile("../../schema/allowlist/v1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := allowlist.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := s.MetricByName("cli.value")
	if !ok {
		t.Fatal("cli.value not defined")
	}
	if !slices.Equal(m.ExplicitBounds, ValueBounds) || len(ValueBounds)+1 != ValueBucketCount {
		t.Errorf("allowlist bounds %v, storage.ValueBounds %v (%d buckets)", m.ExplicitBounds, ValueBounds, ValueBucketCount)
	}
	unit, _ := s.AttrDefFor("cli.value.unit")
	if !slices.Equal(unit.Enum, ValueUnits) {
		t.Errorf("allowlist units %v != storage.ValueUnits %v", unit.Enum, ValueUnits)
	}
}

func TestValueStats(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	// 10 runs: 4 found nothing, 3 found 1, 2 found 3 or 4, 1 found 40.
	a := valueAgg{count: 10, points: 2, sum: f(51), min: f(0), max: f(40)}
	a.buckets[0], a.buckets[1], a.buckets[3], a.buckets[6] = 4, 3, 2, 1
	s := a.stats()
	if s.HitRate == nil || math.Abs(*s.HitRate-0.6) > 1e-9 {
		t.Errorf("hit rate = %v, want 0.6", s.HitRate)
	}
	if s.Mean == nil || *s.Mean != 5.1 {
		t.Errorf("mean = %v, want 5.1", s.Mean)
	}
	if s.P50 == nil || *s.P50 <= 0 || *s.P50 > 1 {
		t.Errorf("p50 = %v, want within (0, 1]", s.P50)
	}
	if s.P99 == nil || *s.P99 <= 20 || *s.P99 > 40 {
		t.Errorf("p99 = %v, want within (20, 40]", s.P99)
	}
	// A run of all zeros reads as exactly zero.
	z := valueAgg{count: 3, sum: f(0), min: f(0), max: f(0)}
	z.buckets[0] = 3
	if s := z.stats(); s.P50 == nil || *s.P50 != 0 || *s.HitRate != 0 {
		t.Errorf("all zeros = %+v", s)
	}
	if s := (&valueAgg{}).stats(); s.Mean != nil || s.HitRate != nil || s.P50 != nil {
		t.Errorf("no values = %+v, want nil statistics", s)
	}
}
