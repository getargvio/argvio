package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

// resolveDSN returns dsn if set, else $ARGVIO_STORAGE_DSN, else an error —
// shared by every operator-CLI subcommand below (migrate, tenant, apikey,
// policies, retention; formerly cmd/admin).
func resolveDSN(dsn string) (string, error) {
	if dsn != "" {
		return dsn, nil
	}
	if env := os.Getenv("ARGVIO_STORAGE_DSN"); env != "" {
		return env, nil
	}
	return "", fmt.Errorf("--dsn or $ARGVIO_STORAGE_DSN is required")
}

func connectPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return pool, nil
}
