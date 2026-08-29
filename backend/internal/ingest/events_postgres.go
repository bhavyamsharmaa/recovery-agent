package ingest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// PostgresEventStore keeps webhook deduplication in the database, so that a
// redelivery arriving after a restart is still recognised as a redelivery.
//
// This is the counterpart to PostgresAttemptStore, and exists because that one
// does. While attempt counts were in memory too, both halves of the request
// path forgot everything together and a restart was at least consistent. Moving
// counts to Postgres on Day 5 broke that symmetry: the count survived, the
// memory of having already counted it did not, so a redelivered webhook was
// processed as new and incremented an attempt for a delivery that never was.
type PostgresEventStore struct {
	db *sql.DB
}

func NewPostgresEventStore(db *sql.DB) *PostgresEventStore {
	return &PostgresEventStore{db: db}
}

// Compile-time proof that swapping this in at cmd/server/main.go is a swap and
// not a refactor.
var _ EventStore = (*PostgresEventStore)(nil)

// RecordEvent inserts the event id and reports whether this caller is the one
// that inserted it.
//
// One statement, not a SELECT followed by an INSERT. ON CONFLICT DO NOTHING
// with RETURNING makes the check and the record the same operation: the
// RETURNING clause produces a row only for the caller whose INSERT actually
// wrote, so twenty simultaneous deliveries of one event yield exactly one true
// and nineteen falses without any of them reading each other's state. A
// SELECT-then-INSERT pair would let two concurrent deliveries both find nothing
// and both proceed — the same race already found in memory on Day 3 and in the
// attempt upsert on Day 5, which is why the test for this fires twenty
// goroutines at one event id.
//
// A database failure returns true — the delivery is processed. This is the
// opposite direction from PostgresAttemptStore, which fails closed, and the
// asymmetry is deliberate: an attempt count that cannot be read must stop the
// payment, because letting it through spends a budget that exists to protect
// the customer. A dedupe check that cannot be made must let the payment
// through, because dropping it discards a real failure permanently and no later
// delivery will bring it back. The cost of being wrong here is at worst one
// double-counted attempt, which the retry budget still bounds; the cost of
// being wrong the other way is a payment nobody ever tries to recover. The same
// reasoning already governs a delivery that arrives with no event id header at
// all: it cannot be deduplicated, so it is processed rather than dropped.
//
// The error is written to stderr rather than swallowed, because a run of these
// means deduplication is not happening and the only visible symptom otherwise
// would be attempt counts climbing faster than deliveries warrant.
func (s *PostgresEventStore) RecordEvent(eventID, paymentID string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()

	var inserted string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO webhook_events (event_id, payment_id)
		VALUES ($1, $2)
		ON CONFLICT (event_id) DO NOTHING
		RETURNING event_id`, eventID, paymentID).Scan(&inserted)

	// No row came back: the conflict fired, nothing was inserted, and this event
	// has been handled before. This is the ordinary duplicate answer, not an
	// error, which is why it is checked before the error branch.
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"{\"event\":\"dedupe_check_failed\",\"event_id\":%q,\"payment_id\":%q,\"error\":%q,\"processed_anyway\":true}\n",
			eventID, paymentID, err.Error())
		return true
	}
	return true
}
