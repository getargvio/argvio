package otlp

// Plausibility bounds for the process resource-usage attributes. Ingest
// otherwise only saturates integers to the column width (clamp.go), which
// would let one faulty client report 2^63 bytes and drag every percentile of
// its command/version bucket to the top. Values outside (0, max] are not
// believable measurements, so they are stored as NULL — "not reported" — and
// the rest of the span is kept.
const (
	// maxPlausiblePeakRSSBytes is 1 TiB, above any single process's resident
	// set on hardware CLIs run on.
	maxPlausiblePeakRSSBytes int64 = 1 << 40
	// maxPlausibleCPUTimeMs is 31 days of wall-clock on 1024 cores, far
	// beyond any CLI invocation but still inside int64.
	maxPlausibleCPUTimeMs int64 = 31 * 24 * 3600 * 1000 * 1024
)

// usage holds the resource-usage columns promoted on traces. nil means the
// client did not report the measure or reported an implausible value.
type usage struct {
	PeakRSSBytes *int64
	CPUTimeMs    *int64
}

// popUsage extracts and removes the resource-usage attributes from kept, so
// they live only in their promoted columns and not also in span_attributes.
func popUsage(kept map[string]any) usage {
	return usage{
		PeakRSSBytes: popBounded(kept, "cli.process.peak_rss_bytes", maxPlausiblePeakRSSBytes),
		CPUTimeMs:    popBounded(kept, "cli.process.cpu_time_ms", maxPlausibleCPUTimeMs),
	}
}

// popBounded removes k from m and returns its value if it is an integer in
// (0, max]; zero, negative and oversized values come back nil.
func popBounded(m map[string]any, k string, max int64) *int64 {
	v, ok := m[k]
	if !ok {
		return nil
	}
	delete(m, k)
	n, isInt := v.(int64)
	if !isInt || n <= 0 || n > max {
		return nil
	}
	return &n
}
