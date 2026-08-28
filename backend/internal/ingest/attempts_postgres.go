package ingest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/classify"
)

// storeTimeout bounds a single attempt-store query. The AttemptStore interface
// takes no context — it was defined on Day 3 against an in-memory map, where
// there was nothing to cancel — so the deadline is applied here rather than
// being passed in. See the note on error handling below for why that interface
// is worth revisiting.
const storeTimeout = 5 * time.Second

// PostgresAttemptStore is the Day 5 implementation of AttemptStore. It stores
// attempt counts in failed_payments, so they survive a restart, which the
// in-memory store does not.
type PostgresAttemptStore struct {
	db *sql.DB
}

func NewPostgresAttemptStore(db *sql.DB) *PostgresAttemptStore {
	return &PostgresAttemptStore{db: db}
}

// Compile-time proof that the swap in cmd/server/main.go is a swap and not a
// refactor: this type satisfies the same interface the handler already takes.
var _ AttemptStore = (*PostgresAttemptStore)(nil)

// And it can answer when the payment first failed, from the same statement.
var _ FirstFailureTracker = (*PostgresAttemptStore)(nil)

// PaymentRecorder is an optional capability, like FirstFailureTracker: a store
// that can keep what a payment was, not only how often it has been seen.
//
// It is separate from AttemptStore for the same reason as before — Get and
// Increment keep their Day 3 signatures — and separate from FirstFailureTracker
// because the two answer different questions and a store could reasonably
// offer one without the other.
type PaymentRecorder interface {
	RecordPayment(ctx context.Context, d PaymentDetails) error
}

var _ PaymentRecorder = (*PostgresAttemptStore)(nil)

// PaymentDetails is everything failed_payments needs that an attempt count
// alone cannot supply. failed_payments declares these NOT NULL, so a row
// cannot be created from a payment id by itself.
type PaymentDetails struct {
	PaymentID     string
	Category      classify.Category
	ErrorCode     string
	ErrorReason   string
	ErrorSource   string
	PaymentMethod string
	AmountPaise   int64
}

// RecordPayment writes what is known about a failed payment without touching
// its attempt count. It is separate from Increment because the AttemptStore
// interface is deliberately unchanged: the stopping rule and confidence gate
// depend on Get/Increment keeping their Day 3 signatures, so the extra columns
// arrive through their own method rather than by widening those.
//
// first_failed_at is set once and never moved; last_seen_at moves every time.
// The error and category columns are refreshed on conflict because a payment
// that fails again may fail differently, and the decision being made is about
// the most recent failure, not the first one.
func (s *PostgresAttemptStore) RecordPayment(ctx context.Context, d PaymentDetails) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO failed_payments (
			payment_id, category, error_code, error_reason, error_source,
			payment_method, amount_paise, first_failed_at, last_seen_at, attempt_count
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now(), now(), 0)
		ON CONFLICT (payment_id) DO UPDATE SET
			category       = EXCLUDED.category,
			error_code     = EXCLUDED.error_code,
			error_reason   = EXCLUDED.error_reason,
			error_source   = EXCLUDED.error_source,
			payment_method = EXCLUDED.payment_method,
			amount_paise   = EXCLUDED.amount_paise,
			last_seen_at   = now()`,
		d.PaymentID, string(d.Category), d.ErrorCode, d.ErrorReason,
		d.ErrorSource, d.PaymentMethod, d.AmountPaise)
	if err != nil {
		return fmt.Errorf("ingest: record payment %s: %w", d.PaymentID, err)
	}
	return nil
}

// Get returns the stored attempt count, or 0 when the payment has no row yet —
// the same answer InMemoryAttemptStore gives for an id it has never seen.
func (s *PostgresAttemptStore) Get(paymentID string) int {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()

	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT attempt_count FROM failed_payments WHERE payment_id = $1`,
		paymentID).Scan(&n)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0
	case err != nil:
		// Get is not on the request path (see the note in docs/README.md), so
		// there is no decision riding on this. Report and answer 0, which is
		// what an unseen payment would answer.
		fmt.Fprintf(os.Stderr, "ingest: get attempt count for %s: %v\n", paymentID, err)
		return 0
	}
	return n
}

// Increment atomically increments and returns the new count, 1 on the first
// call for a payment.
//
// One statement, not a SELECT followed by an UPDATE. ON CONFLICT DO UPDATE
// takes a row lock and computes the new value from the stored one, so two
// concurrent deliveries of the same payment serialise and come away with
// different numbers. A read-then-write pair would let both read the same count
// — the identical race that was found and fixed in the in-memory store on
// Day 3, just relocated into SQL.
//
// This is a plain UPDATE, not an upsert, and that is forced by migration 002.
// PostgreSQL evaluates a CHECK constraint against the proposed row BEFORE
// resolving ON CONFLICT, so an INSERT ... ON CONFLICT DO UPDATE whose proposed
// tuple carries category = '' is rejected even when the row already exists and
// only the UPDATE would ever have run. Verified directly against the database
// rather than inferred.
//
// The consequence is the one the constraint was added for: counting an attempt
// now requires the payment to have been recorded first. A payment that reaches
// here without RecordPayment having succeeded matches no row, which is reported
// as a failure and fails closed below, instead of quietly creating a row that
// says a payment failed without saying what it was.

func (s *PostgresAttemptStore) Increment(paymentID string) int {
	n, _ := s.IncrementAndFirstFailure(paymentID)
	return n
}

// IncrementAndFirstFailure increments and reports when the payment first
// failed, both from the one statement. first_failed_at is only ever written by
// the INSERT branch, so it is the original value on every later call — the
// UPDATE branch deliberately does not touch it.
//
// On error the timestamp is the zero Time, which the handler reads as "elapsed
// time unknown" rather than "zero seconds ago".
func (s *PostgresAttemptStore) IncrementAndFirstFailure(paymentID string) (int, time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()

	var n int
	var firstFailedAt time.Time
	err := s.db.QueryRowContext(ctx, `
		UPDATE failed_payments
		SET attempt_count = attempt_count + 1,
		    last_seen_at  = now()
		WHERE payment_id = $1
		RETURNING attempt_count, first_failed_at`, paymentID).Scan(&n, &firstFailedAt)

	// No row means RecordPayment never succeeded for this payment. Worth its own
	// message: "no rows in result set" on its own would send a reader looking
	// for a query bug rather than for the recording failure that preceded it.
	if errors.Is(err, sql.ErrNoRows) {
		err = fmt.Errorf("no failed_payments row: the payment was never recorded")
	}
	if err != nil {
		// The interface returns no error, and the stopping rule reads this
		// number to decide whether a payment may be retried. Answering 0 would
		// read as "no attempts yet" and would wave a hard_decline — budget 0 —
		// straight past the check that exists to stop it.
		//
		// So this fails closed: an unusable count is reported as an exhausted
		// one, which stops the payment and escalates it. A database outage
		// becomes a queue of escalations rather than a hole in the compliance
		// rules.
		fmt.Fprintf(os.Stderr, "ingest: increment attempt count for %s: %v\n", paymentID, err)
		return math.MaxInt, time.Time{}
	}
	return n, firstFailedAt
}
