// Package db opens the Postgres pool and owns schema migration.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/owenpkent/tender/migrations"
)

// Open returns a pool with conservative limits. Every query in this service is
// short, so a connection held for longer than a few seconds means something is
// stuck rather than slow, and the limits should surface that as pressure
// instead of hiding it behind an unbounded pool.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MaxConns = 16
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// migrationLock is an arbitrary constant, the bytes of "tend". Every process
// that migrates this database takes the same one.
const migrationLock int64 = 0x74656e64

// Migrate applies every pending migration. Migrations are forward only: a
// mistake is corrected by a new migration, never by editing one that has
// already run somewhere.
//
// It takes a session advisory lock first, because replicas start together. Two
// processes running CREATE TYPE at the same moment do not politely take turns:
// one of them fails on a duplicate key in a system catalogue, which is a
// confusing way to learn that a deploy raced itself.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if err := configureGoose(); err != nil {
		return err
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for migration lock: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLock); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	defer func() {
		// Released on its own context, because a cancelled migration still has
		// to let the next process in.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLock)
	}()

	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()

	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// Version reports the migration the database is currently at.
func Version(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	if err := configureGoose(); err != nil {
		return 0, err
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()

	v, err := goose.GetDBVersionContext(ctx, sqlDB)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return v, nil
}

func configureGoose() error {
	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}
	return nil
}
