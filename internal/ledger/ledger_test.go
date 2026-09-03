package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/owenpkent/tender/internal/ledger"
	"github.com/owenpkent/tender/internal/money"
	"github.com/owenpkent/tender/internal/pgtest"
)

const usd = "USDC"

func newLedger(t *testing.T) (*ledger.Ledger, *pgxpool.Pool) {
	t.Helper()
	pool := pgtest.Pool(t)
	return ledger.New(pool), pool
}

func accounts(t *testing.T, l *ledger.Ledger) (world, merchant, fees ledger.Account) {
	t.Helper()
	ctx := context.Background()

	var err error
	if world, err = l.EnsureAccount(ctx, ledger.World, "chain", usd); err != nil {
		t.Fatalf("world account: %v", err)
	}
	if merchant, err = l.EnsureAccount(ctx, ledger.MerchantPending, "merchant_a", usd); err != nil {
		t.Fatalf("merchant account: %v", err)
	}
	if fees, err = l.EnsureAccount(ctx, ledger.Fees, "platform", usd); err != nil {
		t.Fatalf("fees account: %v", err)
	}
	return world, merchant, fees
}

func balance(t *testing.T, l *ledger.Ledger, id uuid.UUID) money.Amount {
	t.Helper()
	b, err := l.Balance(context.Background(), id)
	if err != nil {
		t.Fatalf("balance %s: %v", id, err)
	}
	return b
}

func TestEnsureAccountIsIdempotent(t *testing.T) {
	l, _ := newLedger(t)
	ctx := context.Background()

	first, err := l.EnsureAccount(ctx, ledger.MerchantPending, "merchant_a", usd)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := l.EnsureAccount(ctx, ledger.MerchantPending, "merchant_a", usd)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("got two accounts for one identity: %s and %s", first.ID, second.ID)
	}
}

