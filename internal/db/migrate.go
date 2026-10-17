package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

const migrationLock = 727274

type migration struct {
	version int
	name    string
}

var migrationName = regexp.MustCompile(`^([0-9]{1,6})_[A-Za-z0-9_.-]+\.sql$`)

// loadMigrations lists dir of fsys ordered by the numeric prefix of the file names. The version
// recorded in schema_migrations is that number, so renaming or inserting a file can never shift
// the versions of already applied migrations. Names must look like 001_name.sql, numbers must be
// unique and run 1..n without gaps (a missing number would mean a lost migration).
func loadMigrations(fsys fs.FS, dir string) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migration file %q does not match NNN_name.sql", e.Name())
		}
		v, _ := strconv.Atoi(m[1])
		if v < 1 {
			return nil, fmt.Errorf("migration %q: version must be at least 1", e.Name())
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d", prev, e.Name(), v)
		}
		seen[v] = e.Name()
		out = append(out, migration{v, e.Name()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration versions must be contiguous from 1: expected %d, found %q", i+1, m.name)
		}
	}
	return out, nil
}

// Migrate applies unapplied migrations in version order. Safe to call concurrently.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migs, err := loadMigrations(migrationFS, "migrations")
	if err != nil {
		return err
	}
	for _, mg := range migs {
		version := mg.version
		e := mg
		sqlText, err := migrationFS.ReadFile("migrations/" + e.name)
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
				return fmt.Errorf("migration %s: %w", e.name, err)
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
