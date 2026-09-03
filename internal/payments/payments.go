// Package payments turns onchain events into ledger entries.
//
// Phase 1 has no chain in it. Confirm is called by hand from the CLI with a
// transaction hash, standing in for the watcher that will call it later. The
// interesting part is not where the event comes from, it is that recording it
// twice costs the merchant nothing and that a payment can never be marked
// confirmed without the entry that captured it.
package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/owenpkent/tender/internal/ledger"
	"github.com/owenpkent/tender/internal/money"
)

// Errors callers are expected to branch on.
var (
	ErrInvalidPayment     = errors.New("payments: invalid payment")
	ErrNotFound           = errors.New("payments: payment not found")
	ErrChainTxMismatch    = errors.New("payments: payment already confirmed by a different transaction")
	ErrChainTxUsedByOther = errors.New("payments: onchain transaction already credited to another payment")
)

// Status is the lifecycle of a payment request.
type Status string

// The payment statuses, matching the payment_status enum in migration 00002.
const (
	StatusPending   Status = "pending"
	StatusConfirmed Status = "confirmed"
	StatusExpired   Status = "expired"
)

// Payment is one request for money from one merchant.
type Payment struct {
	ID                uuid.UUID
	MerchantRef       string
	Amount            money.Amount
	Currency          string
	Status            Status
	ChainTx           string
	CaptureTransferID uuid.UUID
	CreatedAt         time.Time
	UpdatedAt         time.Time
	ExpiresAt         time.Time
}

// Service creates payments and records their confirmation.
type Service struct {
	pool   *pgxpool.Pool
	ledger *ledger.Ledger
}

// New returns a service over pool, posting into l.
func New(pool *pgxpool.Pool, l *ledger.Ledger) *Service {
	return &Service{pool: pool, ledger: l}
}

