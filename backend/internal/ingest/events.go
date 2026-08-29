package ingest

import "sync"

// EventStore abstracts webhook deduplication, the way AttemptStore abstracts
// attempt counting.
//
// It exists for the same reason and was extracted for the same one: the check
// began as a sync.Map field on the Handler, which was fine while nothing in the
// request path outlived the process. Once attempt counts moved to Postgres it
// stopped being fine — a restart wiped the dedupe memory while the counts it
// guards survived, so a redelivery of an already-handled event was processed as
// new and incremented a count for a delivery that was not new. Behind an
// interface, the fix is one construction site in cmd/server/main.go.
//
// Like AttemptStore, it takes no context and returns no error. That is the same
// design debt noted there — the signature was shaped by an in-memory
// implementation where nothing could fail or be cancelled — and it is kept
// deliberately so the handler's dedupe branch is untouched by this change. The
// Postgres implementation applies its own deadline and decides its own failure
// direction; see RecordEvent there.
type EventStore interface {
	// RecordEvent records that this event id has been handled and reports
	// whether it had already been recorded.
	//
	// It returns true when the delivery is new and should be processed, false
	// when it is a redelivery and should be dropped. The check and the record
	// are one operation on purpose: a caller that asks "have I seen this?" and
	// then separately says "I have now" leaves a window in which two concurrent
	// deliveries of one event both answer no.
	RecordEvent(eventID, paymentID string) bool
}

// InMemoryEventStore is the Day 1 implementation, kept for tests and for
// running without a database.
//
// Known limitation, and the reason the Postgres implementation exists: this
// forgets everything on restart. A redelivery of an event first seen before a
// restart is processed as new.
type InMemoryEventStore struct {
	// sync.Map rather than a mutex and a map, because LoadOrStore is exactly the
	// operation needed and is atomic as a whole. This is the opposite of
	// InMemoryAttemptStore's choice, and for the opposite reason: counting is a
	// read-modify-write that sync.Map cannot hold together, while this is a
	// single check-and-set that it can.
	seen sync.Map
}

func NewInMemoryEventStore() *InMemoryEventStore {
	return &InMemoryEventStore{}
}

var _ EventStore = (*InMemoryEventStore)(nil)

// RecordEvent is one atomic step. A Load-then-Store pair would let two
// concurrent redeliveries of the same event both read "not seen" and both be
// treated as new, which is the exact failure this check exists to prevent.
//
// paymentID is unused here — there is nowhere to keep it — but it is on the
// interface because the durable implementation records it, so a duplicate in
// the table can be traced back to what it was a duplicate of.
func (s *InMemoryEventStore) RecordEvent(eventID, _ string) bool {
	_, loaded := s.seen.LoadOrStore(eventID, struct{}{})
	return !loaded
}
