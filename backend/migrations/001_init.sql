CREATE TABLE IF NOT EXISTS schema_migrations (
    version TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS failed_payments (
    payment_id       TEXT PRIMARY KEY,
    category         TEXT NOT NULL,
    error_code       TEXT NOT NULL,
    error_reason     TEXT NOT NULL,
    error_source     TEXT NOT NULL,
    payment_method   TEXT NOT NULL,
    amount_paise     BIGINT NOT NULL,
    first_failed_at  TIMESTAMPTZ NOT NULL,
    last_seen_at     TIMESTAMPTZ NOT NULL,
    attempt_count    INT NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS decisions (
    id                 BIGSERIAL PRIMARY KEY,
    payment_id         TEXT NOT NULL REFERENCES failed_payments(payment_id),
    attempt_number     INT NOT NULL,
    action             TEXT NOT NULL,
    confidence         DOUBLE PRECISION,
    reasoning          TEXT,
    customer_message   TEXT,
    alternate_method   TEXT,
    source             TEXT NOT NULL,
    escalation_reason  TEXT,
    original_action    TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS outcomes (
    id            BIGSERIAL PRIMARY KEY,
    payment_id    TEXT NOT NULL REFERENCES failed_payments(payment_id),
    decision_id   BIGINT REFERENCES decisions(id),
    outcome       TEXT NOT NULL,
    recorded_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_decisions_payment_id ON decisions(payment_id);
CREATE INDEX IF NOT EXISTS idx_outcomes_payment_id ON outcomes(payment_id);
