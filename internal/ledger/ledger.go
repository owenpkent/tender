// Package ledger is a double-entry ledger on Postgres.
//
// Two rules hold everywhere in this package. Money is only ever moved between
// accounts, never created, so the entries of a transfer sum to zero. And every
// write carries an idempotency key, because a payment rail is a place where
// the same message arrives twice and the second one must not cost anybody
// anything.
package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/owenpkent/tender/internal/money"
)

// Errors callers are expected to branch on.
var (
	ErrInvalidTransfer     = errors.New("ledger: invalid transfer")
	ErrUnbalanced          = errors.New("ledger: entries do not sum to zero")
	ErrIdempotencyConflict = errors.New("ledger: idempotency key reused with different entries")
	ErrInsufficientFunds   = errors.New("ledger: insufficient funds")
	ErrAccountNotFound     = errors.New("ledger: account not found")
	ErrTransferNotFound    = errors.New("ledger: transfer not found")
)

// Kind enumerates the account kinds in the schema. Only world accounts may go
// negative: they represent the outside of the system, so a debit against one
// is money arriving from a chain rather than money the platform does not have.
type Kind string

// The account kinds, matching the account_kind enum in migration 00001.
const (
	World             Kind = "world"
	MerchantPending   Kind = "merchant_pending"
	MerchantAvailable Kind = "merchant_available"
	Fees              Kind = "fees"
	RefundsPayable    Kind = "refunds_payable"
)

// Account is one balance-bearing bucket.
type Account struct {
	ID       uuid.UUID
	Kind     Kind
	OwnerRef string
	Currency string
}

// Entry is one leg of a transfer. Positive credits the account, negative
// debits it.
type Entry struct {
	AccountID uuid.UUID
	Amount    money.Amount
}

// Transfer is a set of entries that were committed together.
type Transfer struct {
	ID             uuid.UUID
	IdempotencyKey string
	Kind           string
	Reference      string
	CreatedAt      time.Time
	Entries        []Entry
}

// PostRequest describes a movement of money.
type PostRequest struct {
	// IdempotencyKey must be derived from the event being recorded, not
	// generated per attempt, or a retry writes a second transfer.
	IdempotencyKey string
	Kind           string
	Reference      string
	Entries        []Entry
}

// Ledger posts transfers against a Postgres pool.
type Ledger struct {
	pool *pgxpool.Pool
}

// New returns a ledger backed by pool.
func New(pool *pgxpool.Pool) *Ledger { return &Ledger{pool: pool} }

// EnsureAccount returns the account with this identity, creating it if it does
// not exist yet.
func (l *Ledger) EnsureAccount(ctx context.Context, kind Kind, ownerRef, currency string) (Account, error) {
	return ensureAccount(ctx, l.pool, kind, ownerRef, currency)
}

// EnsureAccountTx is EnsureAccount inside a transaction the caller owns.
//
// A caller that is already holding a lock must use this one. Reaching into the
// pool for a second connection while holding the first is how a service
// deadlocks itself under load: past a certain number of concurrent callers,
// every connection in the pool is held by somebody waiting for a connection
// that will never come free. Nothing times out and nothing errors, the service
// simply stops.
func (l *Ledger) EnsureAccountTx(ctx context.Context, tx pgx.Tx, kind Kind, ownerRef, currency string) (Account, error) {
	return ensureAccount(ctx, tx, kind, ownerRef, currency)
}

func ensureAccount(ctx context.Context, q querier, kind Kind, ownerRef, currency string) (Account, error) {
	// Read first. An account that already exists, which is nearly always, then
	// costs no row lock. Going straight to ON CONFLICT DO UPDATE would write a
	// new row version every time and make every concurrent confirmation for
	// one merchant queue behind the others for no reason.
	var a Account
	err := q.QueryRow(ctx, `
		SELECT id, kind::text, owner_ref, currency FROM accounts
		WHERE kind = $1::account_kind AND owner_ref = $2 AND currency = $3`,
		string(kind), ownerRef, currency,
	).Scan(&a.ID, &a.Kind, &a.OwnerRef, &a.Currency)
	if err == nil {
		return a, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Account{}, fmt.Errorf("read account: %w", err)
	}

	// The unique constraint on (kind, owner_ref, currency) is what makes two
	// concurrent callers agree on one account rather than racing to make two.
	err = q.QueryRow(ctx, `
		INSERT INTO accounts (kind, owner_ref, currency)
		VALUES ($1::account_kind, $2, $3)
		ON CONFLICT (kind, owner_ref, currency)
			DO UPDATE SET owner_ref = accounts.owner_ref
		RETURNING id, kind::text, owner_ref, currency`,
		string(kind), ownerRef, currency,
	).Scan(&a.ID, &a.Kind, &a.OwnerRef, &a.Currency)
	if err != nil {
		return Account{}, fmt.Errorf("ensure account: %w", err)
	}
	return a, nil
}

