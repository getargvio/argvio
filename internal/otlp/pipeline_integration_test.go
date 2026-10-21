package otlp

import (
	"context"
	"log/slog"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/getargvio/argvio/internal/allowlist"
	"github.com/getargvio/argvio/internal/consent"
	"github.com/getargvio/argvio/internal/errfp"
	"github.com/getargvio/argvio/internal/storage"
	"github.com/getargvio/argvio/internal/tenant"
)

func testBounds() Bounds {
	return Bounds{
		MaxBatchSize:                  1000,
		MaxAttributeCount:             64,
		MaxAttributeKeyLength:         128,
		MaxAttributeStringValueLength: 4096,
		MaxTimestampSkewPast:          24 * time.Hour,
		MaxTimestampSkewFuture:        5 * time.Minute,
		MaxErrorReasonsPerTenant:      100,
		MaxCounterNamesPerTenant:      200,
		MaxCounterLabelsPerName:       50,
		MaxMeasureNamesPerTenant:      100,
		MaxMeasureLabelsPerName:       50,
	}
}

func setupPipeline(t *testing.T, tierCeiling string, mode consent.EnforcementMode) (*Pipeline, *pgxpool.Pool, uuid.UUID) {
	t.Helper()
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live otlp pipeline test")
	}
	ctx := context.Background()
	if err := storage.MigrateUp(dsn); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	store := tenant.NewStore(pool)
	ten, err := store.CreateTenant(ctx, "otlp-test-"+time.Now().Format("150405.000000000"), "OTLP Test")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if err := store.UpsertConfig(ctx, tenant.Config{
		TenantID:            ten.ID,
		TierCeiling:         tierCeiling,
		TierEnforcementMode: mode,
	}); err != nil {
		t.Fatalf("UpsertConfig: %v", err)
	}

	data, err := os.ReadFile("../../schema/allowlist/v1.yaml")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	schema, err := allowlist.Parse(data)
	if err != nil {
		t.Fatalf("parse schema: %v", err)
	}

	resolved := &tenant.Resolved{
		Tenant: tenant.Tenant{ID: ten.ID, Slug: ten.Slug, Name: ten.Name, Status: "active"},
		Config: tenant.Config{TenantID: ten.ID, TierCeiling: tierCeiling, TierEnforcementMode: mode},
	}

	pipeline := &Pipeline{
		Schema: schema,
		Tenant: resolved,
		Bounds: testBounds(),
		Writer: storage.NewWriter(pool),
		Log:    slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
	return pipeline, pool, ten.ID
}

