package storage

import (
	"context"
	"fmt"
	"time"
)

// Retry and repeated-failure signals per command, derived from raw
// cli.command.completed logs. The server keeps no per-install command
// history, so results are bounded by log retention. See
// docs/journey-telemetry-design.md "Retries and repeated failures".

// RetryWindow is the default window in which a run of a command counts as a
// retry of the install's failed run before it.
const RetryWindow = 10 * time.Minute

// MaxRetryWindow bounds the window a caller may ask for.
const MaxRetryWindow = time.Hour

// RepeatedFailureCount is how many failed completions of one command by one
// install within the window make a repeated failure.
const RepeatedFailureCount = 3

// RetryPoint is the retry picture of one command. Only commands with at least
// one failed completion in the window are listed.
type RetryPoint struct {
	CommandName *string `json:"command_name"`
	// FailedCount is the failed (non-zero exit) completions of the command.
	FailedCount int64 `json:"failed_count"`
	// RetriedCount is how many of them were followed by another run of the
	// same command by the same install within the window.
	RetriedCount int64 `json:"retried_count"`
	// RetryRate is RetriedCount / FailedCount.
	RetryRate float64 `json:"retry_rate"`
	// RetrySequences counts runs of a failure and the retries that follow it.
	RetrySequences int64 `json:"retry_sequences"`
	// RecoveredSequences is how many of those ended in a success.
	RecoveredSequences int64 `json:"recovered_sequences"`
	// RepeatedFailureInstalls is the installs with RepeatedFailureCount
	// failures of the command within the window.
	RepeatedFailureInstalls int64 `json:"repeated_failure_installs"`
	// LongestFailureStreak is the most failures in a row of the command by
	// one install, each within the window of the one before.
	LongestFailureStreak int64 `json:"longest_failure_streak"`
}

// Retries reports, per command, how often a failed run was retried by the same
// install within window, how many retry sequences recovered, and how many
// installs failed the command repeatedly. Within one (install, command) the
// runs are ordered by time:
//
//   - a failure is retried when the next run is at most window after it,
//     whatever its outcome;
//   - a chain is a failure and the runs that follow it, each at most window
//     after a failed run before it; a success ends the chain, a gap longer
//     than window (or a failure with no later run) does too;
//   - a retry sequence is a chain of two or more runs that holds a failure,
//     and it recovered when it ends in a success;
//   - the failure streak is the failures in a chain;
//   - an install failed repeatedly when RepeatedFailureCount of its
//     consecutive failures lie within window of each other, first to last.
//
// Only completions with a cli.install_id (basic consent tier or above), a
// command name and an exit code are seen. The command, cli_version, os, arch
// and is_ci filters pick the runs the chains are built from, so a filter
// that drops some runs can hide a retry; exit_code and attr filters don't
// apply. f.Limit and f.Offset page the commands, most failures first.
func (q *QueryBuilder) Retries(ctx context.Context, scope TenantScope, f Filters, window time.Duration) ([]RetryPoint, error) {
	if window <= 0 || window > MaxRetryWindow {
		return nil, fmt.Errorf("storage: retries: window %s out of range", window)
	}
	pb := sessionEventPredicates(scope, f)
	windowPh := pb.nextPlaceholder(window.Seconds())
	countPh := pb.nextPlaceholder(int32(RepeatedFailureCount))
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))
	offsetPh := pb.nextPlaceholder(f.Offset)

	sql := fmt.Sprintf(`
WITH events AS (
	SELECT log_attributes ->> 'cli.install_id' AS install_id, command_name, time, id, exit_code <> 0 AS failed
	FROM logs
	WHERE %[1]s AND command_name IS NOT NULL AND exit_code IS NOT NULL
),
linked AS (
	SELECT install_id, command_name, time, id, failed,
	       lead(time) OVER w AS next_time,
	       CASE WHEN lag(failed) OVER w AND time - lag(time) OVER w <= make_interval(secs => %[2]s) THEN 0 ELSE 1 END AS starts_chain
	FROM events
	WINDOW w AS (PARTITION BY install_id, command_name ORDER BY time, id)
),
chained AS (
	SELECT install_id, command_name, time, id, failed, next_time,
	       sum(starts_chain) OVER (PARTITION BY install_id, command_name ORDER BY time, id) AS chain_no
	FROM linked
),
flagged AS (
	SELECT install_id, command_name, chain_no, failed,
	       failed AND next_time - time <= make_interval(secs => %[2]s) AS retried,
	       failed AND lead(time, %[3]s - 1) OVER (PARTITION BY install_id, command_name, chain_no, failed ORDER BY time, id)
	                  - time <= make_interval(secs => %[2]s) AS repeated
	FROM chained
),
chains AS (
	SELECT install_id, command_name,
	       count(*) FILTER (WHERE failed) AS failures,
	       count(*) FILTER (WHERE retried) AS retried,
	       count(*) AS runs,
	       bool_or(NOT failed) AS recovered,
	       bool_or(repeated) AS repeated
	FROM flagged
	GROUP BY install_id, command_name, chain_no
	HAVING bool_or(failed)
)
SELECT command_name,
       sum(failures)::bigint AS failed_count,
       sum(retried)::bigint AS retried_count,
       count(*) FILTER (WHERE runs >= 2) AS retry_sequences,
       count(*) FILTER (WHERE recovered) AS recovered_sequences,
       count(DISTINCT install_id) FILTER (WHERE repeated) AS repeated_failure_installs,
       max(failures) AS longest_failure_streak
FROM chains
GROUP BY command_name
ORDER BY failed_count DESC, command_name ASC
LIMIT %[4]s OFFSET %[5]s`, pb.where(), windowPh, countPh, limitPh, offsetPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: retries: %w", err)
	}
	defer rows.Close()

	out := []RetryPoint{}
	for rows.Next() {
		var r RetryPoint
		if err := rows.Scan(&r.CommandName, &r.FailedCount, &r.RetriedCount, &r.RetrySequences,
			&r.RecoveredSequences, &r.RepeatedFailureInstalls, &r.LongestFailureStreak); err != nil {
			return nil, fmt.Errorf("storage: scan retries: %w", err)
		}
		if r.FailedCount > 0 {
			r.RetryRate = float64(r.RetriedCount) / float64(r.FailedCount)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
