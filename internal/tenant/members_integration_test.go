package tenant

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getargvio/argvio/internal/storage"
)

// TestMembers_Live exercises tenant_members against a real Postgres (same
// ARGVIO_TEST_DSN gating as the other live tests).
func TestMembers_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live tenant members test")
	}
	ctx := context.Background()
	if err := storage.MigrateUp(dsn); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()
	store := NewStore(pool)

	suffix := time.Now().Format("150405.000000")
	a, err := store.CreateTenant(ctx, "members-a-"+suffix, "Acme "+suffix)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.CreateTenant(ctx, "members-b-"+suffix, "Beta "+suffix)
	if err != nil {
		t.Fatal(err)
	}
	const issuer = "https://idp.example.com/" // trailing slash is significant
	const sub = "auth0|alice"

	t.Run("FindTenant by id and by slug", func(t *testing.T) {
		for _, ref := range []string{a.ID.String(), a.Slug} {
			got, err := store.FindTenant(ctx, ref)
			if err != nil || got.ID != a.ID {
				t.Errorf("FindTenant(%q) = %+v, %v", ref, got, err)
			}
		}
		if _, err := store.FindTenant(ctx, "no-such-tenant"); err != ErrNotFound {
			t.Errorf("unknown tenant: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("add is idempotent and defaults to viewer", func(t *testing.T) {
		m, created, err := store.AddMember(ctx, a.ID, issuer, sub, "")
		if err != nil || !created || m.Role != "viewer" {
			t.Fatalf("first add: %+v created=%v err=%v", m, created, err)
		}
		m, created, err = store.AddMember(ctx, a.ID, issuer, sub, "")
		if err != nil || created || m.Role != "viewer" {
			t.Fatalf("second add: %+v created=%v err=%v", m, created, err)
		}
	})

	t.Run("re-adding without a role never demotes; with a role it updates", func(t *testing.T) {
		if m, _, err := store.AddMember(ctx, a.ID, issuer, sub, "admin"); err != nil || m.Role != "admin" {
			t.Fatalf("promote: %+v %v", m, err)
		}
		if m, created, err := store.AddMember(ctx, a.ID, issuer, sub, ""); err != nil || created || m.Role != "admin" {
			t.Fatalf("add without role reset the role: %+v created=%v err=%v", m, created, err)
		}
	})

	t.Run("issuer is matched exactly", func(t *testing.T) {
		ms, err := store.ListMemberships(ctx, "https://idp.example.com", sub) // no trailing slash
		if err != nil || len(ms) != 0 {
			t.Fatalf("memberships for the slash-less issuer = %v, %v; want none", ms, err)
		}
	})

	t.Run("memberships span tenants, in stable order", func(t *testing.T) {
		if _, _, err := store.AddMember(ctx, b.ID, issuer, sub, "viewer"); err != nil {
			t.Fatal(err)
		}
		ms, err := store.ListMemberships(ctx, issuer, sub)
		if err != nil || len(ms) != 2 {
			t.Fatalf("memberships = %v, %v", ms, err)
		}
		if ms[0].Tenant.ID != a.ID || ms[0].Role != "admin" || ms[1].Tenant.ID != b.ID {
			t.Errorf("memberships = %+v", ms)
		}
	})

	t.Run("list members filters", func(t *testing.T) {
		if _, _, err := store.AddMember(ctx, a.ID, issuer, "auth0|bob", ""); err != nil {
			t.Fatal(err)
		}
		all, _ := store.ListMembers(ctx, a.ID, "", "")
		one, _ := store.ListMembers(ctx, a.ID, issuer, "auth0|bob")
		none, _ := store.ListMembers(ctx, a.ID, "https://other.example.com", "")
		if len(all) != 2 || len(one) != 1 || len(none) != 0 {
			t.Errorf("all=%d one=%d none=%d", len(all), len(one), len(none))
		}
	})

	t.Run("cache serves repeats and keeps a revoked member for at most the TTL", func(t *testing.T) {
		cache := NewCache(store, 300*time.Millisecond)
		ms, err := cache.Memberships(ctx, issuer, sub)
		if err != nil || len(ms) != 2 {
			t.Fatalf("cold: %v, %v", ms, err)
		}
		if removed, err := store.RemoveMember(ctx, b.ID, issuer, sub); err != nil || !removed {
			t.Fatalf("remove: removed=%v err=%v", removed, err)
		}
		if ms, _ = cache.Memberships(ctx, issuer, sub); len(ms) != 2 {
			t.Errorf("warm cache should still show 2 memberships, got %d", len(ms))
		}
		time.Sleep(400 * time.Millisecond)
		if ms, _ = cache.Memberships(ctx, issuer, sub); len(ms) != 1 {
			t.Errorf("after TTL expected 1 membership, got %d", len(ms))
		}
	})

	t.Run("remove reports whether anything was removed", func(t *testing.T) {
		if removed, err := store.RemoveMember(ctx, b.ID, issuer, sub); err != nil || removed {
			t.Errorf("second remove: removed=%v err=%v", removed, err)
		}
	})
}

// TestActiveTenants_Live exercises the admin tenant listing against a real
// Postgres (same ARGVIO_TEST_DSN gating as the other live tests).
func TestActiveTenants_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live active tenants test")
	}
	ctx := context.Background()
	if err := storage.MigrateUp(dsn); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()
	store := NewStore(pool)

	suffix := time.Now().Format("150405.000000")
	a, err := store.CreateTenant(ctx, "active-a-"+suffix, "Active A "+suffix)
	if err != nil {
		t.Fatal(err)
	}
	gone, err := store.CreateTenant(ctx, "active-gone-"+suffix, "Active Gone "+suffix)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, gone.ID); err != nil {
		t.Fatal(err)
	}

	has := func(ts []Tenant, id uuid.UUID) bool {
		for _, x := range ts {
			if x.ID == id {
				return true
			}
		}
		return false
	}

	t.Run("lists active tenants only", func(t *testing.T) {
		ts, err := store.ListActiveTenants(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !has(ts, a.ID) || has(ts, gone.ID) {
			t.Errorf("active=%v suspended listed=%v", has(ts, a.ID), has(ts, gone.ID))
		}
		for _, x := range ts {
			if x.Status != StatusActive {
				t.Errorf("tenant %s has status %q", x.ID, x.Status)
			}
		}
	})

	t.Run("cache serves repeats and picks up a new tenant within the short TTL", func(t *testing.T) {
		cache := NewCache(store, 4*time.Second) // negative TTL = 1s
		if ts, err := cache.ActiveTenants(ctx); err != nil || !has(ts, a.ID) {
			t.Fatalf("cold: %v, %v", has(ts, a.ID), err)
		}
		b, err := store.CreateTenant(ctx, "active-b-"+suffix, "Active B "+suffix)
		if err != nil {
			t.Fatal(err)
		}
		if ts, _ := cache.ActiveTenants(ctx); has(ts, b.ID) {
			t.Error("warm cache should not see the new tenant yet")
		}
		time.Sleep(1200 * time.Millisecond)
		if ts, _ := cache.ActiveTenants(ctx); !has(ts, b.ID) {
			t.Error("after the TTL the new tenant should be listed")
		}
	})
}