// Balance reads the current balance of an account.
func (l *Ledger) Balance(ctx context.Context, accountID uuid.UUID) (money.Amount, error) {
	var b int64
	err := l.pool.QueryRow(ctx,
		`SELECT balance FROM account_balances WHERE account_id = $1`, accountID).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
	}
	if err != nil {
		return 0, fmt.Errorf("read balance: %w", err)
	}
	return money.Amount(b), nil
}

// Post commits a transfer and reports whether it was a replay of one that had
// already been committed under the same idempotency key.
//
// A replay is not an error. It returns the transfer that already exists and
// moves no money, which is what lets a caller retry a timed-out request
// without having to know whether the first attempt landed. Reusing a key for
// genuinely different money is an error, because that is a bug in the caller
// and quietly returning the wrong transfer would hide it.
func (l *Ledger) Post(ctx context.Context, req PostRequest) (*Transfer, bool, error) {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	transfer, replayed, err := l.PostTx(ctx, tx, req)
	if err != nil {
		return nil, false, err
	}

	// The zero-sum triggers are deferred, so this is where they run. A commit
	// error here is the database refusing a transfer the application thought
	// was fine, which is exactly the case the backstop exists for.
	if err := tx.Commit(ctx); err != nil {
		return nil, false, Classify(err)
	}
	return transfer, replayed, nil
}

// PostTx is Post inside a transaction the caller owns, so that a transfer and
// whatever else it is bookkeeping for commit together or not at all. A payment
// marked confirmed without its ledger entry, or an entry without the payment,
// is the specific bug this whole service exists to not have.
//
// The caller commits. Because the zero-sum triggers are deferred to commit
// time, the transfer this returns is not durable until that succeeds, and the
// caller should run a commit error through Classify.
func (l *Ledger) PostTx(ctx context.Context, tx pgx.Tx, req PostRequest) (*Transfer, bool, error) {
	deltas, err := req.netDeltas()
	if err != nil {
		return nil, false, err
	}
	fingerprint := fingerprintOf(req.Kind, req.Reference, deltas)

	// ON CONFLICT DO UPDATE rather than DO NOTHING, for two reasons. DO
	// NOTHING returns no row, so there would be nothing to hand back on a
	// replay. And DO NOTHING does not block on a conflicting row that a
	// concurrent transaction has inserted but not yet committed, so two
	// simultaneous retries could both see nothing and both decide they were
	// first. DO UPDATE waits for that transaction to finish and then sees its
	// row, which is the behaviour idempotency actually needs.
	//
	// xmax = 0 separates the two paths: a freshly inserted row has no deleting
	// transaction recorded against it, an updated one carries ours.
	var (
		transferID  uuid.UUID
		storedPrint string
		inserted    bool
	)
	err = tx.QueryRow(ctx, `
		INSERT INTO transfers (idempotency_key, kind, reference, fingerprint)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (idempotency_key)
			DO UPDATE SET idempotency_key = transfers.idempotency_key
		RETURNING id, fingerprint, xmax = 0`,
		req.IdempotencyKey, req.Kind, req.Reference, fingerprint,
	).Scan(&transferID, &storedPrint, &inserted)
	if err != nil {
		return nil, false, fmt.Errorf("claim idempotency key: %w", err)
	}

	if !inserted {
		if storedPrint != fingerprint {
			return nil, false, fmt.Errorf("%w: key %q", ErrIdempotencyConflict, req.IdempotencyKey)
		}
		existing, err := loadTransfer(ctx, tx, transferID)
		if err != nil {
			return nil, false, err
		}
		return existing, true, nil
	}

	// Lock the balance rows in ascending account order, one statement each.
	// Every transaction in this process takes the same locks in the same
	// order, which is what makes concurrent transfers between the same pair of
	// accounts wait for each other instead of deadlocking.
	locked := sortedAccounts(deltas)
	for _, id := range locked {
		var current int64
		err := tx.QueryRow(ctx,
			`SELECT balance FROM account_balances WHERE account_id = $1 FOR UPDATE`, id).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, fmt.Errorf("%w: %s", ErrAccountNotFound, id)
		}
		if err != nil {
			return nil, false, fmt.Errorf("lock balance %s: %w", id, err)
		}
	}

	batch := &pgx.Batch{}
	for _, e := range req.Entries {
		batch.Queue(
			`INSERT INTO entries (transfer_id, account_id, amount) VALUES ($1, $2, $3)`,
			transferID, e.AccountID, int64(e.Amount))
	}
	for _, id := range locked {
		batch.Queue(`
			UPDATE account_balances
			SET balance = balance + $2, version = version + 1, updated_at = now()
			WHERE account_id = $1`,
			id, int64(deltas[id]))
	}

	results := tx.SendBatch(ctx, batch)
	for i := 0; i < batch.Len(); i++ {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return nil, false, Classify(err)
		}
	}
	if err := results.Close(); err != nil {
		return nil, false, Classify(err)
	}

	posted, err := loadTransfer(ctx, tx, transferID)
	if err != nil {
		return nil, false, err
	}
	return posted, false, nil
}

