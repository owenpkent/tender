# tender

A stablecoin payment acceptance rail: a merchant asks for money, a buyer pays
it onchain, and a double-entry ledger records the movement exactly once no
matter how many times the event is delivered.

**Nothing here touches real money.** There is no mainnet code, no custody, no
private key handling, and no path by which this software can move anybody's
funds. It is a portfolio system built to be read and argued with, not a
product.

## Status: phase 1 of 4

| Phase | What it adds | State |
|---|---|---|
| 1 | Go service, Postgres double-entry ledger, idempotent capture, migrations | **here** |
| 2 | protobuf/gRPC API with a REST gateway, OpenTelemetry traces, runbook | planned |
| 3 | Temporal workflows for settlement, refunds and reconciliation | planned |
| 4 | Next.js checkout on Base Sepolia, keyboard and screen reader complete | planned |

Phase 1 has no chain in it. `tenderctl payment confirm` stands in for the
watcher that phase 3 brings, which keeps the interesting part in view: what
happens when the same confirmation arrives twenty times at once.

## The invariants

Everything else in this repository is in service of four statements that are
either true or the software is broken.

1. **Money is moved, never created.** The entries of a transfer sum to zero,
   and Postgres enforces it with a deferred constraint trigger, so a future
   service, a migration script or a hand-run `UPDATE` cannot get around the
   care that the Go code takes.
2. **The same event costs the merchant once.** Every write carries an
   idempotency key derived from the event rather than from the attempt.
   Replaying it returns the transfer that already exists and moves nothing.
3. **A payment is confirmed exactly when it is captured.** The status change
   and the ledger entry commit in one transaction, and a `CHECK` constraint
   rejects the rows that would say otherwise.
4. **Internal accounts cannot go negative.** Only `world` accounts may, because
   they represent the outside of the system, so a debit against one is money
   arriving rather than money the platform does not have.

## Running it

Needs Go 1.24 or newer and a Postgres 16 or newer.

With Docker:

```bash
make up                 # starts Postgres and prints the DSN
make migrate            # applies the schema
make test               # runs everything, including the Postgres-backed tests
```

With a Postgres already installed, create the role and database once and point
the Makefile at it:

```bash
psql -U postgres -c "CREATE ROLE tender LOGIN PASSWORD 'tender' CREATEDB"
psql -U postgres -c "CREATE DATABASE tender OWNER tender"
make migrate test DSN='postgres://tender:tender@localhost:5432/tender?sslmode=disable'
```

`CREATEDB` is needed because each test binary builds itself a database rather
than sharing one. `go test ./...` runs packages concurrently, and a shared
database would mean one package truncating tables while another is mid
transaction against them.

Then:

```bash
export TENDER_DSN='postgres://tender:tender@localhost:5432/tender?sslmode=disable'
./bin/tenderctl payment create -merchant acme -amount 25.00
./bin/tenderctl payment confirm -id <the id> -tx 0xdeadbeef
./bin/tenderctl payment confirm -id <the id> -tx 0xdeadbeef   # same result, no second credit
./bin/tenderctl account balance -kind merchant_pending -owner acme
```

Without `TENDER_TEST_DSN` set, the tests that need a database skip and say so
rather than passing quietly. CI always sets it, so they always run there.

## Worth reading

- [`migrations/00001_ledger.sql`](migrations/00001_ledger.sql) for the schema
  and the deferred zero-sum triggers.
- [`internal/ledger/ledger.go`](internal/ledger/ledger.go) for `PostTx`: the
  `ON CONFLICT DO UPDATE ... RETURNING xmax = 0` idempotency claim, and why
  balance locks are taken in a fixed order.
- [`internal/ledger/ledger_test.go`](internal/ledger/ledger_test.go) for the
  twenty concurrent duplicate confirmations, the opposite-direction transfers
  that would deadlock without a lock order, and the test that writes half a
  transfer in raw SQL to prove the database says no.
- [`docs/DESIGN.md`](docs/DESIGN.md) for the decisions and their alternatives.

## License

MIT.
