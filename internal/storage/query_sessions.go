package storage

import (
	"context"
	"fmt"
	"time"
)

// Queries over install sessions: bursts of cli.command.completed events from
// one cli.install_id. The server keeps no per-install command history, so
// sessions are derived from raw logs at query time and are bounded by log
// retention. See docs/journey-telemetry-design.md "Sessions".

// SessionGap is the inactivity that ends a session: a gap between two
// consecutive events of one install that is longer than this starts a new
// session. A gap of exactly SessionGap does not.
const SessionGap = 30 * time.Minute

// MaxSequenceLength caps how many commands of a session make up its sequence;
// later commands are dropped, so long sessions group by how they began.
const MaxSequenceLength = 5

// sessionLengthEdges are the boundaries, in seconds, between the non-zero
// session length buckets: <1m, 1-5m, 5-15m, 15-30m, 30m+. A session of length
// zero (one command, or several at the same instant) is its own bucket.
var sessionLengthEdges = []float64{60, 300, 900, 1800}

// sessionLengthLabels name the buckets in order: zero, then one per range
// delimited by sessionLengthEdges.
var sessionLengthLabels = []string{"0", "<1m", "1-5m", "5-15m", "15-30m", "30m+"}

// sessionEventPredicates selects the tenant's install-attributed
// cli.command.completed events in f.Time with the command, version, os, arch
// and is_ci filters. Events without a cli.install_id (anonymous tier) can't
// be assigned to an install and are left out. The exit_code and attr filters
// don't apply.
func sessionEventPredicates(scope TenantScope, f Filters) *predicateBuilder {
	pb := newScopedPredicates(scope, "tenant_id")
	pb.add("event_name = $%d", eventCommandCompleted)
	pb.add("time >= $%d", f.Time.From)
	pb.add("time < $%d", f.Time.To)
	pb.clauses = append(pb.clauses, "log_attributes ? 'cli.install_id'")
	pb.addIfAnyStr("command_name", f.CommandNames)
	pb.addIfAnyStr("cli_version", f.CLIVersions)
	pb.addIfAnyStr("os", f.OSes)
	pb.addIfAnyStr("arch", f.Arches)
	pb.addIfNotNilBool("is_ci", f.IsCI)
	return pb
}

// sessionizedCTE is the shared head of both queries: every selected event
// numbered into its install's sessions, as the CTE `numbered(install_id,
// session_no, time, command_name)`. An event starts a new session when the
// previous event of the same install is more than gapPh seconds earlier; the
// first event of an install has no predecessor and so stays in session 0.
func sessionizedCTE(where, gapPh string) string {
	return fmt.Sprintf(`
events AS (
	SELECT log_attributes ->> 'cli.install_id' AS install_id, time, command_name, id
	FROM logs
	WHERE %[1]s
),
gapped AS (
	SELECT install_id, time, command_name, id,
	       CASE WHEN time - lag(time) OVER w > make_interval(secs => %[2]s) THEN 1 ELSE 0 END AS starts_session
	FROM events
	WINDOW w AS (PARTITION BY install_id ORDER BY time, command_name, id)
),
numbered AS (
	SELECT install_id, time, command_name, id,
	       sum(starts_session) OVER (PARTITION BY install_id ORDER BY time, command_name, id) AS session_no
	FROM gapped
)`, where, gapPh)
}

// SessionLengthBucket is one range of session lengths. MaxSeconds is the
// exclusive upper bound, nil for the open-ended last bucket; the zero bucket
// has MinSeconds and MaxSeconds both 0.
type SessionLengthBucket struct {
	Label      string   `json:"label"`
	MinSeconds float64  `json:"min_seconds"`
	MaxSeconds *float64 `json:"max_seconds"`
	Sessions   int64    `json:"sessions"`
}

// SessionLengths is the response of SessionLengthDistribution. MedianSeconds
// is nil when there are no sessions.
type SessionLengths struct {
	GapSeconds    float64               `json:"gap_seconds"`
	SessionCount  int64                 `json:"session_count"`
	MedianSeconds *float64              `json:"median_seconds"`
	Buckets       []SessionLengthBucket `json:"buckets"`
}

// emptySessionLengths is the response for no sessions: every bucket, in
// order, with zero sessions.
func emptySessionLengths() SessionLengths {
	out := SessionLengths{GapSeconds: SessionGap.Seconds(), Buckets: make([]SessionLengthBucket, len(sessionLengthLabels))}
	for i, label := range sessionLengthLabels {
		b := SessionLengthBucket{Label: label}
		if i == 0 {
			b.MaxSeconds = new(float64)
		} else {
			if i > 1 {
				b.MinSeconds = sessionLengthEdges[i-2]
			}
			if i-1 < len(sessionLengthEdges) {
				upper := sessionLengthEdges[i-1]
				b.MaxSeconds = &upper
			}
		}
		out.Buckets[i] = b
	}
	return out
}

