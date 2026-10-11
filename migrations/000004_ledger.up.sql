-- Lets an entry point only at a transaction of its own wallet.
ALTER TABLE wager_transactions ADD CONSTRAINT wager_id_wallet_uq UNIQUE (id, wallet_id);

-- Append-only ledger (§6.4).
CREATE TABLE wallet_ledger_entries (
    id                   uuid        PRIMARY KEY,
    -- Keyset pagination order; the wallet row lock makes seq order commit order
    -- within a wallet.
    seq                  bigint      GENERATED ALWAYS AS IDENTITY CONSTRAINT ledger_seq_uq UNIQUE,
    wallet_id            uuid        NOT NULL,
    transaction_id       uuid        NOT NULL,
    direction            text        NOT NULL CONSTRAINT ledger_direction_valid CHECK (direction IN ('DEBIT', 'CREDIT')),
    currency             char(3)     NOT NULL,
    amount_minor         bigint      NOT NULL CONSTRAINT ledger_amount_positive CHECK (amount_minor > 0),
    balance_before_minor bigint      NOT NULL CONSTRAINT ledger_before_non_negative CHECK (balance_before_minor >= 0),
    balance_after_minor  bigint      NOT NULL CONSTRAINT ledger_after_non_negative CHECK (balance_after_minor >= 0),
    wallet_version       bigint      NOT NULL CONSTRAINT ledger_version_positive CHECK (wallet_version >= 1),
    created_at           timestamptz NOT NULL,

    -- Same rule as ledger.NewEntry.
    CONSTRAINT ledger_arithmetic CHECK (
        (direction = 'CREDIT' AND balance_after_minor = balance_before_minor + amount_minor)
        OR (direction = 'DEBIT' AND balance_after_minor = balance_before_minor - amount_minor)
    ),
    CONSTRAINT ledger_wallet_tx_uq UNIQUE (wallet_id, transaction_id),
    -- Two writers that read version n cannot both commit n+1: a lost update
    -- becomes a unique violation even without the row lock.
    CONSTRAINT ledger_wallet_version_uq UNIQUE (wallet_id, wallet_version),
    CONSTRAINT ledger_wallet_currency_fk FOREIGN KEY (wallet_id, currency) REFERENCES wallets (id, currency),
    CONSTRAINT ledger_transaction_wallet_fk FOREIGN KEY (transaction_id, wallet_id) REFERENCES wager_transactions (id, wallet_id)
);

CREATE INDEX ledger_wallet_seq_idx ON wallet_ledger_entries (wallet_id, seq);

-- Layer 2: triggers stop everyone but a superuser, including the migration
-- owner. restrict_violation (23001) marks "forbidden by design".
CREATE FUNCTION ledger_forbid_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'wallet_ledger_entries is append-only (% refused)', TG_OP
        USING ERRCODE = 'restrict_violation';
END $$;

CREATE TRIGGER ledger_no_update_delete BEFORE UPDATE OR DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_forbid_mutation();
-- TRUNCATE bypasses row triggers, so it needs its own statement trigger.
CREATE TRIGGER ledger_no_truncate BEFORE TRUNCATE ON wallet_ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_forbid_mutation();

-- Layer 1: the application can only read and append.
GRANT SELECT, INSERT ON wallet_ledger_entries TO wallet_app;
