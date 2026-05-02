-- -------------------------------------------------------------------------------
-- Notification Outbox - Durable Event Delivery Queue
--
-- Author: Alex Freidah
--
-- Stores pending webhook notifications for async delivery with retry. Events
-- are inserted when state changes occur (object CRUD, circuit breaker
-- transitions, capacity warnings) and drained by a background worker that
-- POSTs CloudEvents JSON to configured endpoints.
-- -------------------------------------------------------------------------------

-- +goose Up

CREATE TABLE IF NOT EXISTS notification_outbox (
    id           BIGSERIAL PRIMARY KEY,
    event_type   TEXT NOT NULL,
    payload      JSONB NOT NULL,
    endpoint_url TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    next_retry   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    attempts     INT NOT NULL DEFAULT 0,
    last_error   TEXT
);

CREATE INDEX IF NOT EXISTS idx_notification_outbox_pending
    ON notification_outbox(next_retry) WHERE attempts < 10;

-- +goose Down
DROP TABLE IF EXISTS notification_outbox;
