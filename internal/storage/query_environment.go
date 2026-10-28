package storage

import (
	"context"
	"fmt"
	"strings"
)

// Distributions of the environment CLIs run in (shell, terminal, TTY, runtime,
// config customization, auth state), read from cli.session.started logs. See
// docs/journey-telemetry-design.md "Session environment".

// EnvironmentDimension names one session-level fact a distribution can be
// taken over.
type EnvironmentDimension string

const (
	EnvShell               EnvironmentDimension = "shell"
	EnvTerminal            EnvironmentDimension = "terminal"
	EnvStdoutTTY           EnvironmentDimension = "stdout_tty"
	EnvRuntime             EnvironmentDimension = "runtime"
	EnvRuntimeVersion      EnvironmentDimension = "runtime_version"
	EnvConfigCustomization EnvironmentDimension = "config_customization"
	EnvAuthState           EnvironmentDimension = "auth_state"
)

// EnvironmentDimensions lists every dimension, in the order the API documents
// them.
var EnvironmentDimensions = []EnvironmentDimension{
	EnvShell, EnvTerminal, EnvStdoutTTY, EnvRuntime, EnvRuntimeVersion, EnvConfigCustomization, EnvAuthState,
}

// Value labels of the stdout_tty dimension. config_customization buckets the
// number of settings that differ from the defaults (0, 1-2, 3-5, 6+), so a raw
// count never reaches a result.
const (
	EnvTTY   = "tty"
	EnvPiped = "piped"
)

// envText is the string value of a session attribute, NULL when absent or empty.
func envText(key string) string {
	return fmt.Sprintf("NULLIF(log_attributes ->> '%s', '')", key)
}

// environmentValueSQL maps each dimension to the expression yielding its value
// for one cli.session.started row, or NULL when the session did not report it.
// They are constants, never built from request input.
var environmentValueSQL = map[EnvironmentDimension]string{
	EnvShell:    envText("cli.shell"),
	EnvTerminal: envText("cli.terminal"),
	EnvStdoutTTY: `CASE log_attributes -> 'cli.stdout.is_tty'
	WHEN 'true'::jsonb THEN '` + EnvTTY + `'
	WHEN 'false'::jsonb THEN '` + EnvPiped + `'
END`,
	EnvRuntime: envText("cli.runtime.name"),
	// A version means nothing without its runtime (1.22 of what?), so a
	// session reporting only one of the two did not report this.
	EnvRuntimeVersion: `CASE WHEN ` + envText("cli.runtime.name") + ` IS NOT NULL AND ` + envText("cli.runtime.version") + ` IS NOT NULL
	THEN ` + envText("cli.runtime.name") + ` || ' ' || ` + envText("cli.runtime.version") + `
END`,
	// A negative count is not a believable measurement: not reported.
	EnvConfigCustomization: `CASE WHEN jsonb_typeof(log_attributes -> 'cli.config.customized_count') = 'number' THEN
	CASE WHEN (log_attributes ->> 'cli.config.customized_count')::numeric < 0 THEN NULL
	     WHEN (log_attributes ->> 'cli.config.customized_count')::numeric = 0 THEN '0'
	     WHEN (log_attributes ->> 'cli.config.customized_count')::numeric <= 2 THEN '1-2'
	     WHEN (log_attributes ->> 'cli.config.customized_count')::numeric <= 5 THEN '3-5'
	     ELSE '6+'
	END
END`,
	EnvAuthState: envText("cli.auth.state"),
}

// ParseEnvironmentDimension resolves the API's name for a dimension.
func ParseEnvironmentDimension(s string) (EnvironmentDimension, bool) {
	d := EnvironmentDimension(s)
	_, ok := environmentValueSQL[d]
	return d, ok
}

// EnvironmentDimensionNames is the dimensions as a comma-separated list, for
// error messages.
func EnvironmentDimensionNames() string {
	names := make([]string, len(EnvironmentDimensions))
	for i, d := range EnvironmentDimensions {
		names[i] = string(d)
	}
	return strings.Join(names, ", ")
}

// EnvironmentValue is one value of a dimension and the sessions that reported it.
type EnvironmentValue struct {
	Value        string `json:"value"`
	SessionCount int64  `json:"session_count"`
}

// EnvironmentOther is everything past the top-K: how many distinct values were
// folded together, and the sessions behind them. Both are 0 when nothing was.
type EnvironmentOther struct {
	ValueCount   int64 `json:"value_count"`
	SessionCount int64 `json:"session_count"`
}

