package ingest

import (
	"context"
	"database/sql"
	"fmt"
)

// DecisionRecorder persists a decision. It is an interface so the handler can
// be constructed without a database — every test in this package that predates
// persistence still builds a Handler with no recorder at all, and a nil
// recorder means the decision is logged and not stored.
type DecisionRecorder interface {
	RecordDecision(ctx context.Context, d DecisionRecord) error
}

// DecisionRecord is one row of the decisions table.
//
// The nullable columns are pointers rather than zero values because the
// difference matters to anyone querying this table directly. A confidence of 0
// is a real score the model can return; "this decision had no model behind it"
// is not the same statement, and storing 0 for it would make the two
// indistinguishable in SQL.
type DecisionRecord struct {
	PaymentID     string
	AttemptNumber int
	Action        string
	Source        string

	// Confidence is nil for stopping-rule and fallback decisions. Day 4's JSON
	// log reports the fallback as confidence 0 by design; the column does not,
	// because a reader of the log has the source field beside it and a reader of
	// the column may not.
	Confidence *float64

	Reasoning        string
	CustomerMessage  string
	AlternateMethod  string
	EscalationReason string
	OriginalAction   string
}

// PostgresDecisionStore writes decisions to Postgres.
type PostgresDecisionStore struct {
	db *sql.DB
}

func NewPostgresDecisionStore(db *sql.DB) *PostgresDecisionStore {
	return &PostgresDecisionStore{db: db}
}

var _ DecisionRecorder = (*PostgresDecisionStore)(nil)

// RecordDecision inserts one decision.
//
// decisions.payment_id references failed_payments, so the payment must already
// have been counted. In the handler it always has: the attempt is recorded
// before any decision path is reached.
func (s *PostgresDecisionStore) RecordDecision(ctx context.Context, d DecisionRecord) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO decisions (
			payment_id, attempt_number, action, confidence, reasoning,
			customer_message, alternate_method, source, escalation_reason,
			original_action
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		d.PaymentID,
		d.AttemptNumber,
		d.Action,
		d.Confidence,
		nullIfEmpty(d.Reasoning),
		nullIfEmpty(d.CustomerMessage),
		nullIfEmpty(d.AlternateMethod),
		d.Source,
		nullIfEmpty(d.EscalationReason),
		nullIfEmpty(d.OriginalAction),
	)
	if err != nil {
		return fmt.Errorf("ingest: record decision for %s: %w", d.PaymentID, err)
	}
	return nil
}

// nullIfEmpty stores NULL rather than an empty string for a column that does
// not apply. An empty alternate_method on an escalate is not a value the model
// chose; it is the absence of one, and the column should say so.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
