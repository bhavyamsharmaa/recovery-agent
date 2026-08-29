-- Webhook deduplication, moved out of process memory.
--
-- Through Day 5 this lived in a sync.Map on the Handler. That was correct while
-- nothing else survived a restart, but it stopped being correct the moment
-- attempt counts moved to Postgres: a restart wiped the dedupe memory while the
-- counts persisted, so a redelivery of an event already handled before the
-- restart was processed as new and incremented an attempt count for a delivery
-- that was not new. The asymmetry is the bug — one half of the request path
-- remembering across restarts and the other half forgetting.
--
-- event_id is the PRIMARY KEY, not merely indexed, because the uniqueness is
-- the mechanism and not an optimisation. The check is an
-- INSERT ... ON CONFLICT (event_id) DO NOTHING RETURNING event_id, which is one
-- statement: a row comes back only for the caller that actually inserted, so
-- twenty simultaneous deliveries of one event produce exactly one "new" and
-- nineteen duplicates without any of them reading each other's state. A SELECT
-- followed by an INSERT would let two concurrent deliveries both see nothing
-- and both proceed, which is the same race already found in-memory on Day 3 and
-- in the attempt upsert on Day 5.
--
-- payment_id is recorded alongside so a duplicate can be traced back to what it
-- was a duplicate of. It is deliberately NOT a foreign key to failed_payments:
-- the dedupe check runs before the payment is recorded, so the reference would
-- not yet resolve, and making it one would order these two writes against each
-- other for no benefit.
CREATE TABLE IF NOT EXISTS webhook_events (
    event_id    TEXT PRIMARY KEY,
    payment_id  TEXT NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
