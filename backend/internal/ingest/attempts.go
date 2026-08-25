package ingest

import "sync"

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
}

func NewInMemoryAttemptStore() *InMemoryAttemptStore {
	return &InMemoryAttemptStore{counts: make(map[string]int)}
}

func (s *InMemoryAttemptStore) Get(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[id]
}

func (s *InMemoryAttemptStore) Increment(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts[id]++
	return s.counts[id]
}