// Transfer reads a committed transfer and its entries.
func (l *Ledger) Transfer(ctx context.Context, id uuid.UUID) (*Transfer, error) {
	return loadTransfer(ctx, l.pool, id)
}

// querier is the part of pgx that both a pool and a transaction satisfy, so
// loadTransfer works inside or outside a transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func loadTransfer(ctx context.Context, q querier, id uuid.UUID) (*Transfer, error) {
	var t Transfer
	err := q.QueryRow(ctx, `
		SELECT id, idempotency_key, kind, reference, created_at
		FROM transfers WHERE id = $1`, id,
	).Scan(&t.ID, &t.IdempotencyKey, &t.Kind, &t.Reference, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrTransferNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("read transfer: %w", err)
	}

	rows, err := q.Query(ctx,
		`SELECT account_id, amount FROM entries WHERE transfer_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("read entries: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var e Entry
		var amount int64
		if err := rows.Scan(&e.AccountID, &amount); err != nil {
			return nil, fmt.Errorf("scan entry: %w", err)
		}
		e.Amount = money.Amount(amount)
		t.Entries = append(t.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read entries: %w", err)
	}
	return &t, nil
}

// netDeltas validates the request and collapses it to one signed amount per
// account. An account may legitimately appear in more than one entry, so the
// balance update works from the net while the entries table keeps every leg as
// it was submitted.
func (r PostRequest) netDeltas() (map[uuid.UUID]money.Amount, error) {
	switch {
	case strings.TrimSpace(r.IdempotencyKey) == "":
		return nil, fmt.Errorf("%w: idempotency key is required", ErrInvalidTransfer)
	case len(r.IdempotencyKey) > 255:
		return nil, fmt.Errorf("%w: idempotency key is longer than 255 bytes", ErrInvalidTransfer)
	case strings.TrimSpace(r.Kind) == "":
		return nil, fmt.Errorf("%w: kind is required", ErrInvalidTransfer)
	case len(r.Entries) < 2:
		return nil, fmt.Errorf("%w: a transfer needs at least 2 entries, got %d", ErrInvalidTransfer, len(r.Entries))
	}

	deltas := make(map[uuid.UUID]money.Amount, len(r.Entries))
	var sum int64
	for i, e := range r.Entries {
		if e.Amount == 0 {
			return nil, fmt.Errorf("%w: entry %d has amount 0", ErrInvalidTransfer, i)
		}
		if e.AccountID == uuid.Nil {
			return nil, fmt.Errorf("%w: entry %d has no account", ErrInvalidTransfer, i)
		}
		next, ok := addChecked(sum, int64(e.Amount))
		if !ok {
			return nil, fmt.Errorf("%w: entries overflow int64", ErrInvalidTransfer)
		}
		sum = next

		netted, ok := addChecked(int64(deltas[e.AccountID]), int64(e.Amount))
		if !ok {
			return nil, fmt.Errorf("%w: entries overflow int64", ErrInvalidTransfer)
		}
		deltas[e.AccountID] = money.Amount(netted)
	}
	if sum != 0 {
		return nil, fmt.Errorf("%w: sum is %d", ErrUnbalanced, sum)
	}

	for id, d := range deltas {
		if d == 0 {
			delete(deltas, id)
		}
	}
	if len(deltas) < 2 {
		return nil, fmt.Errorf("%w: entries net out to no movement", ErrInvalidTransfer)
	}
	return deltas, nil
}

func addChecked(a, b int64) (int64, bool) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, false
	}
	return a + b, true
}

func sortedAccounts(deltas map[uuid.UUID]money.Amount) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(deltas))
	for id := range deltas {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}

// fingerprintOf hashes what the transfer does rather than how it was written,
// so a retry that expresses the same movement with its entries in a different
// order is recognised as the same transfer.
func fingerprintOf(kind, reference string, deltas map[uuid.UUID]money.Amount) string {
	var b strings.Builder
	b.WriteString(kind)
	b.WriteByte('\n')
	b.WriteString(reference)
	b.WriteByte('\n')
	for _, id := range sortedAccounts(deltas) {
		fmt.Fprintf(&b, "%s=%d\n", id, deltas[id])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// Classify turns a Postgres constraint violation into the error a caller
// should branch on. The constraint names are part of the contract between the
// migration and this file: renaming one in SQL without changing it here is a
// quiet downgrade from a typed error to an opaque one.
//
// It is exported because a caller of PostTx owns the commit, and the commit is
// where the deferred zero-sum check reports.
func Classify(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Errorf("post transfer: %w", err)
	}
	switch pgErr.ConstraintName {
	case "balance_non_negative":
		return fmt.Errorf("%w: %s", ErrInsufficientFunds, pgErr.Message)
	case "transfer_balanced":
		return fmt.Errorf("%w: %s", ErrUnbalanced, pgErr.Message)
	case "entries_account_id_fkey":
		return fmt.Errorf("%w: %s", ErrAccountNotFound, pgErr.Message)
	default:
		return fmt.Errorf("post transfer: %w", err)
	}
}
