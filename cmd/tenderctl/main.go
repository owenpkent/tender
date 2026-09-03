// Command tenderctl drives the ledger from a terminal.
//
// Phase 1 has no chain watcher and no HTTP surface, so `payment confirm` is
// where an onchain event enters the system. Confirming the same payment twice
// from a shell is the fastest way to see that a redelivered event costs the
// merchant nothing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/owenpkent/tender/internal/db"
	"github.com/owenpkent/tender/internal/ledger"
	"github.com/owenpkent/tender/internal/money"
	"github.com/owenpkent/tender/internal/payments"
)

const usage = `tenderctl drives the tender ledger.

Usage:
  tenderctl migrate
  tenderctl account balance -kind <kind> -owner <ref> [-currency USDC]
  tenderctl payment create  -merchant <ref> -amount <decimal> [-currency USDC] [-ttl 1h]
  tenderctl payment confirm -id <uuid> -tx <chain tx>
  tenderctl payment get     -id <uuid>
  tenderctl payment expire

The database URL comes from TENDER_DSN, for example
  postgres://tender:tender@localhost:5432/tender?sslmode=disable
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "tenderctl:", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Print(usage)
		return errors.New("no command given")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := os.Getenv("TENDER_DSN")
	if dsn == "" {
		return errors.New("TENDER_DSN is not set")
	}

	pool, err := db.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	l := ledger.New(pool)
	svc := payments.New(pool, l)

	switch args[0] {
	case "migrate":
		if err := db.Migrate(ctx, pool); err != nil {
			return err
		}
		v, err := db.Version(ctx, pool)
		if err != nil {
			return err
		}
		fmt.Printf("schema is at version %d\n", v)
		return nil

	case "account":
		return accountCmd(ctx, l, args[1:])

	case "payment":
		return paymentCmd(ctx, svc, args[1:])

	case "-h", "--help", "help":
		fmt.Print(usage)
		return nil

	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func accountCmd(ctx context.Context, l *ledger.Ledger, args []string) error {
	if len(args) == 0 || args[0] != "balance" {
		return errors.New("usage: tenderctl account balance -kind <kind> -owner <ref>")
	}

	fs := flag.NewFlagSet("account balance", flag.ContinueOnError)
	kind := fs.String("kind", string(ledger.MerchantPending), "account kind")
	owner := fs.String("owner", "", "owner reference")
	currency := fs.String("currency", "USDC", "currency")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *owner == "" {
		return errors.New("-owner is required")
	}

	account, err := l.EnsureAccount(ctx, ledger.Kind(*kind), *owner, *currency)
	if err != nil {
		return err
	}
	balance, err := l.Balance(ctx, account.ID)
	if err != nil {
		return err
	}
	fmt.Printf("%s %s %s  %s %s\n", account.Kind, account.OwnerRef, account.ID, balance, account.Currency)
	return nil
}

func paymentCmd(ctx context.Context, svc *payments.Service, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tenderctl payment <create|confirm|get|expire>")
	}

	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("payment create", flag.ContinueOnError)
		merchant := fs.String("merchant", "", "merchant reference")
		amount := fs.String("amount", "", "amount, for example 12.50")
		currency := fs.String("currency", "USDC", "currency")
		ttl := fs.Duration("ttl", time.Hour, "how long the payment stays payable")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		value, err := money.Parse(*amount)
		if err != nil {
			return err
		}
		p, err := svc.Create(ctx, *merchant, value, *currency, *ttl)
		if err != nil {
			return err
		}
		printPayment(p)
		return nil

	case "confirm":
		fs := flag.NewFlagSet("payment confirm", flag.ContinueOnError)
		id := fs.String("id", "", "payment id")
		tx := fs.String("tx", "", "onchain transaction reference")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		paymentID, err := uuid.Parse(*id)
		if err != nil {
			return fmt.Errorf("parse -id: %w", err)
		}
		p, err := svc.Confirm(ctx, paymentID, *tx)
		if err != nil {
			return err
		}
		printPayment(p)
		return nil

	case "get":
		fs := flag.NewFlagSet("payment get", flag.ContinueOnError)
		id := fs.String("id", "", "payment id")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		paymentID, err := uuid.Parse(*id)
		if err != nil {
			return fmt.Errorf("parse -id: %w", err)
		}
		p, err := svc.Get(ctx, paymentID)
		if err != nil {
			return err
		}
		printPayment(p)
		return nil

	case "expire":
		n, err := svc.ExpirePending(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("expired %d payments\n", n)
		return nil

	default:
		return fmt.Errorf("unknown payment command %q", args[0])
	}
}

func printPayment(p *payments.Payment) {
	fmt.Printf("id         %s\n", p.ID)
	fmt.Printf("merchant   %s\n", p.MerchantRef)
	fmt.Printf("amount     %s %s\n", p.Amount, p.Currency)
	fmt.Printf("status     %s\n", p.Status)
	if p.ChainTx != "" {
		fmt.Printf("chain tx   %s\n", p.ChainTx)
		fmt.Printf("capture    %s\n", p.CaptureTransferID)
	}
	fmt.Printf("expires    %s\n", p.ExpiresAt.Format(time.RFC3339))
}
