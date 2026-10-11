-- Money is BIGINT minor units plus an ISO 4217 code, like the Go Money type.
CREATE TABLE wallets (
    id            uuid        PRIMARY KEY,
    player_id     uuid        NOT NULL,
    currency      char(3)     NOT NULL CONSTRAINT wallets_currency_iso CHECK (currency ~ '^[A-Z]{3}$'),
    -- §5: the balance can never be negative, even with an application bug.
    balance_minor bigint      NOT NULL CONSTRAINT wallets_balance_non_negative CHECK (balance_minor >= 0),
    version       bigint      NOT NULL CONSTRAINT wallets_version_positive CHECK (version >= 1),
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    CONSTRAINT wallets_timestamps_ordered CHECK (updated_at >= created_at),
    CONSTRAINT wallets_player_currency_uq UNIQUE (player_id, currency),
    -- Target of the ledger's composite FK: entries must use the wallet's currency.
    CONSTRAINT wallets_id_currency_uq UNIQUE (id, currency)
);

-- Version moves by exactly one when, and only when, the balance changes, and
-- identity never changes. Raised as check_violation (23514).
CREATE FUNCTION wallets_guard_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id <> OLD.id OR NEW.player_id <> OLD.player_id OR NEW.currency <> OLD.currency
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'wallet % identity columns are immutable', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.balance_minor <> OLD.balance_minor AND NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'wallet % balance changed without version + 1 (% -> %)', OLD.id, OLD.version, NEW.version
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.balance_minor = OLD.balance_minor AND NEW.version <> OLD.version THEN
        RAISE EXCEPTION 'wallet % version changed without a balance change', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER wallets_guard_update BEFORE UPDATE ON wallets
    FOR EACH ROW EXECUTE FUNCTION wallets_guard_update();

-- A wallet is never removed: no DELETE or TRUNCATE for the application.
GRANT SELECT, INSERT, UPDATE ON wallets TO wallet_app;
