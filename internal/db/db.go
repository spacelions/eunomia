package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/migrate"

	"github.com/spacelions/eunomia/internal/db/migrations"
)

// Open creates a bun.DB backed by PostgreSQL and verifies connectivity immediately (fail-fast).
func Open(ctx context.Context, dsn string) (*bun.DB, error) {
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	sqldb.SetMaxOpenConns(16)
	sqldb.SetMaxIdleConns(4)
	sqldb.SetConnMaxLifetime(30 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := sqldb.PingContext(pingCtx); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}

	return bun.NewDB(sqldb, pgdialect.New()), nil
}

// Migrate initializes the bun_migrations table and applies all pending SQL migrations.
// Called explicitly via `eunomia migrate` (never auto-run by `eunomia serve`).
func Migrate(ctx context.Context, db *bun.DB) (*migrate.MigrationGroup, error) {
	migrator := migrate.NewMigrator(db, migrations.Migrations)
	if err := migrator.Init(ctx); err != nil {
		return nil, fmt.Errorf("initializing migrations table: %w", err)
	}
	if err := migrator.Lock(ctx); err != nil {
		return nil, fmt.Errorf("acquiring migration lock: %w", err)
	}
	defer func() {
		_ = migrator.Unlock(ctx)
	}()

	group, err := migrator.Migrate(ctx)
	if err != nil {
		return nil, fmt.Errorf("running migrations: %w", err)
	}
	return group, nil
}
