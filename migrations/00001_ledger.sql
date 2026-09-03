-- +goose Up

-- Every amount in this schema is an exact integer in the currency's minor
-- units. USDC has 6 decimals, so 1_000_000 is one dollar. There is no float
-- anywhere in the money path and there never will be.

CREATE TYPE account_kind AS ENUM (
    'world',              -- the outside of the system: chains, banks, buyers
    'merchant_pending',   -- confirmed onchain, not yet settled to the merchant
    'merchant_available', -- settled and withdrawable
    'fees',               -- platform revenue
    'refunds_payable'
);

CREATE TABLE accounts (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind       account_kind NOT NULL,
    owner_ref  text NOT NULL,
    currency   text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT accounts_identity_unique UNIQUE (kind, owner_ref, currency)
);

-- Balances live in their own table so that posting a transfer locks exactly
-- the rows whose money is moving, and never the account metadata that a
-- reporting query might be reading at the same time.
--
-- allow_negative is denormalized onto this row on purpose: the CHECK below is
-- the last line of defence against overdrawing an internal account, and a
-- CHECK constraint cannot reach into another table to find out whether it is
-- allowed to fire.
CREATE TABLE account_balances (
    account_id     uuid PRIMARY KEY REFERENCES accounts (id) ON DELETE RESTRICT,
    balance        bigint NOT NULL DEFAULT 0,
    allow_negative boolean NOT NULL DEFAULT false,
    version        bigint NOT NULL DEFAULT 0,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT balance_non_negative CHECK (allow_negative OR balance >= 0)
);

-- +goose StatementBegin
CREATE FUNCTION create_balance_for_account() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO account_balances (account_id, allow_negative)
    VALUES (NEW.id, NEW.kind = 'world');
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- An account without a balance row would be an account the ledger cannot post
-- to, so the two are created together rather than hopefully.
CREATE TRIGGER accounts_create_balance
    AFTER INSERT ON accounts
    FOR EACH ROW EXECUTE FUNCTION create_balance_for_account();

CREATE TABLE transfers (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key text NOT NULL,
    kind            text NOT NULL,
    reference       text NOT NULL DEFAULT '',
    fingerprint     text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT transfers_idempotency_key_unique UNIQUE (idempotency_key)
);

CREATE TABLE entries (
    id          bigserial PRIMARY KEY,
    transfer_id uuid NOT NULL REFERENCES transfers (id) ON DELETE RESTRICT,
    account_id  uuid NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    amount      bigint NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT entries_amount_nonzero CHECK (amount <> 0)
);

CREATE INDEX entries_account_idx ON entries (account_id, id DESC);
CREATE INDEX entries_transfer_idx ON entries (transfer_id);

-- The invariant the whole system rests on: the entries of a transfer sum to
-- zero and there are at least two of them. Money is only ever moved, never
-- created. Application code checks this too, but application code is the part
-- most likely to be wrong, so the database gets the final say.
-- +goose StatementBegin
CREATE FUNCTION assert_transfer_balanced(t_id uuid) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    total bigint;
    n     integer;
BEGIN
    SELECT coalesce(sum(amount), 0), count(*) INTO total, n
    FROM entries WHERE transfer_id = t_id;

    IF n < 2 THEN
        RAISE EXCEPTION 'transfer % has % entries, a transfer needs at least 2', t_id, n
            USING ERRCODE = 'check_violation', CONSTRAINT = 'transfer_balanced';
    END IF;

    IF total <> 0 THEN
        RAISE EXCEPTION 'transfer % entries sum to %, must sum to 0', t_id, total
            USING ERRCODE = 'check_violation', CONSTRAINT = 'transfer_balanced';
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION check_entry_transfer_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM assert_transfer_balanced(NEW.transfer_id);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION check_transfer_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM assert_transfer_balanced(NEW.id);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- Both triggers are DEFERRABLE INITIALLY DEFERRED, so they run once at COMMIT
-- rather than after each INSERT. Without that, the first entry of any transfer
-- would fail the check on its own.
--
-- The trigger on transfers is not redundant: it is the one that catches a
-- transfer inserted with no entries at all, which the entries trigger can
-- never see.
CREATE CONSTRAINT TRIGGER entries_balanced
    AFTER INSERT ON entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_entry_transfer_balanced();

CREATE CONSTRAINT TRIGGER transfers_balanced
    AFTER INSERT ON transfers
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_transfer_balanced();

-- +goose Down
DROP TRIGGER transfers_balanced ON transfers;
DROP TRIGGER entries_balanced ON entries;
DROP FUNCTION check_transfer_balanced();
DROP FUNCTION check_entry_transfer_balanced();
DROP FUNCTION assert_transfer_balanced(uuid);
DROP TABLE entries;
DROP TABLE transfers;
DROP TRIGGER accounts_create_balance ON accounts;
DROP FUNCTION create_balance_for_account();
DROP TABLE account_balances;
DROP TABLE accounts;
DROP TYPE account_kind;
