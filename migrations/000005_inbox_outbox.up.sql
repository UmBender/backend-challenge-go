-- Inbox: written in the same transaction as the message's effects (§6.5).
-- A redelivery hits the primary key; the hash detects a reused id.
CREATE TABLE inbox_messages (
    consumer_name  text        NOT NULL CONSTRAINT inbox_consumer_len CHECK (char_length(consumer_name) BETWEEN 1 AND 100),
    message_id     text        NOT NULL CONSTRAINT inbox_message_len CHECK (char_length(message_id) BETWEEN 1 AND 255),
    payload_hash   bytea       NOT NULL CONSTRAINT inbox_hash_sha256 CHECK (octet_length(payload_hash) = 32),
    transaction_id uuid        REFERENCES wager_transactions (id),
    received_at    timestamptz NOT NULL,
    completed_at   timestamptz,
    CONSTRAINT inbox_messages_pk PRIMARY KEY (consumer_name, message_id),
    CONSTRAINT inbox_completed_after_received CHECK (completed_at IS NULL OR completed_at >= received_at)
);

GRANT SELECT, INSERT, UPDATE ON inbox_messages TO wallet_app;

-- Outbox: events committed with their change, published later by any
-- instance (at-least-once).
CREATE TABLE outbox_events (
    -- Consumers deduplicate on it.
    event_id        uuid        PRIMARY KEY,
    aggregate_type  text        NOT NULL CONSTRAINT outbox_aggregate_type_valid CHECK (aggregate_type IN ('Wallet')),
    -- SQS MessageGroupId: per-wallet ordering.
    aggregate_id    uuid        NOT NULL,
    event_type      text        NOT NULL,
    event_version   int         NOT NULL CONSTRAINT outbox_version_positive CHECK (event_version >= 1),
    -- The complete envelope, published as stored.
    payload         jsonb       NOT NULL,
    occurred_at     timestamptz NOT NULL,
    -- The only mutable part.
    attempts        int         NOT NULL DEFAULT 0 CONSTRAINT outbox_attempts_non_negative CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL,
    locked_by       text,
    locked_until    timestamptz,
    published_at    timestamptz,
    last_error      text
);

CREATE INDEX outbox_due_idx ON outbox_events (next_attempt_at) WHERE published_at IS NULL;
CREATE INDEX outbox_unpublished_occurred_idx ON outbox_events (occurred_at) WHERE published_at IS NULL;

-- The envelope never changes, and a published row is final: a late
-- publisher cannot unpublish or re-lock it.
CREATE FUNCTION outbox_guard_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.event_id <> OLD.event_id OR NEW.aggregate_type <> OLD.aggregate_type
       OR NEW.aggregate_id <> OLD.aggregate_id OR NEW.event_type <> OLD.event_type
       OR NEW.event_version <> OLD.event_version OR NEW.payload <> OLD.payload
       OR NEW.occurred_at <> OLD.occurred_at THEN
        RAISE EXCEPTION 'outbox event % envelope is immutable', OLD.event_id
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.published_at IS NOT NULL THEN
        RAISE EXCEPTION 'outbox event % is already published', OLD.event_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER outbox_guard_update BEFORE UPDATE ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION outbox_guard_update();

GRANT SELECT, INSERT, UPDATE ON outbox_events TO wallet_app;