func TestProcessTraces_Live(t *testing.T) {
	pipeline, pool, tenantID := setupPipeline(t, "full", consent.ModeStrip)
	ctx := context.Background()
	now := time.Now()

	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	res := rs.Resource()
	res.Attributes().PutStr("cli.analytics.tier", "anonymous")
	res.Attributes().PutStr("service.name", "test-cli")
	res.Attributes().PutStr("cli.version", "1.0.0")
	res.Attributes().PutStr("cli.os", "linux")
	res.Attributes().PutStr("cli.arch", "amd64")
	res.Attributes().PutBool("cli.is_ci", false)

	ss := rs.ScopeSpans().AppendEmpty()
	ss.Scope().SetName("argvio-test-sdk")

	goodSpan := ss.Spans().AppendEmpty()
	goodSpan.SetName("cli.command.deploy")
	goodSpan.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	goodSpan.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(150 * time.Millisecond)))
	goodSpan.Status().SetCode(ptrace.StatusCodeOk)
	goodSpan.Attributes().PutStr("cli.command.name", "deploy")
	goodSpan.Attributes().PutInt("cli.command.exit_code", 0)

	badSpan := ss.Spans().AppendEmpty()
	badSpan.SetName("cli.totally.unknown")
	badSpan.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	badSpan.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(10 * time.Millisecond)))

	accepted, rejected, sample, err := pipeline.ProcessTraces(ctx, td)
	if err != nil {
		t.Fatalf("ProcessTraces: %v", err)
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("accepted=%d rejected=%d (sample=%q), want 1/1", accepted, rejected, sample)
	}

	var count int
	var commandName, os_, consentTier string
	var exitCode int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM traces WHERE tenant_id=$1`, tenantID).Scan(&count)
	if err != nil || count != 1 {
		t.Fatalf("expected 1 trace row, got count=%d err=%v", count, err)
	}
	err = pool.QueryRow(ctx, `SELECT command_name, os, consent_tier, exit_code FROM traces WHERE tenant_id=$1`, tenantID).
		Scan(&commandName, &os_, &consentTier, &exitCode)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if commandName != "deploy" || os_ != "linux" || consentTier != "anonymous" || exitCode != 0 {
		t.Errorf("unexpected row: command=%q os=%q tier=%q exit=%d", commandName, os_, consentTier, exitCode)
	}
}

func TestProcessTraces_TierStripping_Live(t *testing.T) {
	// Tenant ceiling is "basic" with strip mode; the SDK declares "full" and
	// sends args.raw (optin_plus-only). Expect: record accepted, effective
	// tier capped at "basic", args.raw stripped.
	pipeline, pool, tenantID := setupPipeline(t, "basic", consent.ModeStrip)
	ctx := context.Background()
	now := time.Now()

	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	res := rs.Resource()
	res.Attributes().PutStr("cli.analytics.tier", "full")
	res.Attributes().PutStr("cli.version", "2.0.0")

	ss := rs.ScopeSpans().AppendEmpty()
	span := ss.Spans().AppendEmpty()
	span.SetName("cli.command.push")
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(50 * time.Millisecond)))
	span.Attributes().PutStr("cli.command.name", "push")
	span.Attributes().PutStr("cli.args.raw", "--force --token=SECRET")

	accepted, rejected, _, err := pipeline.ProcessTraces(ctx, td)
	if err != nil {
		t.Fatalf("ProcessTraces: %v", err)
	}
	if accepted != 1 || rejected != 0 {
		t.Fatalf("accepted=%d rejected=%d, want 1/0", accepted, rejected)
	}

	var consentTier string
	var spanAttrs []byte
	err = pool.QueryRow(ctx, `SELECT consent_tier, span_attributes FROM traces WHERE tenant_id=$1 AND command_name='push'`, tenantID).
		Scan(&consentTier, &spanAttrs)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if consentTier != "basic" {
		t.Errorf("consent_tier = %q, want basic (capped by tenant ceiling)", consentTier)
	}
	if strings.Contains(string(spanAttrs), "SECRET") {
		t.Errorf("cli.args.raw should have been stripped at basic tier, but found in span_attributes: %s", spanAttrs)
	}
}

func TestProcessTraces_ResourceUsage_Live(t *testing.T) {
	pipeline, pool, tenantID := setupPipeline(t, "full", consent.ModeStrip)
	ctx := context.Background()
	now := time.Now()

	td := ptrace.NewTraces()
	addSpan := func(tier, command string, rss, cpu *int64) {
		rs := td.ResourceSpans().AppendEmpty()
		rs.Resource().Attributes().PutStr("cli.analytics.tier", tier)
		rs.Resource().Attributes().PutStr("cli.version", "1.0.0")
		span := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
		span.SetName("cli.command." + command)
		id := uuid.New()
		span.SetTraceID(pcommon.TraceID(id))
		span.SetSpanID(pcommon.SpanID(id[:8]))
		span.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
		span.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(20 * time.Millisecond)))
		span.Attributes().PutStr("cli.command.name", command)
		if rss != nil {
			span.Attributes().PutInt("cli.process.peak_rss_bytes", *rss)
		}
		if cpu != nil {
			span.Attributes().PutInt("cli.process.cpu_time_ms", *cpu)
		}
	}
	i64 := func(n int64) *int64 { return &n }

	addSpan("basic", "stored", i64(52428800), i64(310))
	addSpan("anonymous", "stripped", i64(52428800), i64(310))
	addSpan("basic", "negative", i64(-1), i64(-5))
	addSpan("basic", "zero", i64(0), i64(0))
	addSpan("basic", "toolarge", i64(math.MaxInt64), i64(maxPlausibleCPUTimeMs+1))
	addSpan("basic", "omitted", nil, nil)

	accepted, rejected, sample, err := pipeline.ProcessTraces(ctx, td)
	if err != nil {
		t.Fatalf("ProcessTraces: %v", err)
	}
	if accepted != 6 || rejected != 0 {
		t.Fatalf("accepted=%d rejected=%d (sample=%q), want 6/0", accepted, rejected, sample)
	}

	type usageRow struct {
		rss, cpu *int64
		attrs    string
	}
	read := func(command string) usageRow {
		t.Helper()
		var r usageRow
		err := pool.QueryRow(ctx,
			`SELECT peak_rss_bytes, cpu_time_ms, span_attributes::text FROM traces WHERE tenant_id=$1 AND command_name=$2`,
			tenantID, command).Scan(&r.rss, &r.cpu, &r.attrs)
		if err != nil {
			t.Fatalf("read %s: %v", command, err)
		}
		return r
	}

	if r := read("stored"); r.rss == nil || *r.rss != 52428800 || r.cpu == nil || *r.cpu != 310 {
		t.Errorf("basic tier should store usage, got rss=%v cpu=%v", r.rss, r.cpu)
	} else if strings.Contains(r.attrs, "cli.process") {
		t.Errorf("promoted usage should not be duplicated in span_attributes: %s", r.attrs)
	}
	// Both attributes are basic-tier: an anonymous record must not keep them.
	if r := read("stripped"); r.rss != nil || r.cpu != nil || strings.Contains(r.attrs, "cli.process") {
		t.Errorf("anonymous tier should strip usage, got rss=%v cpu=%v attrs=%s", r.rss, r.cpu, r.attrs)
	}
	for _, command := range []string{"negative", "zero", "toolarge", "omitted"} {
		if r := read(command); r.rss != nil || r.cpu != nil {
			t.Errorf("%s: want not reported (NULL), got rss=%v cpu=%v", command, r.rss, r.cpu)
		}
	}
}

func TestProcessMetrics_Live(t *testing.T) {
	pipeline, pool, tenantID := setupPipeline(t, "full", consent.ModeStrip)
	ctx := context.Background()
	now := time.Now()

	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("cli.analytics.tier", "anonymous")
	sm := rm.ScopeMetrics().AppendEmpty()

	sumMetric := sm.Metrics().AppendEmpty()
	sumMetric.SetName("cli.command.invocations")
	sumMetric.SetUnit("1")
	sumMetric.SetEmptySum().SetIsMonotonic(true)
	dp := sumMetric.Sum().DataPoints().AppendEmpty()
	dp.SetTimestamp(pcommon.NewTimestampFromTime(now))
	dp.SetIntValue(1)
	dp.Attributes().PutStr("cli.command.name", "deploy")

	histMetric := sm.Metrics().AppendEmpty()
	histMetric.SetName("cli.command.duration")
	histMetric.SetUnit("ms")
	histMetric.SetEmptyHistogram()
	hdp := histMetric.Histogram().DataPoints().AppendEmpty()
	hdp.SetTimestamp(pcommon.NewTimestampFromTime(now))
	hdp.SetCount(2)
	hdp.SetSum(300)
	hdp.ExplicitBounds().FromRaw([]float64{100, 200})
	hdp.BucketCounts().FromRaw([]uint64{1, 1, 0})

	// Unsupported metric type: should be rejected, not stored.
	summaryMetric := sm.Metrics().AppendEmpty()
	summaryMetric.SetName("cli.command.invocations") // name doesn't matter, type does
	summaryMetric.SetEmptySummary()
	summaryMetric.Summary().DataPoints().AppendEmpty().SetTimestamp(pcommon.NewTimestampFromTime(now))

	accepted, rejected, sample, err := pipeline.ProcessMetrics(ctx, md)
	if err != nil {
		t.Fatalf("ProcessMetrics: %v", err)
	}
	if accepted != 2 || rejected != 1 {
		t.Fatalf("accepted=%d rejected=%d (sample=%q), want 2/1", accepted, rejected, sample)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM metrics WHERE tenant_id=$1`, tenantID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("expected 2 metric rows, got count=%d err=%v", count, err)
	}
}

