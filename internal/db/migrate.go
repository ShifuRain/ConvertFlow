package db

import (
	"context"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

const migrationLock = 727274

// Migrate applies unapplied migrations in filename order. Safe to call concurrently.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	for i, e := range entries {
		version := i + 1
		sqlText, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLock); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version int PRIMARY KEY)`); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		var done bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&done); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if !done {
			if _, err := tx.Exec(ctx, string(sqlText)); err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("migration %s: %w", e.Name(), err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
				_ = tx.Rollback(ctx)
				return err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}
