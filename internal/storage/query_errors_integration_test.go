package storage

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getargvio/argvio/internal/errfp"
)

var testTierRanks = TierRanks{"anonymous": 0, "basic": 1, "full": 2, "optin_plus": 3}

// TestErrorFingerprints_Live seeds error events by hand-computed fingerprint
// and checks ranking, trend, tenant scoping and the read-time tier ceiling.
func TestErrorFingerprints_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live error fingerprints test")
	}
	ctx := context.Background()
	if err := MigrateUp(dsn); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	tenant, other, unconfigured := uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{tenant, other, unconfigured} {
		if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1,$2,$3)`, id, id.String(), "t"); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	}
	setCeiling := func(id uuid.UUID, tier string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
INSERT INTO tenant_config (tenant_id, tier_ceiling) VALUES ($1, $2)
ON CONFLICT (tenant_id) DO UPDATE SET tier_ceiling = EXCLUDED.tier_ceiling`, id, tier); err != nil {
			t.Fatalf("set ceiling: %v", err)
		}
	}
	setCeiling(tenant, "optin_plus")
	setCeiling(other, "optin_plus")

	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -3)
	rng := TimeRange{From: day, To: day.AddDate(0, 0, 2)}

	network := "dial tcp 10.0.0.1:443: connection refused"
	panicStack := "main.sync()\n\t/home/alice/src/tool/sync.go:41 +0x1d\nmain.main()\n\t/home/alice/src/tool/main.go:9 +0x25"
	fpNetwork, _ := errfp.Compute(network, "")
	fpPanic, _ := errfp.Compute("index out of range", panicStack)

	var logs []LogRow
	event := func(tenantID uuid.UUID, at time.Time, tier, install, command, version, msg, stack string) {
		attrs := Attrs{"cli.install_id": install}
		var fp *string
		if r, ok := errfp.Compute(msg, stack); ok {
			attrs[errfp.AttrMessage] = msg
			if stack != "" {
				attrs[errfp.AttrStacktrace] = stack
			}
			fp = &r.Fingerprint
		}
		logs = append(logs, LogRow{
			Time: at, TenantID: tenantID, EventName: errfp.EventName, CommandName: strp(command), CLIVersion: strp(version),
			ConsentTier: tier, ErrorFingerprint: fp, LogAttributes: attrs,
		})
	}
	d1, d2 := day.Add(10*time.Hour), day.AddDate(0, 0, 1).Add(10*time.Hour)
	// Network: 4 events, 1 install. Panic: 3 events, 3 installs, so it ranks
	// first despite fewer events; one of its installs is at optin_plus.
	for range 4 {
		event(tenant, d1, "full", "i1", "sync", "1.2.0", network, "")
	}
	event(tenant, d1, "full", "i2", "sync", "1.10.0", "index out of range", panicStack)
	event(tenant, d2, "full", "i3", "pull", "1.2.0", "index out of range [5]", strings.ReplaceAll(panicStack, "alice", "bob"))
	event(tenant, d2, "optin_plus", "i4", "sync", "1.9.0", "index out of range", panicStack)
	// No message or stack (stripped below full): no fingerprint, not counted.
	event(tenant, d1, "basic", "i5", "sync", "1.2.0", "", "")
	event(other, d1, "full", "x1", "leak", "9.9.9", "other tenant secret", "")

	if _, err := NewWriter(pool).InsertLogs(ctx, logs); err != nil {
		t.Fatalf("insert logs: %v", err)
	}
	qb := NewQueryBuilder(pool)
	scope, _ := NewTenantScope(tenant)
	query := func(t *testing.T, s TenantScope, f Filters) []ErrorFingerprint {
		t.Helper()
		f.Time = rng
		got, err := qb.ErrorFingerprints(ctx, s, f, BucketDay, testTierRanks)
		if err != nil {
			t.Fatalf("ErrorFingerprints: %v", err)
		}
		return got
	}

	t.Run("ranked by affected installs with counts and trend", func(t *testing.T) {
		got := query(t, scope, Filters{})
		if len(got) != 2 {
			t.Fatalf("got %d fingerprints, want 2: %+v", len(got), got)
		}
		p, n := got[0], got[1]
		if p.Fingerprint != fpPanic.Fingerprint || n.Fingerprint != fpNetwork.Fingerprint {
			t.Fatalf("order = %s, %s; want panic %s then network %s", p.Fingerprint, n.Fingerprint, fpPanic.Fingerprint, fpNetwork.Fingerprint)
		}
		if p.EventCount != 3 || p.AffectedInstalls != 3 || n.EventCount != 4 || n.AffectedInstalls != 1 {
			t.Errorf("panic %d/%d network %d/%d, want 3/3 and 4/1", p.EventCount, p.AffectedInstalls, n.EventCount, n.AffectedInstalls)
		}
		if p.Source != errfp.SourceStack || p.TopFrame == nil || *p.TopFrame != "main.sync sync.go" {
			t.Errorf("panic source %q top frame %v", p.Source, p.TopFrame)
		}
		if n.Source != errfp.SourceMessage || n.Message == nil || *n.Message != "dial tcp <n>: connection refused" || n.TopFrame != nil {
			t.Errorf("network source %q message %v top frame %v", n.Source, n.Message, n.TopFrame)
		}
		if !slices.Equal(p.Commands, []string{"pull", "sync"}) || !slices.Equal(p.Versions, []string{"1.2.0", "1.9.0", "1.10.0"}) {
			t.Errorf("panic commands %v versions %v", p.Commands, p.Versions)
		}
		if !p.FirstSeen.Equal(d1) || !p.LastSeen.Equal(d2) {
			t.Errorf("panic seen %v..%v, want %v..%v", p.FirstSeen, p.LastSeen, d1, d2)
		}
		want := []ErrorFingerprintPoint{
			{Bucket: day, EventCount: 1, AffectedInstalls: 1},
			{Bucket: day.AddDate(0, 0, 1), EventCount: 2, AffectedInstalls: 2},
		}
		if len(p.Trend) != 2 || !p.Trend[0].Bucket.Equal(want[0].Bucket) || p.Trend[0].EventCount != 1 ||
			!p.Trend[1].Bucket.Equal(want[1].Bucket) || p.Trend[1].EventCount != 2 || p.Trend[1].AffectedInstalls != 2 {
			t.Errorf("panic trend = %+v, want %+v", p.Trend, want)
		}
	})

	t.Run("paginates", func(t *testing.T) {
		got := query(t, scope, Filters{Limit: 1, Offset: 1})
		if len(got) != 1 || got[0].Fingerprint != fpNetwork.Fingerprint {
			t.Errorf("page 2 = %+v, want only the network fingerprint", got)
		}
	})

	t.Run("filters apply", func(t *testing.T) {
		got := query(t, scope, Filters{CommandNames: []string{"pull"}})
		if len(got) != 1 || got[0].EventCount != 1 {
			t.Errorf("command=pull: %+v, want one fingerprint with one event", got)
		}
	})

	t.Run("tenant scoped", func(t *testing.T) {
		otherScope, _ := NewTenantScope(other)
		got := query(t, otherScope, Filters{})
		if len(got) != 1 || got[0].Commands[0] != "leak" {
			t.Errorf("other tenant got %+v, want only its own event", got)
		}
		for _, fp := range query(t, scope, Filters{}) {
			if slices.Contains(fp.Commands, "leak") {
				t.Errorf("tenant sees other tenant's event: %+v", fp)
			}
		}
	})

	t.Run("lowered ceiling hides events stored above it", func(t *testing.T) {
		setCeiling(tenant, "full")
		got := query(t, scope, Filters{})
		if len(got) != 2 || got[0].Fingerprint != fpPanic.Fingerprint || got[0].EventCount != 2 || slices.Contains(got[0].Versions, "1.9.0") {
			t.Errorf("ceiling full: %+v, want the optin_plus event gone", got)
		}

		setCeiling(tenant, "basic")
		if got := query(t, scope, Filters{}); len(got) != 0 {
			t.Errorf("ceiling basic: got %d fingerprints, want none", len(got))
		}
		setCeiling(tenant, "optin_plus")
	})

	t.Run("no tenant config or unknown ceiling reads nothing", func(t *testing.T) {
		unconfiguredScope, _ := NewTenantScope(unconfigured)
		var rows []LogRow
		for _, r := range logs[:1] {
			r.TenantID = unconfigured
			rows = append(rows, r)
		}
		if _, err := NewWriter(pool).InsertLogs(ctx, rows); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if got := query(t, unconfiguredScope, Filters{}); len(got) != 0 {
			t.Errorf("no tenant_config: got %+v, want none", got)
		}
		got, err := qb.ErrorFingerprints(ctx, scope, Filters{Time: rng}, BucketDay, TierRanks{"anonymous": 0})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("ceiling not in ranking: got %+v, want none", got)
		}
	})

}