func TestProcessLogs_Live(t *testing.T) {
	pipeline, pool, tenantID := setupPipeline(t, "full", consent.ModeStrip)
	ctx := context.Background()
	now := time.Now()

	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("cli.analytics.tier", "full")
	sl := rl.ScopeLogs().AppendEmpty()

	rec := sl.LogRecords().AppendEmpty()
	rec.SetEventName("cli.error.raised")
	rec.SetTimestamp(pcommon.NewTimestampFromTime(now))
	rec.Body().SetStr("connection refused")
	rec.Attributes().PutStr("cli.command.name", "sync")
	rec.Attributes().PutStr("cli.error.type", "NetworkError")
	rec.Attributes().PutStr("cli.error.message", "connection refused after 3 retries")

	accepted, rejected, _, err := pipeline.ProcessLogs(ctx, ld)
	if err != nil {
		t.Fatalf("ProcessLogs: %v", err)
	}
	if accepted != 1 || rejected != 0 {
		t.Fatalf("accepted=%d rejected=%d, want 1/0", accepted, rejected)
	}

	var body *string
	var eventName string
	err = pool.QueryRow(ctx, `SELECT event_name, body FROM logs WHERE tenant_id=$1`, tenantID).Scan(&eventName, &body)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if eventName != "cli.error.raised" {
		t.Errorf("event_name = %q", eventName)
	}
	if body == nil || *body != "connection refused" {
		t.Errorf("expected body to be persisted at full tier, got %v", body)
	}
}

func TestProcessLogs_SessionFootprint_Live(t *testing.T) {
	pipeline, pool, tenantID := setupPipeline(t, "full", consent.ModeStrip)
	ctx := context.Background()
	now := time.Now()

	// Each record is labelled by its cli.version (an anonymous-tier column)
	// so it can be found again whatever the tier does to the rest.
	ld := plog.NewLogs()
	addSession := func(tier, label string, size, startup *int64) {
		rl := ld.ResourceLogs().AppendEmpty()
		rl.Resource().Attributes().PutStr("cli.analytics.tier", tier)
		rec := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
		rec.SetEventName("cli.session.started")
		rec.SetTimestamp(pcommon.NewTimestampFromTime(now))
		rec.Attributes().PutStr("cli.version", label)
		if size != nil {
			rec.Attributes().PutInt("cli.binary.size_bytes", *size)
		}
		if startup != nil {
			rec.Attributes().PutInt("cli.startup.duration_ms", *startup)
		}
	}
	i64 := func(n int64) *int64 { return &n }

	addSession("basic", "stored", i64(18874368), i64(42))
	addSession("anonymous", "stripped", i64(18874368), i64(42))
	addSession("basic", "zero", i64(0), i64(0))
	addSession("basic", "negative", i64(-1), i64(-1))
	addSession("basic", "toolarge", i64(math.MaxInt64), i64(maxPlausibleStartupMs+1))
	addSession("basic", "omitted", nil, nil)
	addSession("basic", "startuponly", nil, i64(7))

	accepted, rejected, sample, err := pipeline.ProcessLogs(ctx, ld)
	if err != nil {
		t.Fatalf("ProcessLogs: %v", err)
	}
	if accepted != 7 || rejected != 0 {
		t.Fatalf("accepted=%d rejected=%d (sample=%q), want 7/0", accepted, rejected, sample)
	}

	type footprintRow struct {
		size, startup *int64
		attrs         string
	}
	read := func(label string) footprintRow {
		t.Helper()
		var r footprintRow
		err := pool.QueryRow(ctx,
			`SELECT binary_size_bytes, startup_ms, log_attributes::text FROM logs WHERE tenant_id=$1 AND cli_version=$2`,
			tenantID, label).Scan(&r.size, &r.startup, &r.attrs)
		if err != nil {
			t.Fatalf("read %s: %v", label, err)
		}
		return r
	}
	eq := func(p *int64, want int64) bool { return p != nil && *p == want }

	if r := read("stored"); !eq(r.size, 18874368) || !eq(r.startup, 42) {
		t.Errorf("basic tier should store the footprint, got size=%v startup=%v", r.size, r.startup)
	} else if strings.Contains(r.attrs, "cli.binary") || strings.Contains(r.attrs, "cli.startup") {
		t.Errorf("promoted footprint should not be duplicated in log_attributes: %s", r.attrs)
	}
	// Both attributes are basic-tier: an anonymous record must not keep them.
	if r := read("stripped"); r.size != nil || r.startup != nil || strings.Contains(r.attrs, "cli.binary") || strings.Contains(r.attrs, "cli.startup") {
		t.Errorf("anonymous tier should strip the footprint, got size=%v startup=%v attrs=%s", r.size, r.startup, r.attrs)
	}
	// Zero startup is a real (sub-millisecond) measurement; zero size is not.
	if r := read("zero"); r.size != nil || !eq(r.startup, 0) {
		t.Errorf("zero: want size NULL and startup 0, got size=%v startup=%v", r.size, r.startup)
	}
	for _, label := range []string{"negative", "toolarge", "omitted"} {
		if r := read(label); r.size != nil || r.startup != nil {
			t.Errorf("%s: want not reported (NULL), got size=%v startup=%v", label, r.size, r.startup)
		}
	}
	// Binary size is optional: a runtime with no single binary still reports startup.
	if r := read("startuponly"); r.size != nil || !eq(r.startup, 7) {
		t.Errorf("startuponly: want size NULL and startup 7, got size=%v startup=%v", r.size, r.startup)
	}
}

