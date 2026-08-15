package otlp

import (
	"context"
	"log/slog"
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
