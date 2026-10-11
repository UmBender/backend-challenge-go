-- One row per operation. The row is also the durable idempotency record and,
-- while pending, the work item the worker resumes (the database is the queue).
CREATE TABLE wager_transactions (
    id                                uuid        PRIMARY KEY,
    origin                            text        NOT NULL CONSTRAINT wager_origin_valid CHECK (origin IN ('INTERNAL', 'EXTERNAL')),
    kind                              text        NOT NULL CONSTRAINT wager_kind_valid
        CHECK (kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
    status                            text        NOT NULL CONSTRAINT wager_status_valid
        CHECK (status IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')),
    -- WALLET_NOT_FOUND is answered 404 without a row.
    wallet_id                         uuid        NOT NULL REFERENCES wallets (id),
    -- The request's values, so mismatch rejections persist what was sent.
    player_id                         uuid        NOT NULL,
    currency                          char(3)     NOT NULL CONSTRAINT wager_currency_iso CHECK (currency ~ '^[A-Z]{3}$'),
    amount_minor                      bigint      NOT NULL CONSTRAINT wager_amount_non_negative CHECK (amount_minor >= 0),

    -- NULL for OPENING (wager_origin_shape).
    provider_id                       text CONSTRAINT wager_provider_len CHECK (char_length(provider_id) BETWEEN 1 AND 255),
    external_transaction_id           text CONSTRAINT wager_external_id_len CHECK (char_length(external_transaction_id) BETWEEN 1 AND 255),
    idempotency_key                   text CONSTRAINT wager_key_len CHECK (char_length(idempotency_key) BETWEEN 1 AND 255),
    payload_hash                      bytea CONSTRAINT wager_hash_sha256 CHECK (octet_length(payload_hash) = 32),
    round_id                          text CONSTRAINT wager_round_len CHECK (char_length(round_id) BETWEEN 1 AND 255),
    game_id                           text CONSTRAINT wager_game_len CHECK (char_length(game_id) BETWEEN 1 AND 255),
    reference_external_transaction_id text CONSTRAINT wager_reference_len CHECK (char_length(reference_external_transaction_id) BETWEEN 1 AND 255),

    reference_transaction_id          uuid REFERENCES wager_transactions (id),
    failure_code                      text,
    -- Replays return these, not the current balance (§9).
    result_balance_minor              bigint CONSTRAINT wager_result_non_negative CHECK (result_balance_minor >= 0),
    result_wallet_version             bigint CONSTRAINT wager_result_version_positive CHECK (result_wallet_version >= 1),

    attempts                          int         NOT NULL DEFAULT 0 CONSTRAINT wager_attempts_non_negative CHECK (attempts >= 0),
    next_attempt_at                   timestamptz,
    expires_at                        timestamptz,

    created_at                        timestamptz NOT NULL,
    updated_at                        timestamptz NOT NULL,
    processed_at                      timestamptz,

    CONSTRAINT wager_origin_shape CHECK (
        (origin = 'INTERNAL' AND kind = 'OPENING'
            AND provider_id IS NULL AND external_transaction_id IS NULL
            AND idempotency_key IS NULL AND payload_hash IS NULL
            AND round_id IS NULL AND game_id IS NULL
            AND reference_external_transaction_id IS NULL)
        OR (origin = 'EXTERNAL' AND kind <> 'OPENING'
            AND provider_id IS NOT NULL AND external_transaction_id IS NOT NULL
            AND idempotency_key IS NOT NULL AND payload_hash IS NOT NULL
            AND round_id IS NOT NULL AND game_id IS NOT NULL)
    ),
    -- §7: zero only for LOSS; OPENING only for a positive initial balance.
    CONSTRAINT wager_amount_policy CHECK (
        (kind = 'LOSS' AND amount_minor = 0) OR (kind <> 'LOSS' AND amount_minor > 0)
    ),
    CONSTRAINT wager_reference_policy CHECK (
        (kind IN ('REFUND', 'ROLLBACK') AND reference_external_transaction_id IS NOT NULL)
        OR (kind IN ('BET', 'LOSS', 'OPENING') AND reference_external_transaction_id IS NULL)
        OR kind = 'WIN'
    ),
    CONSTRAINT wager_failure_code_iff_failed CHECK (
        (status IN ('REJECTED', 'FAILED')) = (failure_code IS NOT NULL)
    ),
    CONSTRAINT wager_processed_has_result CHECK (
        status <> 'PROCESSED'
        OR (result_balance_minor IS NOT NULL AND result_wallet_version IS NOT NULL AND processed_at IS NOT NULL)
    ),
    CONSTRAINT wager_pending_reference_scheduled CHECK (
        status <> 'PENDING_REFERENCE' OR (next_attempt_at IS NOT NULL AND expires_at IS NOT NULL)
    ),
    CONSTRAINT wager_timestamps_ordered CHECK (updated_at >= created_at)
);

-- Durable idempotency per provider: these indexes, not in-process locks or
-- SQS deduplication, guarantee a single financial effect.
CREATE UNIQUE INDEX wager_provider_external_uq ON wager_transactions (provider_id, external_transaction_id)
    WHERE origin = 'EXTERNAL';
CREATE UNIQUE INDEX wager_provider_key_uq ON wager_transactions (provider_id, idempotency_key)
    WHERE origin = 'EXTERNAL';
-- At most one opening credit per wallet (§6.3).
CREATE UNIQUE INDEX wager_single_opening_uq ON wager_transactions (wallet_id)
    WHERE kind = 'OPENING';
-- At most one successful reversal per transaction, even under a race.
CREATE UNIQUE INDEX wager_single_reversal_uq ON wager_transactions (reference_transaction_id)
    WHERE status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK');
-- Pending worker: due work, oldest first.
CREATE INDEX wager_due_idx ON wager_transactions (next_attempt_at)
    WHERE status IN ('PENDING', 'PENDING_REFERENCE');
-- Wake-up of reversals waiting for a given external id.
CREATE INDEX wager_pending_reference_idx ON wager_transactions (provider_id, reference_external_transaction_id)
    WHERE status = 'PENDING_REFERENCE';
-- Resolving a reversal's own reference by the referenced id.
CREATE INDEX wager_reference_tx_idx ON wager_transactions (reference_transaction_id)
    WHERE reference_transaction_id IS NOT NULL;
-- Every wallet-scoped lookup.
CREATE INDEX wager_wallet_idx ON wager_transactions (wallet_id);

-- State machine and identity enforced again: terminal rows never change,
-- nothing returns to PENDING, and the operation's fields are immutable.
CREATE FUNCTION wager_guard_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status IN ('PROCESSED', 'REJECTED', 'FAILED') THEN
        RAISE EXCEPTION 'wager transaction % is terminal (%)', OLD.id, OLD.status
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.status = 'PENDING' THEN
        RAISE EXCEPTION 'wager transaction % cannot move to PENDING', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.id <> OLD.id OR NEW.origin <> OLD.origin OR NEW.kind <> OLD.kind
       OR NEW.wallet_id <> OLD.wallet_id OR NEW.player_id <> OLD.player_id
       OR NEW.currency <> OLD.currency OR NEW.amount_minor <> OLD.amount_minor
       OR NEW.provider_id IS DISTINCT FROM OLD.provider_id
       OR NEW.external_transaction_id IS DISTINCT FROM OLD.external_transaction_id
       OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
       OR NEW.payload_hash IS DISTINCT FROM OLD.payload_hash
       OR NEW.round_id IS DISTINCT FROM OLD.round_id
       OR NEW.game_id IS DISTINCT FROM OLD.game_id
       OR NEW.reference_external_transaction_id IS DISTINCT FROM OLD.reference_external_transaction_id
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'wager transaction % identity fields are immutable', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER wager_guard_update BEFORE UPDATE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION wager_guard_update();

GRANT SELECT, INSERT, UPDATE ON wager_transactions TO wallet_app;
