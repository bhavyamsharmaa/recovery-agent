-- One row per batch run: a set of failed payments put through the agent
-- together, with the money at risk and the money simulated as recovered.
--
-- SIMULATION BOUNDARY. Nothing in this table describes a real payment being
-- retried. No gateway is called, no money moves, and the recovered figures come
-- from a seeded random draw against declared per-action probabilities (see
-- internal/simulate). The table exists to answer "what would this agent's
-- routing have recovered, against a naive strategy" — it is a measurement of a
-- policy, not a record of transactions.
--
-- rng_seed is NOT NULL and stored on the row because it is what makes a run
-- reproducible. A recovery figure in rupees that cannot be regenerated is a
-- number nobody can check; with the seed, any run can be replayed exactly.
-- Storing the seed after the fact would allow a run whose seed was never
-- recorded, so it is required at insert.
--
-- The baseline columns hold the same batch scored under a blind fixed-interval
-- retry strategy, so the comparison is against the same payments and the same
-- RNG stream rather than against a remembered number from another run.
--
-- completed_at is nullable: a row is written when a run starts, so a run that
-- crashes leaves evidence that it was attempted rather than vanishing. A NULL
-- here means "started and never finished", which is a different and more useful
-- statement than no row at all.
--
-- The four result columns are nullable for the same reason — they are unknown
-- until the run completes. batch_size is not: how many payments a run was asked
-- to process is known before any of them are processed.
CREATE TABLE IF NOT EXISTS batch_runs (
    id                       BIGSERIAL PRIMARY KEY,
    started_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at             TIMESTAMPTZ,
    batch_size               INT NOT NULL,
    total_at_risk_paise      BIGINT,
    total_recovered_paise    BIGINT,
    recovery_rate            DOUBLE PRECISION,
    baseline_recovered_paise BIGINT,
    baseline_recovery_rate   DOUBLE PRECISION,
    rng_seed                 BIGINT NOT NULL
);