// SessionLengthDistribution buckets install sessions by length (last event
// time minus first) and gives the session count and median length. Sessions
// are built from the selected events only, and the window clips them: one
// running across f.Time.From or To is counted by the part inside. f.Limit and
// f.Offset don't apply, as the result is a fixed-size summary.
func (q *QueryBuilder) SessionLengthDistribution(ctx context.Context, scope TenantScope, f Filters) (SessionLengths, error) {
	pb := sessionEventPredicates(scope, f)
	gapPh := pb.nextPlaceholder(SessionGap.Seconds())
	edgesPh := pb.nextPlaceholder(sessionLengthEdges)

	sql := fmt.Sprintf(`
WITH %s,
sessions AS (
	SELECT extract(epoch FROM max(time) - min(time))::float8 AS length_s
	FROM numbered
	GROUP BY install_id, session_no
),
bucketed AS (
	SELECT length_s, CASE WHEN length_s = 0 THEN 0 ELSE 1 + width_bucket(length_s, %s::float8[]) END AS bucket_idx
	FROM sessions
)
SELECT bucket_idx, count(*), percentile_cont(0.5) WITHIN GROUP (ORDER BY length_s)
FROM bucketed
GROUP BY GROUPING SETS ((bucket_idx), ())`, sessionizedCTE(pb.where(), gapPh), edgesPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return SessionLengths{}, fmt.Errorf("storage: session lengths: %w", err)
	}
	defer rows.Close()

	out := emptySessionLengths()
	for rows.Next() {
		var idx *int32
		var n int64
		var median *float64
		if err := rows.Scan(&idx, &n, &median); err != nil {
			return SessionLengths{}, fmt.Errorf("storage: scan session lengths: %w", err)
		}
		if idx == nil { // the () grouping set: totals over all sessions
			out.SessionCount, out.MedianSeconds = n, median
			continue
		}
		if int(*idx) >= len(out.Buckets) {
			return SessionLengths{}, fmt.Errorf("storage: session length bucket %d out of range", *idx)
		}
		out.Buckets[*idx].Sessions = n
	}
	if err := rows.Err(); err != nil {
		return SessionLengths{}, fmt.Errorf("storage: session lengths: %w", err)
	}
	return out, nil
}

// SessionSequence is one ordered command sequence and how many sessions ran
// it.
type SessionSequence struct {
	Commands []string `json:"commands"`
	Sessions int64    `json:"sessions"`
}

// SessionSequences returns the most frequent command sequences across install
// sessions, most sessions first. A session's sequence is its commands in
// order with consecutive repeats collapsed (deploy, deploy, status is
// deploy, status), cut to the first MaxSequenceLength commands. Events with
// no command name are left out of the sequence (they still count towards the
// session). minLength drops sequences shorter than that, so single-command
// sessions, which dominate any ranking, can be set aside; it counts commands
// after collapsing and cutting. f.Limit and f.Offset page the ranking.
func (q *QueryBuilder) SessionSequences(ctx context.Context, scope TenantScope, f Filters, minLength int) ([]SessionSequence, error) {
	pb := sessionEventPredicates(scope, f)
	gapPh := pb.nextPlaceholder(SessionGap.Seconds())
	maxLenPh := pb.nextPlaceholder(int32(MaxSequenceLength))
	minLenPh := pb.nextPlaceholder(max(minLength, 1))
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))
	offsetPh := pb.nextPlaceholder(f.Offset)

	sql := fmt.Sprintf(`
WITH %s,
steps AS (
	SELECT install_id, session_no, command_name,
	       lag(command_name) OVER w AS prev_command,
	       row_number() OVER w AS pos
	FROM numbered
	WHERE command_name IS NOT NULL
	WINDOW w AS (PARTITION BY install_id, session_no ORDER BY time, command_name, id)
),
collapsed AS (
	SELECT install_id, session_no, command_name,
	       row_number() OVER (PARTITION BY install_id, session_no ORDER BY pos) AS step
	FROM steps
	WHERE prev_command IS DISTINCT FROM command_name
),
sequences AS (
	SELECT array_agg(command_name ORDER BY step) AS commands
	FROM collapsed
	WHERE step <= %s
	GROUP BY install_id, session_no
)
SELECT commands, count(*) AS sessions
FROM sequences
WHERE cardinality(commands) >= %s
GROUP BY commands
ORDER BY sessions DESC, commands ASC
LIMIT %s OFFSET %s`, sessionizedCTE(pb.where(), gapPh), maxLenPh, minLenPh, limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: session sequences: %w", err)
	}
	defer rows.Close()

	out := []SessionSequence{}
	for rows.Next() {
		var r SessionSequence
		if err := rows.Scan(&r.Commands, &r.Sessions); err != nil {
			return nil, fmt.Errorf("storage: scan session sequences: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
