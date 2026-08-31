// Package trace reads a payment's story out of the database: what failed,
// every decision made about it, and any recorded outcome.
//
// The queries here were written for and proven by cmd/trace-payment, which
// printed them as text. They live in a package of their own now because the
// JSON API needs exactly the same rows in a different shape, and a second copy
// of a query is a second place for the schema to drift away from the code.
// cmd/trace-payment is now a formatter over this package.
package trace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound reports a payment id with no row in failed_payments. It is an
// ordinary answer — "never ingested" — not a failure, so callers distinguish it
// from a database error rather than treating every empty read the same.
var ErrNotFound = errors.New("trace: payment not found")

// Payment is one failed_payments row.
type Payment struct {
	PaymentID     string
	Category      string
	ErrorCode     string
	ErrorReason   string
	ErrorSource   string
	PaymentMethod string
	AmountPaise   int64
	AttemptCount  int
	FirstFailedAt time.Time
	LastSeenAt    time.Time
}

// Decision is one decisions row.
//
// The nullable columns stay nullable all the way up. A NULL confidence is not
// zero confidence — it says the decision had no model behind it — and a caller
// that renders it cannot make that distinction from a float64.
type Decision struct {
	ID               int64
	AttemptNumber    int
	Source           string
	Action           string
	Confidence       sql.NullFloat64
	Reasoning        sql.NullString
	CustomerMessage  sql.NullString
	AlternateMethod  sql.NullString
	EscalationReason sql.NullString
	OriginalAction   sql.NullString
	CreatedAt        time.Time
}

// Outcome is one outcomes row. Written only by batch runs, never by live
// webhook traffic, so a payment that arrived through a real webhook has none —
// correctly, because nothing has confirmed what happened to it.
type Outcome struct {
	Outcome    string
	DecisionID sql.NullInt64
	RecordedAt time.Time
}

// Full is everything known about one payment.
type Full struct {
	Payment   Payment
	Decisions []Decision
	Outcomes  []Outcome
}

// Summary is one row of the payment list: the payment plus the most recent
// decision made about it. The decision fields are nullable because a payment
// can be recorded and counted before any decision is stored against it.
type Summary struct {
	Payment
	LatestAction     sql.NullString
	LatestConfidence sql.NullFloat64
	LatestSource     sql.NullString
}

// ListFilter narrows the payment list. An empty field means "no filter on this
// column", which the SQL expresses by comparing the parameter against an empty
// string rather than by assembling the WHERE clause through concatenation: the
// filter values arrive from a URL query string, and a query built by
// concatenation is one refactor away from being injectable.
type ListFilter struct {
	Category string // matches failed_payments.category exactly
	Action   string // matches the most recent decision's action exactly
}

// LoadPayment reads one failed_payments row, or ErrNotFound.
func LoadPayment(ctx context.Context, db *sql.DB, paymentID string) (Payment, error) {
	p := Payment{PaymentID: paymentID}
	err := db.QueryRowContext(ctx, `
		SELECT category, error_code, error_reason, error_source, payment_method,
		       amount_paise, attempt_count, first_failed_at, last_seen_at
		FROM failed_payments WHERE payment_id = $1`, paymentID).
		Scan(&p.Category, &p.ErrorCode, &p.ErrorReason, &p.ErrorSource,
			&p.PaymentMethod, &p.AmountPaise, &p.AttemptCount,
			&p.FirstFailedAt, &p.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Payment{}, ErrNotFound
	}
	if err != nil {
		return Payment{}, fmt.Errorf("trace: read payment: %w", err)
	}
	return p, nil
}