func TestProcessLogs_ErrorFingerprint_Live(t *testing.T) {
	pipeline, pool, tenantID := setupPipeline(t, "full", consent.ModeStrip)
	ctx := context.Background()
	now := time.Now()

	msg := "open /home/alice/.toolrc: permission denied"
	ld := plog.NewLogs()
	addError := func(tier, label, stack string) {
		rl := ld.ResourceLogs().AppendEmpty()
		rl.Resource().Attributes().PutStr("cli.analytics.tier", tier)
		rec := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
		rec.SetEventName(errfp.EventName)
		rec.SetTimestamp(pcommon.NewTimestampFromTime(now))
		rec.Attributes().PutStr("cli.version", label)
		rec.Attributes().PutStr(errfp.AttrMessage, msg)
		if stack != "" {
			rec.Attributes().PutStr(errfp.AttrStacktrace, stack)
		}
	}
	stack := "main.load()\n\t/home/alice/src/tool/load.go:12 +0x1d"
	addError("full", "message", "")
	addError("full", "stack", stack)
	// Message and stack are full-tier: a basic record loses both, so there
	// is nothing to fingerprint.
	addError("basic", "stripped", stack)

	if accepted, rejected, sample, err := pipeline.ProcessLogs(ctx, ld); err != nil || accepted != 3 || rejected != 0 {
		t.Fatalf("ProcessLogs: accepted=%d rejected=%d sample=%q err=%v, want 3/0", accepted, rejected, sample, err)
	}
	read := func(label string) *string {
		t.Helper()
		var fp *string
		if err := pool.QueryRow(ctx, `SELECT error_fingerprint FROM logs WHERE tenant_id=$1 AND cli_version=$2`, tenantID, label).Scan(&fp); err != nil {
			t.Fatalf("read %s: %v", label, err)
		}
		return fp
	}
	byMessage, _ := errfp.Compute(msg, "")
	byStack, _ := errfp.Compute(msg, stack)
	if fp := read("message"); fp == nil || *fp != byMessage.Fingerprint {
		t.Errorf("message-only error: fingerprint %v, want %s", fp, byMessage.Fingerprint)
	}
	if fp := read("stack"); fp == nil || *fp != byStack.Fingerprint {
		t.Errorf("error with stack: fingerprint %v, want %s", fp, byStack.Fingerprint)
	}
	if fp := read("stripped"); fp != nil {
		t.Errorf("basic-tier error: fingerprint %q, want NULL", *fp)
	}
}

