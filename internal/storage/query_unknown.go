package storage

import (
	"context"
	"fmt"
	"time"
)

// Attempts to run a command that does not exist, and the known command the
// user probably meant, from cli.command.unknown logs. See
// docs/journey-telemetry-design.md "Unknown commands".

const (
	eventCommandUnknown = "cli.command.unknown"

	attrCommandSuggestion = "cli.command.suggestion"
)

// Row kinds of the suggestions query, which returns three kinds of row in one
// result set.
const (
	unknownRowTotal    = "total"
	unknownRowDistinct = "distinct"
	unknownRowValue    = "value"
)

// UnknownCommandPoint is one bucket of the attempt rate. AttemptRate is
// AttemptCount / InvocationCount, 0 when there were no invocations. An attempt
// is not an invocation, so the rate is not bounded by 1.
type UnknownCommandPoint struct {
	Bucket          time.Time `json:"bucket"`
	AttemptCount    int64     `json:"attempt_count"`
	InvocationCount int64     `json:"invocation_count"`
	AttemptRate     float64   `json:"attempt_rate"`
}

// UnknownCommandSuggestion is one known command and the attempts it was
// suggested for.
type UnknownCommandSuggestion struct {
	Suggestion   string `json:"suggestion"`
	AttemptCount int64  `json:"attempt_count"`
}

// UnknownCommandOther is every suggestion that is not listed: those that are
// not a command the tenant has reported, and known ones past the top-K. Both
// are 0 when nothing was folded.
type UnknownCommandOther struct {
	SuggestionCount int64 `json:"suggestion_count"`
	AttemptCount    int64 `json:"attempt_count"`
}

// UnknownCommandSuggestions is the suggestion half of the response.
// AttemptCount is every attempt in scope; SuggestedAttempts those that carried
// a suggestion, so the difference carried none (anonymous tier, or no command
// was close enough). Values and Other add up to SuggestedAttempts.
type UnknownCommandSuggestions struct {
	AttemptCount      int64                      `json:"attempt_count"`
	SuggestedAttempts int64                      `json:"suggested_attempts"`
	Values            []UnknownCommandSuggestion `json:"values"`
	Other             UnknownCommandOther        `json:"other"`
}

// UnknownCommands is the response of UnknownCommands.
type UnknownCommands struct {
	Series      []UnknownCommandPoint     `json:"series"`
	Suggestions UnknownCommandSuggestions `json:"suggestions"`
}

// UnknownCommands reports, per bucket, the cli.command.unknown attempts of the
// tenant next to the command invocations of the same bucket (the command
// stats rollups), and the top f.Limit suggestions, most attempts first (ties by
// name).
//
// A suggestion is client-supplied and the server cannot tell a command name
// from typed text, so it is only listed when it equals a command_name the
// tenant has reported (the set behind ListDimensions: the daily command-stats
// rollup, with no time filter). Every other suggestion, and the known ones
// past the top-K, are folded into Other, so typed text that a misbehaving
// client put there is counted but never returned. A command reported for the
// first time in the last hour or so is not in the rollup yet and folds too.
//
// Attempts are read from raw logs, so results are bounded by log retention.
// The cli_version, os, arch and is_ci filters apply to both sides; command
// does not (an attempt has no command, and narrowing only the invocations
// would skew the rate), exit_code and attr filters don't either. f.Offset does
// not apply. The series lists every bucket that has attempts or invocations.
func (q *QueryBuilder) UnknownCommands(ctx context.Context, scope TenantScope, f Filters, bucket Bucket) (UnknownCommands, error) {
	series, err := q.unknownCommandSeries(ctx, scope, f, bucket)
	if err != nil {
		return UnknownCommands{}, err
	}
	suggestions, err := q.unknownCommandSuggestions(ctx, scope, f)
	if err != nil {
		return UnknownCommands{}, err
	}
	return UnknownCommands{Series: series, Suggestions: suggestions}, nil
}

