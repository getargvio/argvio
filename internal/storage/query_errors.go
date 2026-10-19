package storage

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/getargvio/argvio/internal/errfp"
)

// TierRanks orders consent tier names as the allowlist schema ranks them
// (higher is more detailed). Storage has no schema dependency, so a query
// that checks tiers is handed the ranking by its caller.
type TierRanks map[string]int

// ErrorSampleMaxLen caps, in characters, the normalized message and top
// frame returned for a fingerprint.
const ErrorSampleMaxLen = 512

// ErrorFingerprintPoint is one bucket of a fingerprint's trend.
type ErrorFingerprintPoint struct {
	Bucket           time.Time `json:"bucket"`
	EventCount       int64     `json:"event_count"`
	AffectedInstalls int64     `json:"affected_installs"`
}

// ErrorFingerprint is one failure cause: the cli.error.raised events that
// share a fingerprint (internal/errfp). Message and TopFrame are normalized
// from the most recent of those events, never returned raw, and are nil
// when it had none. Commands and Versions are every one seen, Versions
// oldest first.
type ErrorFingerprint struct {
	Fingerprint      string                  `json:"fingerprint"`
	Source           string                  `json:"source"` // stack | message
	Message          *string                 `json:"message"`
	TopFrame         *string                 `json:"top_frame"`
	EventCount       int64                   `json:"event_count"`
	AffectedInstalls int64                   `json:"affected_installs"`
	FirstSeen        time.Time               `json:"first_seen"`
	LastSeen         time.Time               `json:"last_seen"`
	Commands         []string                `json:"commands"`
	Versions         []string                `json:"versions"`
	Trend            []ErrorFingerprintPoint `json:"trend"`
}