// LoadDecisions reads every decision for a payment, oldest first.
//
// Ordered by attempt_number and then id: attempt_number is the number a reader
// cares about, but two decisions can share one — the confidence gate records
// its override alongside the model answer it overrode — and id breaks that tie
// in the order they were written.
func LoadDecisions(ctx context.Context, db *sql.DB, paymentID string) ([]Decision, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, attempt_number, source, action, confidence, reasoning,
		       customer_message, alternate_method, escalation_reason,
		       original_action, created_at
		FROM decisions WHERE payment_id = $1 ORDER BY attempt_number, id`, paymentID)
	if err != nil {
		return nil, fmt.Errorf("trace: read decisions: %w", err)
	}
	defer rows.Close()

	var all []Decision
	for rows.Next() {
		var d Decision
		if err := rows.Scan(&d.ID, &d.AttemptNumber, &d.Source, &d.Action,
			&d.Confidence, &d.Reasoning, &d.CustomerMessage, &d.AlternateMethod,
			&d.EscalationReason, &d.OriginalAction, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("trace: scan decision: %w", err)
		}
		all = append(all, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace: iterate decisions: %w", err)
	}
	return all, nil
}

// LoadOutcomes reads every outcome recorded against a payment.
func LoadOutcomes(ctx context.Context, db *sql.DB, paymentID string) ([]Outcome, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT outcome, decision_id, recorded_at
		FROM outcomes WHERE payment_id = $1 ORDER BY id`, paymentID)
	if err != nil {
		return nil, fmt.Errorf("trace: read outcomes: %w", err)
	}
	defer rows.Close()

	var all []Outcome
	for rows.Next() {
		var o Outcome
		if err := rows.Scan(&o.Outcome, &o.DecisionID, &o.RecordedAt); err != nil {
			return nil, fmt.Errorf("trace: scan outcome: %w", err)
		}
		all = append(all, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace: iterate outcomes: %w", err)
	}
	return all, nil
}

// Load reads the payment and everything recorded against it.
//
// The payment is read first and its absence returned immediately: without it
// there is nothing to attach decisions to, and the two child queries would
// return empty sets that read as "no decisions recorded" rather than "no such
// payment".
func Load(ctx context.Context, db *sql.DB, paymentID string) (Full, error) {
	p, err := LoadPayment(ctx, db, paymentID)
	if err != nil {
		return Full{}, err
	}
	decisions, err := LoadDecisions(ctx, db, paymentID)
	if err != nil {
		return Full{}, err
	}
	outcomes, err := LoadOutcomes(ctx, db, paymentID)
	if err != nil {
		return Full{}, err
	}
	return Full{Payment: p, Decisions: decisions, Outcomes: outcomes}, nil
}