func TestProcessLogs_ErrorReason_Live(t *testing.T) {
	pipeline, pool, tenantID := setupPipeline(t, "basic", consent.ModeStrip)
	pipeline.Bounds.MaxErrorReasonsPerTenant = 2
	ctx := context.Background()
	now := time.Now()

	// Each record is labelled by its cli.version so it can be found again.
	ld := plog.NewLogs()
	addError := func(tier, label, reason string) {
		rl := ld.ResourceLogs().AppendEmpty()
		rl.Resource().Attributes().PutStr("cli.analytics.tier", tier)
		rec := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
		rec.SetEventName("cli.error.raised")
		rec.SetTimestamp(pcommon.NewTimestampFromTime(now))
		rec.Attributes().PutStr("cli.version", label)
		rec.Attributes().PutStr("cli.error.type", "auth")
		if reason != "" {
			rec.Attributes().PutStr("cli.error.reason", reason)
		}
	}
	addError("basic", "valid", "no-token")
	addError("basic", "valid-again", "no-token")
	addError("basic", "second", "expired-token")
	// Third distinct reason with the cap at 2: ties on frequency go by name,
	// so this one, last by name, is folded.
	addError("basic", "over-cap", "wrong-scope")
	addError("basic", "free-text", "could not read /home/alice/.toolrc")
	addError("anonymous", "anonymous", "no-token")
	addError("basic", "old-sdk", "")

	if accepted, rejected, sample, err := pipeline.ProcessLogs(ctx, ld); err != nil || accepted != 7 || rejected != 0 {
		t.Fatalf("ProcessLogs: accepted=%d rejected=%d sample=%q err=%v, want 7/0", accepted, rejected, sample, err)
	}
	read := func(label string) *string {
		t.Helper()
		var reason *string
		if err := pool.QueryRow(ctx, `SELECT log_attributes ->> 'cli.error.reason' FROM logs WHERE tenant_id=$1 AND cli_version=$2`, tenantID, label).Scan(&reason); err != nil {
			t.Fatalf("read %s: %v", label, err)
		}
		return reason
	}
	want := map[string]string{
		"valid": "no-token", "valid-again": "no-token", "second": "expired-token",
		"over-cap": "other",
		// Dropped by the allowlist pattern, below the tier, or never sent.
		"free-text": "", "anonymous": "", "old-sdk": "",
	}
	for label, w := range want {
		got := read(label)
		gotStr := ""
		if got != nil {
			gotStr = *got
		}
		if gotStr != w {
			t.Errorf("%s: reason = %q, want %q", label, gotStr, w)
		}
	}
	var folded int64
	if err := pool.QueryRow(ctx, `SELECT folded_count FROM error_reason_overflow WHERE tenant_id=$1`, tenantID).Scan(&folded); err != nil || folded != 1 {
		t.Errorf("overflow folded_count = %d (err %v), want 1", folded, err)
	}
}

func TestProcessMetrics_ErrorReason_Live(t *testing.T) {
	pipeline, pool, tenantID := setupPipeline(t, "basic", consent.ModeStrip)
	pipeline.Bounds.MaxErrorReasonsPerTenant = 1
	ctx := context.Background()
	now := time.Now()

	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("cli.analytics.tier", "basic")
	m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("cli.errors")
	m.SetUnit("1")
	m.SetEmptySum().SetIsMonotonic(true)
	for _, reason := range []string{"no-token", "invalid-url", ""} {
		dp := m.Sum().DataPoints().AppendEmpty()
		dp.SetTimestamp(pcommon.NewTimestampFromTime(now))
		dp.SetIntValue(1)
		dp.Attributes().PutStr("cli.error.type", "auth")
		if reason != "" {
			dp.Attributes().PutStr("cli.error.reason", reason)
		}
	}

	if accepted, rejected, sample, err := pipeline.ProcessMetrics(ctx, md); err != nil || accepted != 3 || rejected != 0 {
		t.Fatalf("ProcessMetrics: accepted=%d rejected=%d sample=%q err=%v, want 3/0", accepted, rejected, sample, err)
	}
	rows, err := pool.Query(ctx, `SELECT coalesce(datapoint_attributes ->> 'cli.error.reason', '<none>') FROM metrics WHERE tenant_id=$1 AND metric_name='cli.errors'`, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		got[r]++
	}
	// Cap of 1: the first reason by name is admitted, the other folds.
	if len(got) != 3 || got["invalid-url"] != 1 || got["other"] != 1 || got["<none>"] != 1 {
		t.Errorf("stored reasons = %v, want invalid-url, other and one without", got)
	}
}

