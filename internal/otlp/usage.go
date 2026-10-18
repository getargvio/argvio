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
		PeakRSSBytes: popBounded(kept, "cli.process.peak_rss_bytes", 1, maxPlausiblePeakRSSBytes),
		CPUTimeMs:    popBounded(kept, "cli.process.cpu_time_ms", 1, maxPlausibleCPUTimeMs),
	}
}

// popBounded removes k from m and returns its value if it is an integer in
// [min, max]; anything outside, or not an integer, comes back nil.
func popBounded(m map[string]any, k string, min, max int64) *int64 {
	v, ok := m[k]
	if !ok {
		return nil
	}
	delete(m, k)
	n, isInt := v.(int64)
	if !isInt || n < min || n > max {
		return nil
	}
	return &n
}

// Plausibility bounds for the per-session build attributes, for the same
// reason as above: one faulty client must not skew its version's figures.
const (
	// maxPlausibleBinarySizeBytes is 4 GiB; a CLI executable beyond that is
	// a misreport (a disk size, a sparse file) rather than a build.
	maxPlausibleBinarySizeBytes int64 = 4 << 30
	// maxPlausibleStartupMs is one hour; a CLI that takes longer to begin
	// running than that has hung or measured from the wrong origin.
	maxPlausibleStartupMs int64 = 3600 * 1000
)

// sessionFootprint holds the build-footprint columns promoted on logs. nil
// means the client did not report the measure or reported an implausible
// value.
type sessionFootprint struct {
	BinarySizeBytes *int64
	StartupMs       *int64
}

// popSessionFootprint extracts and removes the binary-size and startup
// attributes from kept, so they live only in their promoted columns and not
// also in log_attributes. The allowlist only admits them on
// cli.session.started, so for any other event this is a no-op.
//
// A binary can't be zero bytes, so zero size is "not reported". Startup of
// zero is a real measurement (under a millisecond), so it is kept.
func popSessionFootprint(kept map[string]any) sessionFootprint {
	return sessionFootprint{
		BinarySizeBytes: popBounded(kept, "cli.binary.size_bytes", 1, maxPlausibleBinarySizeBytes),
		StartupMs:       popBounded(kept, "cli.startup.duration_ms", 0, maxPlausibleStartupMs),
	}
}
