package otlp

import "testing"

func TestPopUsage(t *testing.T) {
	i64 := func(n int64) *int64 { return &n }
	tests := []struct {
		name    string
		in      map[string]any
		wantRSS *int64
		wantCPU *int64
	}{
		{"both reported", map[string]any{"cli.process.peak_rss_bytes": int64(52428800), "cli.process.cpu_time_ms": int64(310)}, i64(52428800), i64(310)},
		{"missing is not reported", map[string]any{}, nil, nil},
		{"zero is not reported", map[string]any{"cli.process.peak_rss_bytes": int64(0), "cli.process.cpu_time_ms": int64(0)}, nil, nil},
		{"negative is not reported", map[string]any{"cli.process.peak_rss_bytes": int64(-1), "cli.process.cpu_time_ms": int64(-5)}, nil, nil},
		{"upper bound is inclusive", map[string]any{"cli.process.peak_rss_bytes": maxPlausiblePeakRSSBytes, "cli.process.cpu_time_ms": maxPlausibleCPUTimeMs}, i64(maxPlausiblePeakRSSBytes), i64(maxPlausibleCPUTimeMs)},
		{"above upper bound is not reported", map[string]any{"cli.process.peak_rss_bytes": maxPlausiblePeakRSSBytes + 1, "cli.process.cpu_time_ms": maxPlausibleCPUTimeMs + 1}, nil, nil},
		{"saturated int64 is not reported", map[string]any{"cli.process.peak_rss_bytes": int64(1<<63 - 1)}, nil, nil},
		{"wrong type is not reported", map[string]any{"cli.process.peak_rss_bytes": "lots"}, nil, nil},
		{"measures are independent", map[string]any{"cli.process.peak_rss_bytes": int64(-1), "cli.process.cpu_time_ms": int64(42)}, nil, i64(42)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := popUsage(tt.in)
			if !eqInt64(got.PeakRSSBytes, tt.wantRSS) {
				t.Errorf("PeakRSSBytes = %v, want %v", deref(got.PeakRSSBytes), deref(tt.wantRSS))
			}
			if !eqInt64(got.CPUTimeMs, tt.wantCPU) {
				t.Errorf("CPUTimeMs = %v, want %v", deref(got.CPUTimeMs), deref(tt.wantCPU))
			}
			for _, k := range []string{"cli.process.peak_rss_bytes", "cli.process.cpu_time_ms"} {
				if _, left := tt.in[k]; left {
					t.Errorf("%s should be removed from kept, so it is not also stored in span_attributes", k)
				}
			}
		})
	}
}

func eqInt64(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}
