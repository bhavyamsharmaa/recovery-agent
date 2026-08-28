-- failed_payments.category is TEXT NOT NULL, which a row can satisfy with an
-- empty string. Increment's INSERT branch does exactly that when it creates a
-- row before RecordPayment has supplied the real values, so a failure to record
-- the payment produced a row that was valid but said nothing about what failed.
--
-- Only category is constrained. It is the column every downstream step depends
-- on — the retry budget is looked up by it and the model is told it — so an
-- empty one is never useful. Constraining every descriptive column would risk a
-- future legitimate write failing for an unrelated reason.
--
-- Wrapped in a DO block because Postgres has no ADD CONSTRAINT IF NOT EXISTS,
-- and a migration has to be safe to re-run against a database that already has
-- it: the runner records versions, but the file should not depend on that.
--
-- This ALTER fails if any existing row has an empty category. That is the
-- correct outcome — such rows are the bug this constraint exists to prevent,
-- and they must be cleaned up rather than grandfathered in.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'category_not_empty'
    ) THEN
        ALTER TABLE failed_payments
            ADD CONSTRAINT category_not_empty CHECK (category <> '');
    END IF;
END
$$;
