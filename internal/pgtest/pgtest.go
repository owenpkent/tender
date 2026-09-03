// Package pgtest hands tests a real Postgres.
//
// There is no in-memory fake here on purpose. Most of what this repository
// claims to get right (deferred constraint triggers, ON CONFLICT under
// contention, row locks taken in a fixed order) is behaviour of Postgres
// rather than behaviour of Go, and a fake would test the fake.
package pgtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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
// The database is per test binary and reset per test, so tests in a package
// must not call t.Parallel. Contention is exercised inside a test with
// goroutines, which is the contention worth testing anyway.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv(DSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set, skipping the tests that need Postgres", DSNEnv)
	}

	once.Do(func() { shared, initErr = openIsolated(dsn) })
	if initErr != nil {
		t.Fatalf("prepare test database: %v", initErr)
	}

	truncate(t, shared)
	return shared
}

// openIsolated builds this test binary its own database.
//
// go test compiles one binary per package and runs them at the same time. A
// shared database would mean one package truncating its tables between tests
// while another is mid-transaction against them, which fails in a way that
// looks like a bug in the code under test rather than in the harness.
func openIsolated(dsn string) (*pgxpool.Pool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	name := databaseName()

	admin, err := db.Open(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer admin.Close()

	// FORCE disconnects anything still attached from a previous run that was
	// interrupted, which is otherwise the one way this gets stuck.
	if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`); err != nil {
		return nil, fmt.Errorf("drop %s: %w", name, err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+quoteIdent(name)); err != nil {
		return nil, fmt.Errorf("create %s: %w", name, err)
	}

	testDSN, err := withDatabase(dsn, name)
	if err != nil {
		return nil, err
	}

	pool, err := db.Open(ctx, testDSN)
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// databaseName derives a stable name from the test binary, so a run leaves one
// database per package behind and the next run reuses the name rather than
// littering.
func databaseName() string {
	base := filepath.Base(os.Args[0])
	base = strings.TrimSuffix(base, ".exe")
	base = strings.TrimSuffix(base, ".test")

	var b strings.Builder
	b.WriteString("tender_test_")
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}

	name := b.String()
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func withDatabase(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		return "", fmt.Errorf("%s must be a URL such as postgres://user:pass@host:5432/db, got %q", DSNEnv, dsn)
	}
	u.Path = "/" + name
	return u.String(), nil
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
