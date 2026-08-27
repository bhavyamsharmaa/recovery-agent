// Package db opens the Postgres connection and applies schema migrations.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/bhavyamsharmaa/recovery-agent/migrations"
)

// Connect opens a connection pool using DATABASE_URL from the environment.
// It returns an error rather than panicking if the variable is missing or the
// connection fails, matching how ANTHROPIC_API_KEY is handled in decide.
//
// pgx is used through database/sql rather than natively: the standard
// interface is what the rest of the project will expect, and pgx is the
// better-maintained driver behind it.
func Connect(ctx context.Context) (*sql.DB, error) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return nil, errors.New("db: DATABASE_URL is not set")
	}

	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("db: parse DATABASE_URL: %w", err)
	}

	// Neon's connection string points at a PgBouncer pooler in transaction
	// mode, which does not carry server-side prepared statements across
	// statements in a pool. pgx's default mode caches them and would fail
	// intermittently — the failure surfaces later as "prepared statement
	// already exists", never at connect time, which makes it expensive to
	// diagnose.
	//
	// Simple protocol also lets a migration file contain several statements in
	// one Exec. The extended protocol allows exactly one, which would mean
	// splitting SQL on semicolons — fragile the moment a migration contains a
	// function body or a string with a semicolon in it.
	//
	// pgx escapes parameters client-side in this mode, so parameterised queries
	// remain safe.
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	pool := stdlib.OpenDB(*cfg)
	if err := pool.PingContext(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: connect: %w", err)
	}
	return pool, nil
}

// Migrate applies every embedded migration not already recorded in
// schema_migrations, in filename order, each inside its own transaction.
//
// Running it twice is a no-op: the second run finds every version recorded and
// applies nothing. The migrations are additionally written with IF NOT EXISTS
// so that a database migrated before this runner existed still converges.
func Migrate(ctx context.Context, pool *sql.DB) ([]string, error) {
	// Bootstrap. The table is also created by 001_init.sql, but it has to exist
	// before the runner can ask what has been applied — including on the very
	// first run, when 001 has not been applied yet.
	if _, err := pool.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return nil, fmt.Errorf("db: create schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return nil, err
	}

	versions, err := availableVersions()
	if err != nil {
		return nil, err
	}

	var ran []string
	for _, v := range versions {
		if applied[v] {
			continue
		}
		if err := applyOne(ctx, pool, v); err != nil {
			return ran, err
		}
		ran = append(ran, v)
	}
	return ran, nil
}

func appliedVersions(ctx context.Context, pool *sql.DB) (map[string]bool, error) {
	rows, err := pool.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("db: read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]bool)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("db: scan schema_migrations: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate schema_migrations: %w", err)
	}
	return applied, nil
}

// availableVersions lists the embedded migrations sorted by filename, which is
// why they are numbered: lexical order has to equal application order.
func availableVersions() ([]string, error) {
	entries, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("db: list migrations: %w", err)
	}
	if len(entries) == 0 {
		return nil, errors.New("db: no migrations were embedded")
	}
	sort.Strings(entries)
	return entries, nil
}

// applyOne runs a migration and records it in the same transaction, so a
// migration can never be applied without being recorded, or recorded without
// being applied. A failure rolls back both.
func applyOne(ctx context.Context, pool *sql.DB, version string) error {
	body, err := migrations.FS.ReadFile(version)
	if err != nil {
		return fmt.Errorf("db: read migration %s: %w", version, err)
	}

	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: begin %s: %w", version, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	if _, err := tx.ExecContext(ctx, string(body)); err != nil {
		return fmt.Errorf("db: apply %s: %w", version, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
		return fmt.Errorf("db: record %s: %w", version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("db: commit %s: %w", version, err)
	}
	return nil
}
