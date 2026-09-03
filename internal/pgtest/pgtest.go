// Package pgtest hands tests a real Postgres.
//
// There is no in-memory fake here on purpose. Most of what this repository
// claims to get right (deferred constraint triggers, ON CONFLICT under
// contention, row locks taken in a fixed order) is behaviour of Postgres
// rather than behaviour of Go, and a fake would test the fake.
package pgtest

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/owenpkent/tender/internal/db"
)

// DSNEnv names the environment variable holding the test database URL. CI sets
// it from a Postgres service container; locally, `make up` prints the value.
const DSNEnv = "TENDER_TEST_DSN"

var (
	once    sync.Once
	shared  *pgxpool.Pool
	initErr error
)

// Pool returns a migrated pool with every table empty.
//
// The database is shared and reset per test, so tests in a package must not
// call t.Parallel. Contention is exercised inside a test with goroutines,
// which is the contention worth testing anyway.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv(DSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set, skipping the tests that need Postgres", DSNEnv)
	}

	once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		shared, initErr = db.Open(ctx, dsn)
		if initErr != nil {
			return
		}
		initErr = db.Migrate(ctx, shared)
	})
	if initErr != nil {
		t.Fatalf("prepare test database: %v", initErr)
	}

	truncate(t, shared)
	return shared
}

func truncate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := pool.Exec(ctx, `
		TRUNCATE payments, entries, transfers, account_balances, accounts
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}
