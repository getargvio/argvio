package otlp

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"

	"github.com/getargvio/argvio/internal/allowlist"
	"github.com/getargvio/argvio/internal/consent"
	"github.com/getargvio/argvio/internal/ratelimit"
	"github.com/getargvio/argvio/internal/storage"
	"github.com/getargvio/argvio/internal/tenant"
)

// TestHTTPTracesEndToEnd_Live drives the real net/http handler: OTLP/HTTP
// protobuf request in, auth via a real API key against a real tenant.Cache
// backed by Postgres, real partial-success response out.
func TestHTTPTracesEndToEnd_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live HTTP test")
	}
	ctx := context.Background()
	if err := storage.MigrateUp(dsn); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	store := tenant.NewStore(pool)
	ten, err := store.CreateTenant(ctx, "http-test-"+time.Now().Format("150405.000000000"), "HTTP Test")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if err := store.UpsertConfig(ctx, tenant.Config{TenantID: ten.ID, TierCeiling: "full", TierEnforcementMode: consent.ModeStrip}); err != nil {
		t.Fatalf("UpsertConfig: %v", err)
	}
	rawKey, _, err := store.CreateAPIKey(ctx, ten.ID, tenant.ScopePublicIngest)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	testLog := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	loader, err := allowlist.Load("../../schema/allowlist/v1.yaml", testLog)
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}

	cache := tenant.NewCache(store, time.Minute)
	limiter := ratelimit.New(ratelimit.Defaults{RequestsPerSecond: 1000, BytesPerSecond: 100_000_000, Burst: 1000})
	srv := NewServer(cache, loader, storage.NewWriter(pool), testBounds(), limiter, testLog)

	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()

	now := time.Now()
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("cli.analytics.tier", "anonymous")
	ss := rs.ScopeSpans().AppendEmpty()

	good := ss.Spans().AppendEmpty()
	good.SetName("cli.command.status")
	good.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	good.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(10 * time.Millisecond)))
	good.Attributes().PutStr("cli.command.name", "status")

	bad := ss.Spans().AppendEmpty()
	bad.SetName("cli.not.allowlisted")
	bad.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	bad.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(10 * time.Millisecond)))

	req := ptraceotlp.NewExportRequestFromTraces(td)
	body, err := req.MarshalProto()
	if err != nil {
		t.Fatalf("MarshalProto: %v", err)
	}

	httpReq, _ := http.NewRequest("POST", ts.URL+"/v1/traces", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	httpReq.Header.Set(APIKeyHeader, rawKey)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("http request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var respBuf bytes.Buffer
	respBuf.ReadFrom(resp.Body)

	exportResp := ptraceotlp.NewExportResponse()
	if err := exportResp.UnmarshalProto(respBuf.Bytes()); err != nil {
		t.Fatalf("UnmarshalProto response: %v", err)
	}
	if exportResp.PartialSuccess().RejectedSpans() != 1 {
		t.Errorf("rejected_spans = %d, want 1", exportResp.PartialSuccess().RejectedSpans())
	}

	// Bad API key -> 401, no data written.
	httpReq2, _ := http.NewRequest("POST", ts.URL+"/v1/traces", bytes.NewReader(body))
	httpReq2.Header.Set("Content-Type", "application/x-protobuf")
	httpReq2.Header.Set(APIKeyHeader, "totally-bogus-key")
	resp2, err := http.DefaultClient.Do(httpReq2)
	if err != nil {
		t.Fatalf("http request 2: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for bad api key", resp2.StatusCode)
	}
}