// ErrorFingerprints ranks failure causes in f.Time by distinct affected
// installs, then event count, and returns page f.Limit/f.Offset of them, each
// with a trend bucketed by bucket (buckets without events are omitted).
//
// Messages and stacks are the most sensitive data the API reads, so an event
// only counts if its recorded consent_tier is at or below the tenant's
// *current* tier_ceiling, read from tenant_config in the same statement.
// Ingest already strips detail above the ceiling at the time, but a ceiling
// lowered since then must hide what was stored under the old one. A tenant
// with no config row, or a ceiling missing from tiers, sees nothing.
//
// Only events with a stored fingerprint count: the SDKs send messages and
// stacks at the full tier and above, so this undercounts relative to
// ErrorRateSeries and ErrorTypes. Reads raw logs (no rollup), so it is
// bounded by log retention. Command, version, os, arch and is_ci filters
// apply.
func (q *QueryBuilder) ErrorFingerprints(ctx context.Context, scope TenantScope, f Filters, bucket Bucket, tiers TierRanks) ([]ErrorFingerprint, error) {
	bucketExpr, err := logBucketExpr(bucket)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(tiers))
	ranks := make([]int32, 0, len(tiers))
	for name, rank := range tiers {
		names = append(names, name)
		ranks = append(ranks, int32(rank)) // #nosec G115 -- schema tier ranks are small
	}

	pb := newScopedPredicates(scope, "tenant_id")
	pb.add("event_name = $%d", errfp.EventName)
	pb.add("time >= $%d", f.Time.From)
	pb.add("time < $%d", f.Time.To)
	pb.addIfAnyStr("command_name", f.CommandNames)
	pb.addIfAnyStr("cli_version", f.CLIVersions)
	pb.addIfAnyStr("os", f.OSes)
	pb.addIfAnyStr("arch", f.Arches)
	pb.addIfNotNilBool("is_ci", f.IsCI)
	tenantPh := pb.nextPlaceholder(scope.tenantID)
	namesPh := pb.nextPlaceholder(names)
	ranksPh := pb.nextPlaceholder(ranks)
	msgPh := pb.nextPlaceholder(errfp.AttrMessage)
	stackPh := pb.nextPlaceholder(errfp.AttrStacktrace)
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))
	offsetPh := pb.nextPlaceholder(f.Offset)

	sql := fmt.Sprintf(`
WITH tier(name, rank) AS (
	SELECT * FROM unnest(%[3]s::text[], %[4]s::int[])
),
readable AS (
	SELECT t.name
	FROM tenant_config tc
	JOIN tier ceiling ON ceiling.name = tc.tier_ceiling
	JOIN tier t ON t.rank <= ceiling.rank
	WHERE tc.tenant_id = %[2]s
),
ev AS (
	SELECT time, error_fingerprint AS fp, command_name, cli_version,
	       log_attributes ->> 'cli.install_id' AS install_id,
	       log_attributes ->> %[5]s AS message,
	       log_attributes ->> %[6]s AS stack
	FROM logs
	WHERE %[1]s AND error_fingerprint IS NOT NULL
	  AND consent_tier IN (SELECT name FROM readable)
),
page AS (
	SELECT fp, count(*) AS event_count, count(DISTINCT install_id) AS affected_installs,
	       min(time) AS first_seen, max(time) AS last_seen,
	       coalesce(array_agg(DISTINCT command_name) FILTER (WHERE command_name IS NOT NULL), '{}') AS commands,
	       coalesce(array_agg(DISTINCT cli_version) FILTER (WHERE cli_version IS NOT NULL), '{}') AS versions
	FROM ev
	GROUP BY fp
	ORDER BY affected_installs DESC, event_count DESC, fp ASC
	LIMIT %[8]s OFFSET %[9]s
),
trend AS (
	SELECT fp, array_agg(bucket ORDER BY bucket) AS buckets,
	       array_agg(n ORDER BY bucket) AS counts, array_agg(i ORDER BY bucket) AS installs
	FROM (
		SELECT ev.fp, %[7]s AS bucket, count(*) AS n, count(DISTINCT install_id) AS i
		FROM ev JOIN page USING (fp)
		GROUP BY 1, 2
	) b
	GROUP BY fp
),
sample AS (
	SELECT DISTINCT ON (ev.fp) ev.fp, ev.message, ev.stack
	FROM ev JOIN page USING (fp)
	ORDER BY ev.fp, ev.time DESC
)
SELECT p.fp, p.event_count, p.affected_installs, p.first_seen, p.last_seen, p.commands, p.versions,
       t.buckets, t.counts, t.installs, s.message, s.stack
FROM page p
JOIN trend t USING (fp)
JOIN sample s USING (fp)
ORDER BY p.affected_installs DESC, p.event_count DESC, p.fp ASC`,
		pb.where(), tenantPh, namesPh, ranksPh, msgPh, stackPh, bucketExpr, limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: error fingerprints: %w", err)
	}
	defer rows.Close()

	out := []ErrorFingerprint{}
	for rows.Next() {
		var (
			r              ErrorFingerprint
			buckets        []time.Time
			counts, inst   []int64
			message, stack *string
		)
		if err := rows.Scan(&r.Fingerprint, &r.EventCount, &r.AffectedInstalls, &r.FirstSeen, &r.LastSeen,
			&r.Commands, &r.Versions, &buckets, &counts, &inst, &message, &stack); err != nil {
			return nil, fmt.Errorf("storage: scan error fingerprints: %w", err)
		}
		slices.Sort(r.Commands)
		slices.SortFunc(r.Versions, func(a, b string) int { return compareVersionKeys(&a, &b) })
		r.Trend = make([]ErrorFingerprintPoint, len(buckets))
		for i := range buckets {
			r.Trend[i] = ErrorFingerprintPoint{Bucket: buckets[i], EventCount: counts[i], AffectedInstalls: inst[i]}
		}
		r.Source, r.Message, r.TopFrame = errorSample(message, stack)
		out = append(out, r)
	}
	return out, rows.Err()
}

// errorSample normalizes one event's raw message and stack into the
// representative shown for its fingerprint, truncated to ErrorSampleMaxLen.
// The raw text is only ever held here, never returned.
func errorSample(message, stack *string) (source string, msg, topFrame *string) {
	var m, s string
	if message != nil {
		m = *message
	}
	if stack != nil {
		s = *stack
	}
	res, _ := errfp.Compute(m, s)
	nonEmpty := func(v string) *string {
		if v == "" {
			return nil
		}
		v = errfp.Truncate(v, ErrorSampleMaxLen)
		return &v
	}
	return res.Source, nonEmpty(res.Message), nonEmpty(res.TopFrame)
}
