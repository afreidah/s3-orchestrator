-- The notifier deletes an outbox row once its endpoint's max_retries are
-- spent, so every row left is still deliverable. The pending index stops
-- excluding rows past ten attempts, which an endpoint allowed more retries
-- than that left stuck.

DROP INDEX IF EXISTS idx_notification_outbox_pending;
CREATE INDEX IF NOT EXISTS idx_notification_outbox_pending
    ON notification_outbox(next_retry);
