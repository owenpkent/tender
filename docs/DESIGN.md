# Design notes

What was decided, and what it was decided against. Phase 1 only.

## Amounts are integers

`money.Amount` is an int64 count of minor units. USDC has six decimals, so one
dollar is 1,000,000 and an int64 holds a little over nine trillion dollars of
them.

`money.Parse` rejects more precision than the currency has instead of rounding
it. A ledger that quietly drops a fraction of a cent stops balancing, and it
stops balancing slowly, which is the worst way for it to happen.

Rejected: `numeric` in Postgres with a decimal library in Go. It is more
correct for multi-currency and worth revisiting when a second currency arrives,
but it buys nothing today and costs a dependency plus a class of scan bugs.

## The ledger is double-entry, and the database knows it

A transfer is a set of entries that sum to zero. Two deferred constraint
triggers enforce it at commit time: one on `entries`, which catches an
unbalanced set, and one on `transfers`, which catches a transfer written with
no entries at all. The triggers must be deferred, because the first entry of
any transfer is unbalanced on its own.

The application checks the same thing before it writes. That is not redundant.
The application check gives a caller a typed error and a useful message; the
database check is what makes the invariant true for code that has not been
written yet.

Rejected: computing balances by summing `entries` on read. It is the purest
version and it is correct, but the balance of a busy merchant becomes a growing
aggregate, and the non-negative rule then has no single row to attach a `CHECK`
to. `account_balances` is a materialized total updated in the same transaction
as the entries that move it.

## Idempotency

Every transfer carries a key, and the key is derived from the event rather than
from the attempt. A capture is keyed `payment:<id>:capture`, so the tenth
redelivery of one confirmation lands on the same key as the first.

The claim is one statement:

```sql
INSERT INTO transfers (idempotency_key, ...) VALUES (...)
ON CONFLICT (idempotency_key) DO UPDATE SET idempotency_key = transfers.idempotency_key
RETURNING id, fingerprint, xmax = 0
```

Two details carry the weight.

`DO UPDATE` rather than `DO NOTHING`. `DO NOTHING` returns no row, so there
would be nothing to hand back on a replay. Worse, it does not block on a
conflicting row that a concurrent transaction has inserted but not committed:
both racers would see nothing and both would believe they were first.
`DO UPDATE` waits for that transaction to finish and then sees its row.

`xmax = 0` separates insert from conflict. A freshly inserted row has no
deleting transaction recorded against it; a row this statement updated carries
ours. That is what `Post` returns as its `replayed` flag.

The stored `fingerprint` is a hash of what the transfer does: kind, reference,
and the net amount per account, sorted. Reusing a key for the same movement
written in a different order is a replay. Reusing it for different money is
`ErrIdempotencyConflict`, because that is a caller bug and returning the wrong
transfer would hide it.

Payments get a second, independent guard: a unique index on `chain_tx`. The
ledger key stops one payment being credited twice; the index stops one onchain
transaction being credited to two different payments.

## Lock order

Posting a transfer locks each affected balance row with `SELECT ... FOR UPDATE`
in ascending account id order, one statement per account.

Every transaction takes the same locks in the same order, so concurrent
transfers between the same pair of accounts wait for each other instead of
deadlocking. `TestPostConcurrentOppositeDirectionsDoNotDeadlock` drives forty
transfers in both directions at once specifically to fail if this stops being
true.

Paths that touch both a payment and its balances take the payment row first.

Rejected: a single `SELECT ... FOR UPDATE ... ORDER BY account_id`. Postgres
does not guarantee that a query returns and locks rows in the order requested
under all plans, and the guarantee is the entire point.

Rejected: `SERIALIZABLE` isolation. It would be correct, and it moves the
problem to retrying serialization failures under load. Explicit row locks in a
fixed order are the cheaper answer at this shape.

## Composition across a transaction

`Ledger.PostTx` takes a transaction the caller owns; `Ledger.Post` is the
convenience wrapper that opens and commits one.

`payments.Confirm` needs this. A payment marked confirmed without its ledger
entry, or an entry without the payment, is the exact bug the service exists to
not have, so the status update and the capture commit together or not at all.

Because the zero-sum triggers are deferred, the caller's commit is where they
report, which is why `ledger.Classify` is exported: the caller owns the commit,
so the caller has to be able to name the error it produces.

## Errors are typed at the constraint

`Classify` maps a Postgres constraint violation onto the error a caller should
branch on: `balance_non_negative` to `ErrInsufficientFunds`,
`transfer_balanced` to `ErrUnbalanced`, `payments_chain_tx_unique` to
`ErrChainTxUsedByOther`.

The constraint names are a contract between the migration and the Go file.
Renaming one in SQL without changing it here is a quiet downgrade from a typed
error to an opaque one, and nothing will fail until a customer hits it.

## Expiry does not refuse money

An expired payment still confirms. Expiry governs whether the checkout page
will still take a payment. It cannot govern a transaction that is already
settled on a chain, and refusing to record one would not make the funds go
anywhere; it would only mean nobody knows where they are. The refund path,
which arrives with the settlement workflow in phase 3, is how a merchant gives
it back.

## Migrations

Forward only, embedded in the binary, applied by goose. A mistake is corrected
by a new migration, never by editing one that has already run somewhere.

## Tests use a real Postgres

There is no in-memory fake. Almost everything phase 1 claims to get right is
behaviour of Postgres rather than behaviour of Go, and a fake would test the
fake. The tests skip loudly when `TENDER_TEST_DSN` is unset rather than passing
quietly, and CI always sets it.

`go test -race` is not optional: every claim about concurrent confirmations is
a claim about behaviour under contention.
