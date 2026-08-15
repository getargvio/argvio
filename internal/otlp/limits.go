// Package otlp is the public server's ingest pipeline: OTLP gRPC + HTTP
// receivers for metrics/logs/traces, Layer 1 structural validation, and the
// glue between internal/allowlist + internal/consent (Layer 2 + tier
// enforcement) and internal/storage (the write path).
//
// Nothing here trusts the client. See docs/architecture.md.
package otlp

import "time"

// Bounds are the Layer 1 structural limits from internal/config.PublicConfig,
// collected here so the conversion code in this package takes one small
// struct instead of the whole config tree.
type Bounds struct {
	MaxBatchSize                  int
	MaxAttributeCount             int
	MaxAttributeKeyLength         int
	MaxAttributeStringValueLength int
	MaxTimestampSkewPast          time.Duration
	MaxTimestampSkewFuture        time.Duration
}

// checkTimestamp reports whether ts is within [now-past, now+future]. A
// violation rejects the individual record it belongs to (span/log
// record/data point), not the whole request — other records in the same
// batch may have perfectly valid timestamps.
func checkTimestamp(ts, now time.Time, past, future time.Duration) bool {
	if ts.Before(now.Add(-past)) {
		return false
	}
	if ts.After(now.Add(future)) {
		return false
	}
	return true
}