func TestProcessMetrics_Counter_Live(t *testing.T) {
	pipeline, pool, tenantID := setupPipeline(t, "basic", consent.ModeStrip)
	pipeline.Bounds.MaxCounterNamesPerTenant = 2
	pipeline.Bounds.MaxCounterLabelsPerName = 1
	ctx := context.Background()
	now := time.Now()

	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	// Anonymous: counters are reported without any install id.
	rm.Resource().Attributes().PutStr("cli.analytics.tier", "anonymous")
	metrics := rm.ScopeMetrics().AppendEmpty().Metrics()
	newCounter := func(temporality pmetric.AggregationTemporality) pmetric.Metric {
		m := metrics.AppendEmpty()
		m.SetName("cli.counter")
		m.SetUnit("1")
		m.SetEmptySum().SetIsMonotonic(true)
		m.Sum().SetAggregationTemporality(temporality)
		return m
	}
	// Each point is labelled by its cli.version so it can be found again.
	add := func(m pmetric.Metric, version, name, label string, v int64) {
		dp := m.Sum().DataPoints().AppendEmpty()
		dp.SetTimestamp(pcommon.NewTimestampFromTime(now))
		dp.SetIntValue(v)
		dp.Attributes().PutStr("cli.version", version)
		if name != "" {
			dp.Attributes().PutStr("cli.counter.name", name)
		}
		if label != "" {
			dp.Attributes().PutStr("cli.counter.label", label)
		}
	}
	delta := newCounter(pmetric.AggregationTemporalityDelta)
	add(delta, "kept", "rules.generated", "get", 3)
	add(delta, "kept-again", "rules.generated", "get", 2)
	// Cap of one label per name: "get" is more frequent, "post" folds.
	add(delta, "label-folded", "rules.generated", "post", 1)
	add(delta, "no-label", "schemes.seen", "", 4)
	// Cap of two names, both taken above: a third folds, label and all.
	add(delta, "name-folded", "tools.found", "nmap", 1)
	add(delta, "bad-label", "schemes.seen", "Bearer Token", 1)
	add(delta, "no-name", "", "get", 1)
	add(delta, "bad-name", "/home/alice", "", 1)
	add(delta, "negative", "rules.generated", "get", -1)
	// Cumulative counters would be double counted when summed: the whole
	// metric is rejected.
	add(newCounter(pmetric.AggregationTemporalityCumulative), "cumulative", "rules.generated", "get", 9)

	accepted, rejected, sample, err := pipeline.ProcessMetrics(ctx, md)
	if err != nil || accepted != 6 || rejected != 4 {
		t.Fatalf("ProcessMetrics: accepted=%d rejected=%d sample=%q err=%v, want 6/4", accepted, rejected, sample, err)
	}

	type stored struct {
		name, label, tier string
		value             float64
	}
	rows, err := pool.Query(ctx, `
SELECT cli_version, datapoint_attributes ->> 'cli.counter.name', coalesce(datapoint_attributes ->> 'cli.counter.label', ''), consent_tier, value
FROM metrics WHERE tenant_id = $1 AND metric_name = 'cli.counter'`, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]stored{}
	for rows.Next() {
		var version string
		var s stored
		if err := rows.Scan(&version, &s.name, &s.label, &s.tier, &s.value); err != nil {
			t.Fatal(err)
		}
		got[version] = s
	}
	rows.Close()
	want := map[string]stored{
		"kept":         {"rules.generated", "get", "anonymous", 3},
		"kept-again":   {"rules.generated", "get", "anonymous", 2},
		"label-folded": {"rules.generated", "other", "anonymous", 1},
		"no-label":     {"schemes.seen", "", "anonymous", 4},
		"name-folded":  {"other", "", "anonymous", 1},
		"bad-label":    {"schemes.seen", "", "anonymous", 1},
	}
	if len(got) != len(want) {
		t.Errorf("stored %d counter points, want %d: %+v", len(got), len(want), got)
	}
	for version, w := range want {
		if got[version] != w {
			t.Errorf("%s: stored %+v, want %+v", version, got[version], w)
		}
	}

	var foldedNames, foldedLabels int64
	if err := pool.QueryRow(ctx, `SELECT folded_names, folded_labels FROM counter_overflow WHERE tenant_id = $1`, tenantID).Scan(&foldedNames, &foldedLabels); err != nil || foldedNames != 1 || foldedLabels != 1 {
		t.Errorf("overflow = names %d labels %d (err %v), want 1 and 1", foldedNames, foldedLabels, err)
	}
}