// EnvironmentDistribution is the response of SessionEnvironment. SessionCount
// is every session in scope; ReportedSessions those that reported the
// dimension, so SessionCount - ReportedSessions did not (anonymous tier, an
// older SDK, or a CLI that does not know the fact). Values and Other add up to
// ReportedSessions.
type EnvironmentDistribution struct {
	Dimension        EnvironmentDimension `json:"dimension"`
	SessionCount     int64                `json:"session_count"`
	ReportedSessions int64                `json:"reported_sessions"`
	Values           []EnvironmentValue   `json:"values"`
	Other            EnvironmentOther     `json:"other"`
}

// SessionEnvironment counts the cli.session.started sessions in f.Time per
// value of dim, most sessions first (ties by value), keeping the top f.Limit
// values and folding the rest into Other. Free-text dimensions (terminal,
// shell, runtime) are client-influenced, so the cap is what keeps one buggy or
// hostile client from making the response unbounded. f.Offset does not apply.
//
// Sessions are read from raw logs, so results are bounded by log retention.
// Only basic-tier-or-above sessions carry these attributes; the others count in
// SessionCount only (see InstallCoverage). The cli_version, os, arch and is_ci
// filters apply; command, exit_code and attr filters don't, a session start
// having no command.
func (q *QueryBuilder) SessionEnvironment(ctx context.Context, scope TenantScope, f Filters, dim EnvironmentDimension) (EnvironmentDistribution, error) {
	valueSQL, ok := environmentValueSQL[dim]
	if !ok {
		return EnvironmentDistribution{}, fmt.Errorf("storage: unknown environment dimension %q", dim)
	}

	pb := newScopedPredicates(scope, "tenant_id")
	pb.add("event_name = $%d", eventSessionStarted)
	pb.add("time >= $%d", f.Time.From)
	pb.add("time < $%d", f.Time.To)
	pb.addIfAnyStr("cli_version", f.CLIVersions)
	pb.addIfAnyStr("os", f.OSes)
	pb.addIfAnyStr("arch", f.Arches)
	pb.addIfNotNilBool("is_ci", f.IsCI)
	limitPh := pb.nextPlaceholder(clampLimit(f.Limit))

	// One scan: s is referenced more than once, so it is materialized. The
	// kind column tells the three kinds of row apart; the last column is the
	// rank for a value, so ordering by it keeps the ranking.
	sql := fmt.Sprintf(`
WITH s AS (
	SELECT %[1]s AS value
	FROM logs
	WHERE %[2]s
),
ranked AS (
	SELECT value, count(*) AS n, row_number() OVER (ORDER BY count(*) DESC, value ASC) AS rn
	FROM s
	WHERE value IS NOT NULL
	GROUP BY value
)
SELECT 'total' AS kind, NULL::text AS value, count(*)::bigint AS n, count(value)::bigint AS rn FROM s
UNION ALL
SELECT 'value', value, n, rn FROM ranked WHERE rn <= %[3]s::bigint
UNION ALL
SELECT 'other', NULL, coalesce(sum(n), 0)::bigint, count(*)::bigint FROM ranked WHERE rn > %[3]s::bigint
ORDER BY 1, 4`, valueSQL, pb.where(), limitPh)

	rows, err := q.pool.Query(ctx, sql, pb.args...)
	if err != nil {
		return EnvironmentDistribution{}, fmt.Errorf("storage: session environment: %w", err)
	}
	defer rows.Close()

	out := EnvironmentDistribution{Dimension: dim, Values: []EnvironmentValue{}}
	for rows.Next() {
		var (
			kind  string
			value *string
			n, rn int64
		)
		if err := rows.Scan(&kind, &value, &n, &rn); err != nil {
			return EnvironmentDistribution{}, fmt.Errorf("storage: scan session environment: %w", err)
		}
		switch kind {
		case "total":
			out.SessionCount, out.ReportedSessions = n, rn
		case "value":
			out.Values = append(out.Values, EnvironmentValue{Value: *value, SessionCount: n})
		case "other":
			out.Other = EnvironmentOther{ValueCount: rn, SessionCount: n}
		}
	}
	if err := rows.Err(); err != nil {
		return EnvironmentDistribution{}, fmt.Errorf("storage: session environment: %w", err)
	}
	return out, nil
}