func (q *QueryBuilder) unknownCommandSeries(ctx context.Context, scope TenantScope, f Filters, bucket Bucket) ([]UnknownCommandPoint, error) {
	table, invBucket, err := commandStatsSource(bucket)
	if err != nil {
		return nil, err
	}
	attBucket, err := bucketOf(bucket, "time")
	if err != nil {
		return nil, err
	}

	// The dimension filters have the same column names in logs and in the
	// rollup, so one set serves both sides; only the time column differs.
	pb := newScopedPredicates(scope, "tenant_id")
	pb.addIfAnyStr("cli_version", f.CLIVersions)
	pb.addIfAnyStr("os", f.OSes)
	pb.addIfAnyStr("arch", f.Arches)
	pb.addIfNotNilBool("is_ci", f.IsCI)
	eventPh := pb.nextPlaceholder(eventCommandUnknown)
	fromPh := pb.nextPlaceholder(f.Time.From)
	toPh := pb.nextPlaceholder(f.Time.To)

	sql := fmt.Sprintf(`
WITH attempts AS (
	SELECT %[1]s AS bucket, count(*)::bigint AS n
	FROM logs
	WHERE %[3]s AND event_name = %[4]s AND time >= %[5]s AND time < %[6]s
	GROUP BY 1
),
invocations AS (
	SELECT %[2]s AS bucket, sum(invocation_count)::bigint AS n
	FROM %[7]s
	WHERE %[3]s AND bucket >= %[5]s AND bucket < %[6]s
	GROUP BY 1
)
SELECT coalesce(a.bucket, i.bucket) AS bucket, coalesce(a.n, 0), coalesce(i.n, 0)
FROM attempts a
FULL JOIN invocations i ON i.bucket = a.bucket
ORDER BY 1 ASC`, attBucket, invBucket, pb.where(), eventPh, fromPh, toPh, table)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("storage: unknown commands: %w", err)
	}
	defer rows.Close()

	out := []UnknownCommandPoint{}
	for rows.Next() {
		var p UnknownCommandPoint
		if err := rows.Scan(&p.Bucket, &p.AttemptCount, &p.InvocationCount); err != nil {
			return nil, fmt.Errorf("storage: scan unknown commands: %w", err)
		}
		if p.InvocationCount > 0 {
			p.AttemptRate = float64(p.AttemptCount) / float64(p.InvocationCount)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (q *QueryBuilder) unknownCommandSuggestions(ctx context.Context, scope TenantScope, f Filters) (UnknownCommandSuggestions, error) {
	pb := newScopedPredicates(scope, "tenant_id")
	pb.add("event_name = $%d", eventCommandUnknown)
	pb.add("time >= $%d", f.Time.From)
	pb.add("time < $%d", f.Time.To)
	pb.addIfAnyStr("cli_version", f.CLIVersions)
	pb.addIfAnyStr("os", f.OSes)
	pb.addIfAnyStr("arch", f.Arches)
	pb.addIfNotNilBool("is_ci", f.IsCI)
	scopePh := pb.nextPlaceholder(scope.tenantID)
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))

	// One scan of the attempts, materialized as it is read more than once.
	// known is the tenant's reported command names. The kind column tells
	// the rows apart; the last column is the rank of a listed suggestion.
	sql := fmt.Sprintf(`
WITH a AS (
	SELECT NULLIF(log_attributes ->> '%[1]s', '') AS suggestion
	FROM logs
	WHERE %[2]s
),
known AS (
	SELECT DISTINCT command_name AS name
	FROM %[3]s
	WHERE tenant_id = %[4]s AND command_name IS NOT NULL
),
ranked AS (
	SELECT a.suggestion AS value, count(*) AS n, row_number() OVER (ORDER BY count(*) DESC, a.suggestion ASC) AS rn
	FROM a
	JOIN known ON known.name = a.suggestion
	GROUP BY a.suggestion
)
SELECT '%[6]s' AS kind, NULL::text AS value, count(*)::bigint AS n, count(suggestion)::bigint AS rn FROM a
UNION ALL
SELECT '%[7]s', NULL, count(DISTINCT suggestion)::bigint, 0 FROM a
UNION ALL
SELECT '%[8]s', value, n, rn FROM ranked WHERE rn <= %[5]s::bigint
ORDER BY 1, 4`, attrCommandSuggestion, pb.where(), caggCommandStatsDaily, scopePh, limitPh,
		unknownRowTotal, unknownRowDistinct, unknownRowValue)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return UnknownCommandSuggestions{}, fmt.Errorf("storage: unknown command suggestions: %w", err)
	}
	defer rows.Close()

	out := UnknownCommandSuggestions{Values: []UnknownCommandSuggestion{}}
	var distinct, listed int64
	for rows.Next() {
		var (
			kind  string
			value *string
			n, rn int64
		)
		if err := rows.Scan(&kind, &value, &n, &rn); err != nil {
			return UnknownCommandSuggestions{}, fmt.Errorf("storage: scan unknown command suggestions: %w", err)
		}
		switch kind {
		case unknownRowTotal:
			out.AttemptCount, out.SuggestedAttempts = n, rn
		case unknownRowDistinct:
			distinct = n
		case unknownRowValue:
			out.Values = append(out.Values, UnknownCommandSuggestion{Suggestion: *value, AttemptCount: n})
			listed += n
		}
	}
	if err := rows.Err(); err != nil {
		return UnknownCommandSuggestions{}, fmt.Errorf("storage: unknown command suggestions: %w", err)
	}
	out.Other = UnknownCommandOther{
		SuggestionCount: distinct - int64(len(out.Values)),
		AttemptCount:    out.SuggestedAttempts - listed,
	}
	return out, nil
}