func TestProcessMetrics_Measure_Live(t *testing.T) {
	pipeline, pool, tenantID := setupPipeline(t, "basic", consent.ModeStrip)
	pipeline.Bounds.MaxMeasureNamesPerTenant = 2
	pipeline.Bounds.MaxMeasureLabelsPerName = 1
	ctx := context.Background()
	now := time.Now()
	bounds := []float64{1, 5, 10, 50, 100, 500, 1000, 5000, 10000, 60000, 300000}

	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	// Anonymous: measures are reported without any install id.
	rm.Resource().Attributes().PutStr("cli.analytics.tier", "anonymous")
	metrics := rm.ScopeMetrics().AppendEmpty().Metrics()
	newMeasure := func(unit string, temporality pmetric.AggregationTemporality) pmetric.Metric {
		m := metrics.AppendEmpty()
		m.SetName("cli.measure")
		m.SetUnit(unit)
		m.SetEmptyHistogram().SetAggregationTemporality(temporality)
		return m
	}
	// Each point is labelled by its cli.version so it can be found again.
	// counts has one entry per bucket; sum, min and max follow from them
	// unless sum is overridden.
	type pt struct {
		version, name, label string
		counts               []uint64
		sum                  float64
		bounds               []float64
	}
	add := func(m pmetric.Metric, p pt) {
		dp := m.Histogram().DataPoints().AppendEmpty()
		dp.SetTimestamp(pcommon.NewTimestampFromTime(now))
		var total uint64
		for _, c := range p.counts {
			total += c
		}
		dp.SetCount(total)
		dp.SetSum(p.sum)
		dp.SetMin(p.sum / float64(max(total, 1)))
		dp.SetMax(p.sum)
		if p.bounds == nil {
			p.bounds = bounds
		}
		dp.ExplicitBounds().FromRaw(p.bounds)
		dp.BucketCounts().FromRaw(p.counts)
		dp.Attributes().PutStr("cli.version", p.version)
		if p.name != "" {
			dp.Attributes().PutStr("cli.measure.name", p.name)
		}
		if p.label != "" {
			dp.Attributes().PutStr("cli.measure.label", p.label)
		}
	}
	fast := []uint64{0, 0, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0} // two runs in (100, 500]
	delta := newMeasure("ms", pmetric.AggregationTemporalityDelta)
	add(delta, pt{version: "kept", name: "john.run", label: "hmac", counts: fast, sum: 600})
	add(delta, pt{version: "kept-again", name: "john.run", label: "hmac", counts: fast, sum: 700})
	// Cap of one label per name: "hmac" is more frequent, "md5" folds.
	add(delta, pt{version: "label-folded", name: "john.run", label: "md5", counts: fast, sum: 800})
	add(delta, pt{version: "no-label", name: "scan", counts: fast, sum: 900})
	// Cap of two names, both taken above: a third folds, label and all.
	add(delta, pt{version: "name-folded", name: "probe", label: "tcp", counts: fast, sum: 100})
	add(delta, pt{version: "bad-label", name: "scan", label: "Not A Label", counts: fast, sum: 300})
	add(delta, pt{version: "no-name", counts: fast, sum: 300})
	add(delta, pt{version: "bad-name", name: "/home/alice", counts: fast, sum: 300})
	// Buckets other than the server's cannot be added to the others'.
	add(delta, pt{version: "other-bounds", name: "scan", counts: []uint64{1, 1, 0}, sum: 3, bounds: []float64{10, 100}})
	// Counts that do not add up, or a negative duration.
	bad := pt{version: "inconsistent", name: "scan", counts: fast, sum: 600}
	add(delta, bad)
	delta.Histogram().DataPoints().At(delta.Histogram().DataPoints().Len() - 1).SetCount(5)
	add(delta, pt{version: "negative", name: "scan", counts: fast, sum: -600})
	// Cumulative histograms would be double counted when added, seconds are
	// not milliseconds: the whole metric is rejected.
	add(newMeasure("ms", pmetric.AggregationTemporalityCumulative), pt{version: "cumulative", name: "scan", counts: fast, sum: 600})
	add(newMeasure("s", pmetric.AggregationTemporalityDelta), pt{version: "seconds", name: "scan", counts: fast, sum: 600})
	// And so is a measure sent as another kind of metric.
	asSum := metrics.AppendEmpty()
	asSum.SetName("cli.measure")
	asSum.SetUnit("ms")
	asSum.SetEmptySum().SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
	sdp := asSum.Sum().DataPoints().AppendEmpty()
	sdp.SetTimestamp(pcommon.NewTimestampFromTime(now))
	sdp.SetIntValue(5)
	sdp.Attributes().PutStr("cli.measure.name", "scan")
	sdp.Attributes().PutStr("cli.version", "as-sum")

	accepted, rejected, sample, err := pipeline.ProcessMetrics(ctx, md)
	if err != nil || accepted != 6 || rejected != 8 {
		t.Fatalf("ProcessMetrics: accepted=%d rejected=%d sample=%q err=%v, want 6/8", accepted, rejected, sample, err)
	}

	type stored struct {
		name, label, tier string
		count             int64
		sum               float64
		buckets           int
	}
	rows, err := pool.Query(ctx, `
SELECT cli_version, datapoint_attributes ->> 'cli.measure.name', coalesce(datapoint_attributes ->> 'cli.measure.label', ''),
       consent_tier, histogram_count, histogram_sum, cardinality(histogram_bucket_counts)
FROM metrics WHERE tenant_id = $1 AND metric_name = 'cli.measure'`, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]stored{}
	for rows.Next() {
		var version string
		var s stored
		if err := rows.Scan(&version, &s.name, &s.label, &s.tier, &s.count, &s.sum, &s.buckets); err != nil {
			t.Fatal(err)
		}
		got[version] = s
	}
	rows.Close()
	want := map[string]stored{
		"kept":         {"john.run", "hmac", "anonymous", 2, 600, 12},
		"kept-again":   {"john.run", "hmac", "anonymous", 2, 700, 12},
		"label-folded": {"john.run", "other", "anonymous", 2, 800, 12},
		"no-label":     {"scan", "", "anonymous", 2, 900, 12},
		"name-folded":  {"other", "", "anonymous", 2, 100, 12},
		"bad-label":    {"scan", "", "anonymous", 2, 300, 12},
	}
	if len(got) != len(want) {
		t.Errorf("stored %d measure points, want %d: %+v", len(got), len(want), got)
	}
	for version, w := range want {
		if got[version] != w {
			t.Errorf("%s: stored %+v, want %+v", version, got[version], w)
		}
	}

	var foldedNames, foldedLabels int64
	if err := pool.QueryRow(ctx, `SELECT folded_names, folded_labels FROM measure_overflow WHERE tenant_id = $1`, tenantID).Scan(&foldedNames, &foldedLabels); err != nil || foldedNames != 1 || foldedLabels != 1 {
		t.Errorf("overflow = names %d labels %d (err %v), want 1 and 1", foldedNames, foldedLabels, err)
	}
	// Measures have their own vocabulary: counters are untouched.
	var counters int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM counter_names WHERE tenant_id = $1`, tenantID).Scan(&counters); err != nil || counters != 0 {
		t.Errorf("counter_names = %d (err %v), want 0", counters, err)
	}
}

func TestProcessLogs_TracksInstalls_Live(t *testing.T) {
	pipeline, pool, tenantID := setupPipeline(t, "basic", consent.ModeStrip)
	ctx := context.Background()
	now := time.Now()

	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("cli.analytics.tier", "basic")
	rl.Resource().Attributes().PutStr("cli.version", "1.2.3")
	rl.Resource().Attributes().PutStr("cli.os", "linux")
	rl.Resource().Attributes().PutStr("cli.arch", "amd64")
	records := rl.ScopeLogs().AppendEmpty().LogRecords()

	add := func(event string, at time.Time, attrs map[string]any) {
		rec := records.AppendEmpty()
		rec.SetEventName(event)
		rec.SetTimestamp(pcommon.NewTimestampFromTime(at))
		for k, v := range attrs {
			switch x := v.(type) {
			case string:
				rec.Attributes().PutStr(k, x)
			case int64:
				rec.Attributes().PutInt(k, x)
			}
		}
	}
	add("cli.session.started", now.Add(-2*time.Minute), map[string]any{"cli.install_id": "install-1", "cli.install.method": "brew"})
	add("cli.command.completed", now.Add(-time.Minute), map[string]any{"cli.command.name": "deploy", "cli.command.exit_code": int64(0), "cli.install_id": "install-1"})
	add("cli.session.started", now.Add(-time.Minute), map[string]any{}) // no install id: not tracked
	add("cli.help.viewed", now, map[string]any{"cli.command.name": "deploy"})

	if _, rejected, _, err := pipeline.ProcessLogs(ctx, ld); err != nil || rejected != 0 {
		t.Fatalf("ProcessLogs rejected=%d err=%v", rejected, err)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM installs WHERE tenant_id = $1`, tenantID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("installs rows = %d (%v), want 1", n, err)
	}
	var version, method string
	var firstSuccess *time.Time
	if err := pool.QueryRow(ctx, `SELECT last_version, install_method, first_success_at FROM installs WHERE tenant_id = $1 AND install_id = 'install-1'`, tenantID).
		Scan(&version, &method, &firstSuccess); err != nil {
		t.Fatalf("read install: %v", err)
	}
	if version != "1.2.3" || method != "brew" || firstSuccess == nil {
		t.Errorf("install = version %q method %q first success %v", version, method, firstSuccess)
	}
}

