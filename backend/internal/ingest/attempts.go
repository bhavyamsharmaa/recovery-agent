package ingest

import (
	"sync"
	"time"
)

// FirstFailureTracker is an optional capability a store may offer alongside
// AttemptStore: reporting when a payment first failed, at the same time as
// counting the attempt.
//
// It is a separate interface rather than a wider AttemptStore because the
// stopping rule and the confidence gate are written against Get/Increment and
// those signatures stay fixed. A store that cannot answer simply does not
// implement this, and the handler asks with a type assertion — the same shape
// as http.Flusher.
//
// The two values come back together so that a persistent store can answer both
// from one statement. Reading the timestamp separately would mean a second
// round trip per webhook for a value the first one already had in hand.
type FirstFailureTracker interface {
	// IncrementAndFirstFailure increments as Increment does and additionally
	// returns when this payment was first seen. The timestamp is the zero Time
	// if it is unknown.
	IncrementAndFirstFailure(paymentID string) (int, time.Time)
}

// AttemptStore abstracts attempt-count persistence. The in-memory
// implementation below is used through Day 4; Day 5 adds a Postgres-backed
// implementation satisfying the same interface, so the swap is a change to one
// construction site in cmd/server/main.go rather than a change to the handler.
type AttemptStore interface {
	// Get returns the current attempt count for a payment ID, without
	// incrementing. Returns 0 if never seen.
	Get(paymentID string) int

	// Increment atomically increments and returns the new count. The first call
	// for a given ID returns 1, not 0.
	Increment(paymentID string) int
}

// InMemoryAttemptStore is the Day 1-4 implementation. Known limitation: counts
// reset on process restart (see docs/README.md). The map must never be touched
// directly outside this file — every caller goes through AttemptStore.
type InMemoryAttemptStore struct {
	// A plain mutex, not sync.Map. Increment is a read-modify-write that has to
	// be atomic as a whole: sync.Map makes each operation atomic but gives no
	// way to hold the read and the write together, so two concurrent increments
	// could both read the same count and one would be lost.
	mu     sync.Mutex
	counts map[string]int

	// firstSeen is this store's stand-in for failed_payments.first_failed_at, so
	// that running without a database still produces a real elapsed time rather
	// than a silent zero. Lost on restart, like the counts beside it.
	firstSeen map[string]time.Time
}

func NewInMemoryAttemptStore() *InMemoryAttemptStore {
	return &InMemoryAttemptStore{
		counts:    make(map[string]int),
		firstSeen: make(map[string]time.Time),
	}
}

// Both stores answer this, so the handler behaves the same either way.
var _ FirstFailureTracker = (*InMemoryAttemptStore)(nil)

func (s *InMemoryAttemptStore) Get(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[id]
}

func (s *InMemoryAttemptStore) Increment(id string) int {
	n, _ := s.IncrementAndFirstFailure(id)
	return n
}

// IncrementAndFirstFailure records the first sighting the first time it is
// called for an id, and never moves it afterwards. Both maps are written under
// the one lock, so the count and the timestamp cannot disagree.
func (s *InMemoryAttemptStore) IncrementAndFirstFailure(id string) (int, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.counts[id]++
	if _, ok := s.firstSeen[id]; !ok {
		s.firstSeen[id] = time.Now().UTC()
	}
	return s.counts[id], s.firstSeen[id]
}
