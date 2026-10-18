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

func TestPopSessionFootprint(t *testing.T) {
	i64 := func(n int64) *int64 { return &n }
	tests := []struct {
		name        string
		in          map[string]any
		wantSize    *int64
		wantStartup *int64
	}{
		{"both reported", map[string]any{"cli.binary.size_bytes": int64(18874368), "cli.startup.duration_ms": int64(42)}, i64(18874368), i64(42)},
		{"missing is not reported", map[string]any{}, nil, nil},
		{"zero size is not reported", map[string]any{"cli.binary.size_bytes": int64(0)}, nil, nil},
		{"zero startup is a measurement", map[string]any{"cli.startup.duration_ms": int64(0)}, nil, i64(0)},
		{"negative is not reported", map[string]any{"cli.binary.size_bytes": int64(-1), "cli.startup.duration_ms": int64(-1)}, nil, nil},
		{"upper bound is inclusive", map[string]any{"cli.binary.size_bytes": maxPlausibleBinarySizeBytes, "cli.startup.duration_ms": maxPlausibleStartupMs}, i64(maxPlausibleBinarySizeBytes), i64(maxPlausibleStartupMs)},
		{"above upper bound is not reported", map[string]any{"cli.binary.size_bytes": maxPlausibleBinarySizeBytes + 1, "cli.startup.duration_ms": maxPlausibleStartupMs + 1}, nil, nil},
		{"saturated int64 is not reported", map[string]any{"cli.binary.size_bytes": int64(1<<63 - 1), "cli.startup.duration_ms": int64(1<<63 - 1)}, nil, nil},
		{"wrong type is not reported", map[string]any{"cli.binary.size_bytes": "big", "cli.startup.duration_ms": 1.5}, nil, nil},
		{"measures are independent", map[string]any{"cli.binary.size_bytes": int64(-1), "cli.startup.duration_ms": int64(7)}, nil, i64(7)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := popSessionFootprint(tt.in)
			if !eqInt64(got.BinarySizeBytes, tt.wantSize) {
				t.Errorf("BinarySizeBytes = %v, want %v", deref(got.BinarySizeBytes), deref(tt.wantSize))
			}
			if !eqInt64(got.StartupMs, tt.wantStartup) {
				t.Errorf("StartupMs = %v, want %v", deref(got.StartupMs), deref(tt.wantStartup))
			}
			for _, k := range []string{"cli.binary.size_bytes", "cli.startup.duration_ms"} {
				if _, left := tt.in[k]; left {
					t.Errorf("%s should be removed from kept, so it is not also stored in log_attributes", k)
				}
			}
		})
	}
}
