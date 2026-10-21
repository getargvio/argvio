package storage

import "testing"

func TestEmptySessionLengths(t *testing.T) {
	got := emptySessionLengths()
	if got.GapSeconds != 1800 || got.SessionCount != 0 || got.MedianSeconds != nil {
		t.Fatalf("header = %+v, want a 1800s gap and no sessions", got)
	}
	type bounds struct {
		label    string
		min, max float64
		open     bool // no upper bound
	}
	want := []bounds{
		{"0", 0, 0, false},
		{"<1m", 0, 60, false},
		{"1-5m", 60, 300, false},
		{"5-15m", 300, 900, false},
		{"15-30m", 900, 1800, false},
		{"30m+", 1800, 0, true},
	}
	if len(got.Buckets) != len(want) {
		t.Fatalf("got %d buckets, want %d", len(got.Buckets), len(want))
	}
	for i, w := range want {
		b := got.Buckets[i]
		if b.Label != w.label || b.MinSeconds != w.min || b.Sessions != 0 {
			t.Errorf("bucket %d = %+v, want label %q from %v", i, b, w.label, w.min)
		}
		if w.open != (b.MaxSeconds == nil) || (!w.open && *b.MaxSeconds != w.max) {
			t.Errorf("bucket %q max = %v, want %v (open=%v)", w.label, b.MaxSeconds, w.max, w.open)
		}
	}
}
