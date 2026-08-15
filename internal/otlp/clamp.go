package otlp

import "math"

// clampInt16/clampInt32/clampInt64 narrow a wider integer to a storage
// column's width by saturating at the bounds instead of silently wrapping
// (e.g. a large positive value becoming negative). The values narrowed here
// (severity numbers, span kind/status enums, exit codes, metric counts) all
// originate on the OTLP/HTTP or OTLP/gRPC ingest path, i.e. from an
// untrusted client — see docs/architecture.md.

func clampInt16(n int32) int16 {
	switch {
	case n > math.MaxInt16:
		return math.MaxInt16
	case n < math.MinInt16:
		return math.MinInt16
	default:
		return int16(n)
	}
}

func clampInt32(n int64) int32 {
	switch {
	case n > math.MaxInt32:
		return math.MaxInt32
	case n < math.MinInt32:
		return math.MinInt32
	default:
		return int32(n)
	}
}

func clampInt64(n uint64) int64 {
	if n > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(n)
}