// Create opens a pending payment that expires after ttl.
func (s *Service) Create(ctx context.Context, merchantRef string, amount money.Amount, currency string, ttl time.Duration) (*Payment, error) {
	switch {
	case strings.TrimSpace(merchantRef) == "":
		return nil, fmt.Errorf("%w: merchant reference is required", ErrInvalidPayment)
	case amount <= 0:
		return nil, fmt.Errorf("%w: amount must be positive, got %s", ErrInvalidPayment, amount)
	case strings.TrimSpace(currency) == "":
		return nil, fmt.Errorf("%w: currency is required", ErrInvalidPayment)
	case ttl <= 0:
		return nil, fmt.Errorf("%w: ttl must be positive", ErrInvalidPayment)
	}

	// Both accounts are created up front so that Confirm never has to create
	// one while holding the payment row lock.
	if _, err := s.ledger.EnsureAccount(ctx, ledger.World, chainOwnerRef, currency); err != nil {
		return nil, err
	}
	if _, err := s.ledger.EnsureAccount(ctx, ledger.MerchantPending, merchantRef, currency); err != nil {
		return nil, err
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO payments (merchant_ref, amount, currency, expires_at)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4::double precision))
		RETURNING `+paymentColumns,
		merchantRef, int64(amount), currency, ttl.Seconds())

	p, err := scanPayment(row)
	if err != nil {
		return nil, fmt.Errorf("create payment: %w", err)
	}
	return p, nil
}

// Get reads a payment.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (*Payment, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+paymentColumns+` FROM payments WHERE id = $1`, id)
	p, err := scanPayment(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("read payment: %w", err)
	}
	return p, nil
}

// Confirm records that chainTx paid this payment, crediting the merchant's
// pending balance in the same transaction that marks the payment confirmed.
//
// Calling it again with the same transaction hash is a no-op that returns the
// payment as it stands, because a chain watcher will deliver the same event
// more than once and it must not be able to pay a merchant twice.
func (s *Service) Confirm(ctx context.Context, id uuid.UUID, chainTx string) (*Payment, error) {
	if strings.TrimSpace(chainTx) == "" {
		return nil, fmt.Errorf("%w: chain transaction reference is required", ErrInvalidPayment)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the payment first, then the balances underneath it. Every path that
	// touches both takes them in that order.
	row := tx.QueryRow(ctx, `SELECT `+paymentColumns+` FROM payments WHERE id = $1 FOR UPDATE`, id)
	p, err := scanPayment(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("lock payment: %w", err)
	}

	if p.Status == StatusConfirmed {
		if p.ChainTx != chainTx {
			return nil, fmt.Errorf("%w: confirmed by %s, asked to confirm with %s",
				ErrChainTxMismatch, p.ChainTx, chainTx)
		}
		return p, nil
	}

	// An expired payment is still confirmed if the money arrived. Expiry
	// governs whether the checkout page will still take a payment; it cannot
	// govern a transaction that is already settled on a chain, and refusing to
	// record one would not make the funds go anywhere. The refund path, which
	// arrives with the settlement workflow, is how a merchant gives it back.
	// EnsureAccountTx, not EnsureAccount: the payment row is locked, so asking
	// the pool for a second connection here would deadlock the service once
	// concurrent confirmations outnumber the pool.
	world, err := s.ledger.EnsureAccountTx(ctx, tx, ledger.World, chainOwnerRef, p.Currency)
	if err != nil {
		return nil, err
	}
	pending, err := s.ledger.EnsureAccountTx(ctx, tx, ledger.MerchantPending, p.MerchantRef, p.Currency)
	if err != nil {
		return nil, err
	}

	transfer, _, err := s.ledger.PostTx(ctx, tx, ledger.PostRequest{
		// Keyed on the payment, not on the delivery of the event, so every
		// redelivery of every duplicate lands on the same key.
		IdempotencyKey: "payment:" + p.ID.String() + ":capture",
		Kind:           "payment_capture",
		Reference:      chainTx,
		Entries: []ledger.Entry{
			{AccountID: world.ID, Amount: -p.Amount},
			{AccountID: pending.ID, Amount: p.Amount},
		},
	})
	if err != nil {
		return nil, err
	}

	row = tx.QueryRow(ctx, `
		UPDATE payments
		SET status = 'confirmed', chain_tx = $2, capture_transfer_id = $3, updated_at = now()
		WHERE id = $1
		RETURNING `+paymentColumns,
		p.ID, chainTx, transfer.ID)

	confirmed, err := scanPayment(row)
	if err != nil {
		return nil, fmt.Errorf("confirm payment: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, classify(err)
	}
	return confirmed, nil
}

// ExpirePending marks every pending payment past its expiry as expired and
// reports how many it touched. It is safe to run repeatedly and on more than
// one instance at once, which is what a scheduled job has to be.
func (s *Service) ExpirePending(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE payments SET status = 'expired', updated_at = now()
		WHERE status = 'pending' AND expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("expire pending: %w", err)
	}
	return tag.RowsAffected(), nil
}

// chainOwnerRef is the owner of the world account that inbound funds are
// debited from. One per currency is enough while there is one chain.
const chainOwnerRef = "chain"

// paymentColumns is cast explicitly rather than left to inference: status is a
// Postgres enum, and asking for its text is clearer than relying on the driver
// to guess what a type it has never seen should become.
const paymentColumns = `id, merchant_ref, amount, currency, status::text,
	coalesce(chain_tx, ''), coalesce(capture_transfer_id, '00000000-0000-0000-0000-000000000000'::uuid),
	created_at, updated_at, expires_at`

func scanPayment(row pgx.Row) (*Payment, error) {
	var p Payment
	var amount int64
	err := row.Scan(&p.ID, &p.MerchantRef, &amount, &p.Currency, &p.Status,
		&p.ChainTx, &p.CaptureTransferID, &p.CreatedAt, &p.UpdatedAt, &p.ExpiresAt)
	if err != nil {
		return nil, err
	}
	p.Amount = money.Amount(amount)
	return &p, nil
}

func classify(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "payments_chain_tx_unique" {
		return fmt.Errorf("%w: %s", ErrChainTxUsedByOther, pgErr.Message)
	}
	return ledger.Classify(err)
}
