package storage

import (
	"math"
	"os"
	"slices"
	"testing"

	"github.com/getargvio/argvio/internal/allowlist"
)

// The rollups and the percentile estimate assume the buckets the allowlist
// makes ingest enforce.
func TestMeasureBoundsMatchAllowlist(t *testing.T) {
	data, err := os.ReadFile("../../schema/allowlist/v1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := allowlist.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := s.MetricByName("cli.measure")
	if !ok {
		t.Fatal("cli.measure not defined")
	}
	if !slices.Equal(m.ExplicitBounds, MeasureBounds) {
		t.Errorf("allowlist bounds %v != storage.MeasureBounds %v", m.ExplicitBounds, MeasureBounds)
	}
	if len(MeasureBounds)+1 != MeasureBucketCount {
		t.Errorf("%d bounds make %d buckets, not %d", len(MeasureBounds), len(MeasureBounds)+1, MeasureBucketCount)
	}
}

func TestMeasureQuantile(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	near := func(got *float64, want float64) bool { return got != nil && math.Abs(*got-want) < 1e-9 }
	var none [MeasureBucketCount]int64

	if got := measureQuantile(none, nil, nil, 0.5); got != nil {
		t.Errorf("no observations: %v, want nil", *got)
	}

	// 100 observations in (100, 500]: interpolated across the bucket.
	var one [MeasureBucketCount]int64
	one[5] = 100
	for _, c := range []struct{ q, want float64 }{{0.5, 300}, {0.95, 480}, {1, 500}} {
		if got := measureQuantile(one, nil, nil, c.q); !near(got, c.want) {
			t.Errorf("q=%v: %v, want %v", c.q, got, c.want)
		}
	}
	// Clamped to what was observed.
	if got := measureQuantile(one, f(150), f(200), 0.99); !near(got, 200) {
		t.Errorf("clamped to max: %v, want 200", got)
	}
	if got := measureQuantile(one, f(450), f(480), 0.01); !near(got, 450) {
		t.Errorf("clamped to min: %v, want 450", got)
	}

	// 50 fast and 50 slow: the median is the edge of the fast bucket, p99 is
	// deep in the slow one.
	var two [MeasureBucketCount]int64
	two[2], two[8] = 50, 50 // (5, 10] and (5000, 10000]
	if got := measureQuantile(two, nil, nil, 0.5); !near(got, 10) {
		t.Errorf("median = %v, want 10", got)
	}
	if got := measureQuantile(two, nil, nil, 0.99); !near(got, 5000+5000*0.98) {
		t.Errorf("p99 = %v, want %v", got, 5000+5000*0.98)
	}

	// The open-ended edges: the first bucket starts at the minimum (or 0), the
	// last ends at the maximum (or its lower bound).
	var low, high [MeasureBucketCount]int64
	low[0], high[11] = 10, 10
	if got := measureQuantile(low, nil, nil, 0.5); !near(got, 0.5) {
		t.Errorf("first bucket, no min: %v, want 0.5", got)
	}
	if got := measureQuantile(low, f(0.2), f(1), 0.5); !near(got, 0.6) {
		t.Errorf("first bucket, min 0.2: %v, want 0.6", got)
	}
	if got := measureQuantile(high, nil, nil, 0.5); !near(got, 300000) {
		t.Errorf("last bucket, no max: %v, want 300000", got)
	}
	if got := measureQuantile(high, f(400000), f(900000), 0.5); !near(got, 600000) {
		t.Errorf("last bucket, max 900000: %v, want 600000", got)
	}
}
