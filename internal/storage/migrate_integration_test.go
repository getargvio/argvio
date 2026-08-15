package storage

import (
	"os"
	"testing"
)

// TestMigrateUpDown_Live runs the real migration set against a live
// Postgres/Timescale instance. It only runs when ARGVIO_TEST_DSN is set
// (e.g. in CI with a timescale/timescaledb-ha service container, or
// locally against `docker compose up -d postgres`), since it needs real
// infrastructure, not just a Go test sandbox.
func TestMigrateUpDown_Live(t *testing.T) {
	dsn := os.Getenv("ARGVIO_TEST_DSN")
	if dsn == "" {
		t.Skip("ARGVIO_TEST_DSN not set; skipping live migration test")
	}

	if err := MigrateUp(dsn); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}
	version, dirty, err := MigrationVersion(dsn)
	if err != nil {
		t.Fatalf("MigrationVersion: %v", err)
	}
	if dirty {
		t.Fatalf("schema left dirty after MigrateUp")
	}
	if version == 0 {
		t.Fatalf("expected non-zero migration version after MigrateUp")
	}

	if err := MigrateUp(dsn); err != nil {
		t.Fatalf("MigrateUp (idempotent re-run): %v", err)
	}

	if err := MigrateDown(dsn); err != nil {
		t.Fatalf("MigrateDown: %v", err)
	}
	_, dirty, err = MigrationVersion(dsn)
	if err != nil {
		t.Fatalf("MigrationVersion after down: %v", err)
	}
	if dirty {
		t.Fatalf("schema left dirty after MigrateDown")
	}
}
