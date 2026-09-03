-- +goose Up

CREATE TYPE payment_status AS ENUM ('pending', 'confirmed', 'expired');

CREATE TABLE payments (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_ref        text NOT NULL,
    amount              bigint NOT NULL CHECK (amount > 0),
    currency            text NOT NULL,
    status              payment_status NOT NULL DEFAULT 'pending',
    chain_tx            text,
    capture_transfer_id uuid REFERENCES transfers (id),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    expires_at          timestamptz NOT NULL,
    -- A payment is confirmed exactly when it has both a chain reference and
    -- the ledger transfer that captured it. Encoding it as a constraint means
    -- no code path can leave a payment confirmed but unposted.
    CONSTRAINT payments_confirmed_has_capture CHECK (
        (status = 'confirmed') = (chain_tx IS NOT NULL AND capture_transfer_id IS NOT NULL)
    )
);

-- One payment per onchain transaction. This is the second half of the
-- idempotency story: the ledger key stops a replayed API call, and this stops
-- the same transaction being credited to two different payments.
CREATE UNIQUE INDEX payments_chain_tx_unique ON payments (chain_tx) WHERE chain_tx IS NOT NULL;

CREATE INDEX payments_merchant_idx ON payments (merchant_ref, created_at DESC);
CREATE INDEX payments_pending_expiry_idx ON payments (expires_at) WHERE status = 'pending';

-- +goose Down
DROP TABLE payments;
DROP TYPE payment_status;
