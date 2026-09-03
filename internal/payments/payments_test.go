package payments_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/owenpkent/tender/internal/ledger"
	"github.com/owenpkent/tender/internal/money"
	"github.com/owenpkent/tender/internal/payments"
	"github.com/owenpkent/tender/internal/pgtest"
)

const usd = "USDC"

func newService(t *testing.T) (*payments.Service, *ledger.Ledger) {
	t.Helper()
	pool := pgtest.Pool(t)
	l := ledger.New(pool)
	return payments.New(pool, l), l
}

func merchantBalance(t *testing.T, l *ledger.Ledger, merchantRef string) money.Amount {
	t.Helper()
	acct, err := l.EnsureAccount(context.Background(), ledger.MerchantPending, merchantRef, usd)
	if err != nil {
		t.Fatalf("merchant account: %v", err)
	}
	b, err := l.Balance(context.Background(), acct.ID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	return b
}

func TestCreateThenConfirm(t *testing.T) {
	svc, l := newService(t)
	ctx := context.Background()

	p, err := svc.Create(ctx, "merchant_a", 25_000_000, usd, time.Hour)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.Status != payments.StatusPending {
		t.Errorf("new payment status = %s, want pending", p.Status)
	}
	if p.CaptureTransferID != uuid.Nil {
		t.Errorf("new payment already has a capture transfer")
	}

	confirmed, err := svc.Confirm(ctx, p.ID, "0xdeadbeef")
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if confirmed.Status != payments.StatusConfirmed {
		t.Errorf("status = %s, want confirmed", confirmed.Status)
	}
	if confirmed.CaptureTransferID == uuid.Nil {
		t.Error("confirmed payment has no capture transfer")
	}
	if got := merchantBalance(t, l, "merchant_a"); got != 25_000_000 {
		t.Errorf("merchant pending balance = %s, want 25.000000", got)
	}

	transfer, err := l.Transfer(ctx, confirmed.CaptureTransferID)
	if err != nil {
		t.Fatalf("read capture transfer: %v", err)
	}
	if transfer.Reference != "0xdeadbeef" {
		t.Errorf("transfer reference = %q, want the chain transaction", transfer.Reference)
	}
}

func TestCreateRejectsBadInput(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()

	cases := []struct {
		name     string
		merchant string
		amount   money.Amount
		currency string
		ttl      time.Duration
	}{
		{"no merchant", "", 1, usd, time.Hour},
		{"zero amount", "merchant_a", 0, usd, time.Hour},
		{"negative amount", "merchant_a", -1, usd, time.Hour},
		{"no currency", "merchant_a", 1, "", time.Hour},
		{"no ttl", "merchant_a", 1, usd, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.Create(ctx, c.merchant, c.amount, c.currency, c.ttl)
			if !errors.Is(err, payments.ErrInvalidPayment) {
				t.Fatalf("want ErrInvalidPayment, got %v", err)
			}
		})
	}
}

func TestConfirmTwiceCreditsOnce(t *testing.T) {
	svc, l := newService(t)
	ctx := context.Background()

	p, err := svc.Create(ctx, "merchant_a", 12_000_000, usd, time.Hour)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	first, err := svc.Confirm(ctx, p.ID, "0xabc")
	if err != nil {
		t.Fatalf("first confirm: %v", err)
	}
	second, err := svc.Confirm(ctx, p.ID, "0xabc")
	if err != nil {
		t.Fatalf("second confirm: %v", err)
	}
	if first.CaptureTransferID != second.CaptureTransferID {
		t.Errorf("redelivery posted a second transfer: %s then %s",
			first.CaptureTransferID, second.CaptureTransferID)
	}
	if got := merchantBalance(t, l, "merchant_a"); got != 12_000_000 {
		t.Errorf("merchant balance = %s after a redelivered event, want 12.000000", got)
	}
}

func TestConfirmWithDifferentChainTxIsRejected(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()

	p, err := svc.Create(ctx, "merchant_a", 1_000_000, usd, time.Hour)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Confirm(ctx, p.ID, "0xaaa"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if _, err := svc.Confirm(ctx, p.ID, "0xbbb"); !errors.Is(err, payments.ErrChainTxMismatch) {
		t.Fatalf("want ErrChainTxMismatch, got %v", err)
	}
}

// TestOneChainTxCannotPayTwoPayments is the other half of the idempotency
// story. The ledger key stops a replayed call to one payment; the unique index
// on chain_tx stops one transaction being credited to two of them.
func TestOneChainTxCannotPayTwoPayments(t *testing.T) {
	svc, l := newService(t)
	ctx := context.Background()

	first, err := svc.Create(ctx, "merchant_a", 3_000_000, usd, time.Hour)
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, err := svc.Create(ctx, "merchant_a", 3_000_000, usd, time.Hour)
	if err != nil {
		t.Fatalf("create second: %v", err)
	}

	if _, err := svc.Confirm(ctx, first.ID, "0xonce"); err != nil {
		t.Fatalf("confirm first: %v", err)
	}
	if _, err := svc.Confirm(ctx, second.ID, "0xonce"); !errors.Is(err, payments.ErrChainTxUsedByOther) {
		t.Fatalf("want ErrChainTxUsedByOther, got %v", err)
	}
	if got := merchantBalance(t, l, "merchant_a"); got != 3_000_000 {
		t.Errorf("merchant balance = %s, want one payment credited", got)
	}
}

// TestConcurrentConfirmsCreditOnce uses more callers than the pool has
// connections, on purpose. Confirm holds a row lock while it works, so a
// version of it that reaches into the pool for a second connection would park
// every caller on a connection that never comes free: the service would stop
// without erroring. A timeout is set so that regression fails here instead of
// hanging a CI run.
func TestConcurrentConfirmsCreditOnce(t *testing.T) {
	svc, l := newService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p, err := svc.Create(ctx, "merchant_a", 8_000_000, usd, time.Hour)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	const callers = 24
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		errs  []error
		seen  = map[uuid.UUID]int{}
		start = make(chan struct{})
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			confirmed, err := svc.Confirm(ctx, p.ID, "0xrace")

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			seen[confirmed.CaptureTransferID]++
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d of %d confirms failed, first: %v", len(errs), callers, errs[0])
	}
	if len(seen) != 1 {
		t.Errorf("callers saw %d distinct capture transfers, want 1", len(seen))
	}
	if got := merchantBalance(t, l, "merchant_a"); got != 8_000_000 {
		t.Errorf("merchant balance = %s, want 8.000000 credited once", got)
	}
}

// TestExpiredPaymentStillConfirms records the product decision in the code:
// funds that are already onchain are recorded whether or not the checkout page
// had given up waiting for them.
func TestExpiredPaymentStillConfirms(t *testing.T) {
	svc, l := newService(t)
	ctx := context.Background()

	p, err := svc.Create(ctx, "merchant_a", 2_000_000, usd, time.Nanosecond)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	n, err := svc.ExpirePending(ctx)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired %d payments, want 1", n)
	}
	expired, err := svc.Get(ctx, p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if expired.Status != payments.StatusExpired {
		t.Fatalf("status = %s, want expired", expired.Status)
	}

	if _, err := svc.Confirm(ctx, p.ID, "0xlate"); err != nil {
		t.Fatalf("confirm after expiry: %v", err)
	}
	if got := merchantBalance(t, l, "merchant_a"); got != 2_000_000 {
		t.Errorf("merchant balance = %s, want the late payment credited", got)
	}
}

func TestGetUnknownPayment(t *testing.T) {
	svc, _ := newService(t)
	if _, err := svc.Get(context.Background(), uuid.New()); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