// feature and flag discovery events: an activation naming a feature, and an
// invocation listing the flags passed.
func discoveryLogs(tier string, now time.Time) plog.Logs {
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("cli.analytics.tier", tier)
	rl.Resource().Attributes().PutStr("cli.version", "1.2.3")
	records := rl.ScopeLogs().AppendEmpty().LogRecords()

	feature := records.AppendEmpty()
	feature.SetEventName("cli.feature.activated")
	feature.SetTimestamp(pcommon.NewTimestampFromTime(now.Add(-time.Minute)))
	feature.Attributes().PutStr("cli.install_id", "install-1")
	feature.Attributes().PutStr("cli.session_id", "session-1")
	feature.Attributes().PutStr("cli.activation.feature", "deploy")

	invoked := records.AppendEmpty()
	invoked.SetEventName("cli.command.invoked")
	invoked.SetTimestamp(pcommon.NewTimestampFromTime(now.Add(-2 * time.Minute)))
	invoked.Attributes().PutStr("cli.install_id", "install-1")
	invoked.Attributes().PutStr("cli.command.name", "deploy")
	flags := invoked.Attributes().PutEmptySlice("cli.flag.names")
	flags.AppendEmpty().SetStr("--force")
	flags.AppendEmpty().SetStr("--force")
	flags.AppendEmpty().SetStr("-v")
	return ld
}

func TestProcessLogs_TracksDiscoveries_Live(t *testing.T) {
	pipeline, pool, tenantID := setupPipeline(t, "basic", consent.ModeStrip)
	ctx := context.Background()
	now := time.Now()

	if _, rejected, _, err := pipeline.ProcessLogs(ctx, discoveryLogs("basic", now)); err != nil || rejected != 0 {
		t.Fatalf("ProcessLogs rejected=%d err=%v", rejected, err)
	}
	// A second batch with an earlier sighting of the same things.
	if _, rejected, _, err := pipeline.ProcessLogs(ctx, discoveryLogs("basic", now.Add(-time.Hour))); err != nil || rejected != 0 {
		t.Fatalf("ProcessLogs rejected=%d err=%v", rejected, err)
	}

	rows, err := pool.Query(ctx, `SELECT kind, name, first_at FROM install_discoveries
		WHERE tenant_id = $1 AND install_id = 'install-1' ORDER BY kind, name`, tenantID)
	if err != nil {
		t.Fatalf("query discoveries: %v", err)
	}
	defer rows.Close()
	got := map[string]time.Time{}
	for rows.Next() {
		var kind, name string
		var at time.Time
		if err := rows.Scan(&kind, &name, &at); err != nil {
			t.Fatal(err)
		}
		got[kind+"/"+name] = at
	}
	if len(got) != 3 {
		t.Fatalf("discoveries = %v, want feature/deploy, flag/--force and flag/-v", got)
	}
	if want := now.Add(-time.Hour - time.Minute); got["feature/deploy"].Sub(want).Abs() > time.Millisecond {
		t.Errorf("feature/deploy first_at = %v, want the earlier batch's %v", got["feature/deploy"], want)
	}
}

// Below basic tier the install id is stripped by the allowlist, so there is
// nothing to tie a discovery to and nothing is tracked.
func TestProcessLogs_AnonymousTierTracksNoDiscoveries_Live(t *testing.T) {
	pipeline, pool, tenantID := setupPipeline(t, "anonymous", consent.ModeStrip)
	ctx := context.Background()

	if _, rejected, _, err := pipeline.ProcessLogs(ctx, discoveryLogs("basic", time.Now())); err != nil || rejected != 0 {
		t.Fatalf("ProcessLogs rejected=%d err=%v", rejected, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM install_discoveries WHERE tenant_id = $1`, tenantID).Scan(&n); err != nil || n != 0 {
		t.Errorf("discoveries = %d (%v), want 0 for anonymous tier", n, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM logs WHERE tenant_id = $1 AND event_name = 'cli.feature.activated'`, tenantID).Scan(&n); err != nil || n != 1 {
		t.Errorf("stored activation events = %d (%v), want 1: the event itself is still accepted", n, err)
	}
}