// List reads payments most recently seen first, each with its latest decision.
//
// The latest decision is picked with DISTINCT ON rather than a MAX subquery
// because several columns have to come from the same winning row: action,
// confidence and source must agree with each other, and three independent
// aggregates would not guarantee that.
//
// The join is a LEFT JOIN so a payment recorded but not yet decided on still
// appears, with null decision fields. Filtering on action then excludes those
// rows by itself: NULL = 'escalate' is not true.
//
// last_seen_at alone is not a unique ordering — two deliveries can land in the
// same instant — so payment_id breaks the tie and the page order stays stable
// between identical requests.
func List(ctx context.Context, db *sql.DB, f ListFilter) ([]Summary, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT p.payment_id, p.category, p.error_code, p.error_reason,
		       p.error_source, p.payment_method, p.amount_paise, p.attempt_count,
		       p.first_failed_at, p.last_seen_at,
		       d.action, d.confidence, d.source
		FROM failed_payments p
		LEFT JOIN (
			SELECT DISTINCT ON (payment_id) payment_id, action, confidence, source
			FROM decisions
			ORDER BY payment_id, attempt_number DESC, id DESC
		) d ON d.payment_id = p.payment_id
		WHERE ($1::text = '' OR p.category = $1::text)
		  AND ($2::text = '' OR d.action = $2::text)
		ORDER BY p.last_seen_at DESC, p.payment_id DESC`, f.Category, f.Action)
	if err != nil {
		return nil, fmt.Errorf("trace: list payments: %w", err)
	}
	defer rows.Close()

	var all []Summary
	for rows.Next() {
		var s Summary
		if err := rows.Scan(&s.PaymentID, &s.Category, &s.ErrorCode, &s.ErrorReason,
			&s.ErrorSource, &s.PaymentMethod, &s.AmountPaise, &s.AttemptCount,
			&s.FirstFailedAt, &s.LastSeenAt,
			&s.LatestAction, &s.LatestConfidence, &s.LatestSource); err != nil {
			return nil, fmt.Errorf("trace: scan payment: %w", err)
		}
		all = append(all, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace: iterate payments: %w", err)
	}
	return all, nil
}

// Escalation is one payment whose latest decision needs a human.
//
// It carries the reasoning and the customer message inline rather than making
// the caller fetch them separately: the queue exists to answer "why does a
// person need to look at this", and an answer that requires another round trip
// is one a reviewer scanning a list will not read.
type Escalation struct {
	Payment

	DecisionID       int64
	AttemptNumber    int
	Action           string
	Source           string
	Confidence       sql.NullFloat64
	EscalationReason sql.NullString
	OriginalAction   sql.NullString
	Reasoning        sql.NullString
	CustomerMessage  sql.NullString
	DecidedAt        time.Time
}

// escalatedActions are the actions that end automated handling. A payment whose
// latest decision is one of these is waiting on a person.
//
// no_retry is included alongside escalate because both stop the machine: the
// difference is that escalate names a policy reason for stopping and no_retry
// is what the fallback answers when the system could not reason at all. A queue
// that showed only escalate would hide exactly the cases where the agent knew
// least.
var escalatedActions = []string{"escalate", "no_retry"}

// Escalations reads every payment whose most recent decision stops automated
// handling, most recent first.
//
// "Most recent decision" is resolved with DISTINCT ON for the same reason List
// uses it: action, source, confidence and reasoning must all come from the same
// winning row. A MAX(id) subquery filtered afterwards would work too, but it
// invites a later edit that compares one column from the winning row against
// another from a different one.
//
// Ordered by when the decision was made rather than when the payment was last
// seen: this is a work queue, and what a reviewer wants at the top is the case
// that most recently started needing them.
func Escalations(ctx context.Context, db *sql.DB) ([]Escalation, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT p.payment_id, p.category, p.error_code, p.error_reason,
		       p.error_source, p.payment_method, p.amount_paise, p.attempt_count,
		       p.first_failed_at, p.last_seen_at,
		       d.id, d.attempt_number, d.action, d.source, d.confidence,
		       d.escalation_reason, d.original_action, d.reasoning,
		       d.customer_message, d.created_at
		FROM failed_payments p
		JOIN (
			SELECT DISTINCT ON (payment_id)
			       id, payment_id, attempt_number, action, source, confidence,
			       escalation_reason, original_action, reasoning, customer_message,
			       created_at
			FROM decisions
			ORDER BY payment_id, attempt_number DESC, id DESC
		) d ON d.payment_id = p.payment_id
		WHERE d.action = ANY($1)
		ORDER BY d.created_at DESC, d.id DESC`, escalatedActions)
	if err != nil {
		return nil, fmt.Errorf("trace: list escalations: %w", err)
	}
	defer rows.Close()

	var all []Escalation
	for rows.Next() {
		var e Escalation
		if err := rows.Scan(&e.PaymentID, &e.Category, &e.ErrorCode, &e.ErrorReason,
			&e.ErrorSource, &e.PaymentMethod, &e.AmountPaise, &e.AttemptCount,
			&e.FirstFailedAt, &e.LastSeenAt,
			&e.DecisionID, &e.AttemptNumber, &e.Action, &e.Source, &e.Confidence,
			&e.EscalationReason, &e.OriginalAction, &e.Reasoning,
			&e.CustomerMessage, &e.DecidedAt); err != nil {
			return nil, fmt.Errorf("trace: scan escalation: %w", err)
		}
		all = append(all, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace: iterate escalations: %w", err)
	}
	return all, nil
}
