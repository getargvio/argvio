package storage

import (
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the "pgx5" driver scheme
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/getargvio/argvio/migrations"
)

// newMigrator builds a golang-migrate instance backed by the embedded SQL
// files in migrations/ and the pgx/v5 driver (consistent with the rest of
// the storage layer — no lib/pq dependency pulled in just for migrations).
//
// dsn is accepted in the same postgres://... form used everywhere else in
// this codebase (pgxpool.New, docker-compose, docs) and rewritten to the
// "pgx5://" scheme golang-migrate's pgx driver registers under, so callers
// never need to know about that detail.
func newMigrator(dsn string) (*migrate.Migrate, error) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("storage: migration source: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, toPgx5URL(dsn))
	if err != nil {
		return nil, fmt.Errorf("storage: migrate init: %w", err)
	}
	return m, nil
}

func toPgx5URL(dsn string) string {
	for _, scheme := range []string{"postgres://", "postgresql://"} {
		if strings.HasPrefix(dsn, scheme) {
			return "pgx5://" + strings.TrimPrefix(dsn, scheme)
		}
	}
	return dsn
}

// MigrateUp applies all pending "up" migrations. Returns nil (not an error)
// if the schema is already at the latest version.
func MigrateUp(dsn string) error {
	m, err := newMigrator(dsn)
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("storage: migrate up: %w", err)
	}
	return nil
}

// MigrateDown rolls back all migrations. Intended for local dev / tests
// only — never wired to a production code path.
func MigrateDown(dsn string) error {
	m, err := newMigrator(dsn)
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("storage: migrate down: %w", err)
	}
	return nil
}

// MigrationVersion reports the currently applied migration version and
// whether the schema is in a dirty (failed mid-migration) state.
func MigrationVersion(dsn string) (version uint, dirty bool, err error) {
	m, err := newMigrator(dsn)
	if err != nil {
		return 0, false, err
	}
	defer m.Close()
	version, dirty, err = m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	return version, dirty, err
}