func TestPostMovesMoney(t *testing.T) {
	l, _ := newLedger(t)
	world, merchant, fees := accounts(t, l)

	transfer, replayed, err := l.Post(context.Background(), ledger.PostRequest{
		IdempotencyKey: "capture-1",
		Kind:           "payment_capture",
		Reference:      "0xabc",
		Entries: []ledger.Entry{
			{AccountID: world.ID, Amount: -10_000_000},
			{AccountID: merchant.ID, Amount: 9_700_000},
			{AccountID: fees.ID, Amount: 300_000},
		},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if replayed {
		t.Error("first post reported as a replay")
	}
	if len(transfer.Entries) != 3 {
		t.Errorf("transfer has %d entries, want 3", len(transfer.Entries))
	}

	if got := balance(t, l, world.ID); got != -10_000_000 {
		t.Errorf("world balance = %s, want -10.000000", got)
	}
	if got := balance(t, l, merchant.ID); got != 9_700_000 {
		t.Errorf("merchant balance = %s, want 9.700000", got)
	}
	if got := balance(t, l, fees.ID); got != 300_000 {
		t.Errorf("fees balance = %s, want 0.300000", got)
	}
}

func TestPostRejectsUnbalanced(t *testing.T) {
	l, _ := newLedger(t)
	world, merchant, _ := accounts(t, l)

	_, _, err := l.Post(context.Background(), ledger.PostRequest{
		IdempotencyKey: "bad-1",
		Kind:           "payment_capture",
		Entries: []ledger.Entry{
			{AccountID: world.ID, Amount: -10_000_000},
			{AccountID: merchant.ID, Amount: 9_000_000},
		},
	})
	if !errors.Is(err, ledger.ErrUnbalanced) {
		t.Fatalf("want ErrUnbalanced, got %v", err)
	}
}

func TestPostRejectsSingleEntry(t *testing.T) {
	l, _ := newLedger(t)
	world, _, _ := accounts(t, l)

	_, _, err := l.Post(context.Background(), ledger.PostRequest{
		IdempotencyKey: "bad-2",
		Kind:           "payment_capture",
		Entries:        []ledger.Entry{{AccountID: world.ID, Amount: -1}},
	})
	if !errors.Is(err, ledger.ErrInvalidTransfer) {
		t.Fatalf("want ErrInvalidTransfer, got %v", err)
	}
}

func TestPostRejectsOverdraftOfInternalAccount(t *testing.T) {
	l, _ := newLedger(t)
	world, merchant, fees := accounts(t, l)
	ctx := context.Background()

	if _, _, err := l.Post(ctx, ledger.PostRequest{
		IdempotencyKey: "fund-1",
		Kind:           "payment_capture",
		Entries: []ledger.Entry{
			{AccountID: world.ID, Amount: -5_000_000},
			{AccountID: merchant.ID, Amount: 5_000_000},
		},
	}); err != nil {
		t.Fatalf("fund merchant: %v", err)
	}

	_, _, err := l.Post(ctx, ledger.PostRequest{
		IdempotencyKey: "overdraft-1",
		Kind:           "fee",
		Entries: []ledger.Entry{
			{AccountID: merchant.ID, Amount: -5_000_001},
			{AccountID: fees.ID, Amount: 5_000_001},
		},
	})
	if !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("want ErrInsufficientFunds, got %v", err)
	}
	if got := balance(t, l, merchant.ID); got != 5_000_000 {
		t.Errorf("merchant balance = %s after a rejected transfer, want 5.000000", got)
	}
}

func TestPostReplayMovesNoMoney(t *testing.T) {
	l, _ := newLedger(t)
	world, merchant, _ := accounts(t, l)
	ctx := context.Background()

	req := ledger.PostRequest{
		IdempotencyKey: "capture-2",
		Kind:           "payment_capture",
		Reference:      "0xdef",
		Entries: []ledger.Entry{
			{AccountID: world.ID, Amount: -4_000_000},
			{AccountID: merchant.ID, Amount: 4_000_000},
		},
	}

	first, replayed, err := l.Post(ctx, req)
	if err != nil || replayed {
		t.Fatalf("first post: transfer=%v replayed=%v err=%v", first, replayed, err)
	}

	// Same movement, entries submitted the other way round. The fingerprint is
	// over what the transfer does, so this is the same transfer.
	req.Entries[0], req.Entries[1] = req.Entries[1], req.Entries[0]

	second, replayed, err := l.Post(ctx, req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replayed {
		t.Error("second post was not reported as a replay")
	}
	if second.ID != first.ID {
		t.Errorf("replay returned transfer %s, want %s", second.ID, first.ID)
	}
	if got := balance(t, l, merchant.ID); got != 4_000_000 {
		t.Errorf("merchant balance = %s after a replay, want 4.000000", got)
	}
}

func TestPostRejectsIdempotencyKeyReuse(t *testing.T) {
	l, _ := newLedger(t)
	world, merchant, _ := accounts(t, l)
	ctx := context.Background()

	if _, _, err := l.Post(ctx, ledger.PostRequest{
		IdempotencyKey: "shared-key",
		Kind:           "payment_capture",
		Entries: []ledger.Entry{
			{AccountID: world.ID, Amount: -1_000_000},
			{AccountID: merchant.ID, Amount: 1_000_000},
		},
	}); err != nil {
		t.Fatalf("first post: %v", err)
	}

	_, _, err := l.Post(ctx, ledger.PostRequest{
		IdempotencyKey: "shared-key",
		Kind:           "payment_capture",
		Entries: []ledger.Entry{
			{AccountID: world.ID, Amount: -2_000_000},
			{AccountID: merchant.ID, Amount: 2_000_000},
		},
	})
	if !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Fatalf("want ErrIdempotencyConflict, got %v", err)
	}
}

// TestPostConcurrentDuplicatesCreditOnce is the test this repository exists
// for. Twenty callers deliver the same event at the same moment, as a chain
// watcher with retries eventually will, and the merchant is paid once.
func TestPostConcurrentDuplicatesCreditOnce(t *testing.T) {
	l, pool := newLedger(t)
	world, merchant, _ := accounts(t, l)
	ctx := context.Background()

	const callers = 20
	req := ledger.PostRequest{
		IdempotencyKey: "capture-concurrent",
		Kind:           "payment_capture",
		Reference:      "0xfeed",
		Entries: []ledger.Entry{
			{AccountID: world.ID, Amount: -7_500_000},
			{AccountID: merchant.ID, Amount: 7_500_000},
		},
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		ids     = map[uuid.UUID]int{}
		fresh   int
		errsOut []error
	)
	start := make(chan struct{})

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			transfer, replayed, err := l.Post(ctx, req)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errsOut = append(errsOut, err)
				return
			}
			ids[transfer.ID]++
			if !replayed {
				fresh++
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(errsOut) > 0 {
		t.Fatalf("%d of %d callers failed, first: %v", len(errsOut), callers, errsOut[0])
	}
	if len(ids) != 1 {
		t.Errorf("callers saw %d distinct transfers, want 1", len(ids))
	}
	if fresh != 1 {
		t.Errorf("%d callers believed they posted it, want exactly 1", fresh)
	}
	if got := balance(t, l, merchant.ID); got != 7_500_000 {
		t.Errorf("merchant balance = %s, want 7.500000 credited once", got)
	}

	var entryCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM entries`).Scan(&entryCount); err != nil {
		t.Fatalf("count entries: %v", err)
	}
	if entryCount != 2 {
		t.Errorf("ledger holds %d entries, want 2", entryCount)
	}
}

// TestPostConcurrentOppositeDirectionsDoNotDeadlock drives transfers both ways
// between the same two accounts at once. Without a fixed lock order this is
// the classic deadlock, and Postgres would abort roughly half of them.
func TestPostConcurrentOppositeDirectionsDoNotDeadlock(t *testing.T) {
	l, _ := newLedger(t)
	world, merchant, fees := accounts(t, l)
	ctx := context.Background()

	if _, _, err := l.Post(ctx, ledger.PostRequest{
		IdempotencyKey: "float",
		Kind:           "payment_capture",
		Entries: []ledger.Entry{
			{AccountID: world.ID, Amount: -1_000_000_000},
			{AccountID: merchant.ID, Amount: 1_000_000_000},
		},
	}); err != nil {
		t.Fatalf("fund merchant: %v", err)
	}
	if _, _, err := l.Post(ctx, ledger.PostRequest{
		IdempotencyKey: "float-fees",
		Kind:           "payment_capture",
		Entries: []ledger.Entry{
			{AccountID: world.ID, Amount: -1_000_000_000},
			{AccountID: fees.ID, Amount: 1_000_000_000},
		},
	}); err != nil {
		t.Fatalf("fund fees: %v", err)
	}

	const rounds = 40
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	start := make(chan struct{})

	for i := 0; i < rounds; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			from, to := merchant, fees
			if i%2 == 1 {
				from, to = fees, merchant
			}
			_, _, err := l.Post(ctx, ledger.PostRequest{
				IdempotencyKey: fmt.Sprintf("shuffle-%d", i),
				Kind:           "fee",
				Entries: []ledger.Entry{
					{AccountID: from.ID, Amount: -1_000},
					{AccountID: to.ID, Amount: 1_000},
				},
			})
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d of %d transfers failed, first: %v", len(errs), rounds, errs[0])
	}

	total := balance(t, l, merchant.ID) + balance(t, l, fees.ID)
	if total != 2_000_000_000 {
		t.Errorf("merchant plus fees = %s, want 2000.000000: money was created or destroyed", total)
	}
}

// TestDatabaseRejectsUnbalancedTransferWrittenDirectly bypasses the ledger
// package entirely and writes half a transfer with SQL. The deferred trigger
// is the reason a future service, a migration script or a hand-run UPDATE
// cannot break the invariant that the Go code is careful about.
func TestDatabaseRejectsUnbalancedTransferWrittenDirectly(t *testing.T) {
	l, pool := newLedger(t)
	world, _, _ := accounts(t, l)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var transferID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO transfers (idempotency_key, kind, fingerprint)
		VALUES ('handwritten', 'payment_capture', 'none') RETURNING id`).Scan(&transferID)
	if err != nil {
		t.Fatalf("insert transfer: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO entries (transfer_id, account_id, amount) VALUES ($1, $2, $3)`,
		transferID, world.ID, int64(-1_000_000)); err != nil {
		t.Fatalf("insert entry: %v", err)
	}

	err = tx.Commit(ctx)
	if err == nil {
		t.Fatal("commit succeeded, the zero-sum trigger did not fire")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "transfer_balanced" {
		t.Fatalf("want a transfer_balanced violation, got %v", err)
	}
	if !errors.Is(ledger.Classify(err), ledger.ErrUnbalanced) {
		t.Errorf("Classify did not map the violation to ErrUnbalanced")
	}
}

// TestDatabaseRejectsEmptyTransfer covers the case the entries trigger cannot
// see, because it only ever runs when an entry is written.
func TestDatabaseRejectsEmptyTransfer(t *testing.T) {
	_, pool := newLedger(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO transfers (idempotency_key, kind, fingerprint)
		VALUES ('empty', 'payment_capture', 'none')`); err != nil {
		t.Fatalf("insert transfer: %v", err)
	}
	if err := tx.Commit(ctx); err == nil {
		t.Fatal("committed a transfer with no entries")
	}
}
