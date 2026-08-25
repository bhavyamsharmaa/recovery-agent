package ingest

import (
	"sort"
	"sync"
	"testing"
)

func TestAttemptStoreCountsPerPaymentID(t *testing.T) {
	s := NewInMemoryAttemptStore()

	if got := s.Get("pay_never_seen"); got != 0 {
		t.Errorf("Get on an unseen id = %d, want 0", got)
	}

	// The first Increment returns 1, not 0: the count is "how many times this
	// payment has been seen", and by the time Increment returns it has been
	// seen once.
	if got := s.Increment("pay_a"); got != 1 {
		t.Errorf("first Increment = %d, want 1", got)
	}
	if got := s.Increment("pay_a"); got != 2 {
		t.Errorf("second Increment = %d, want 2", got)
	}

	// Get must not increment.
	if got := s.Get("pay_a"); got != 2 {
		t.Errorf("Get after two Increments = %d, want 2", got)
	}
	if got := s.Get("pay_a"); got != 2 {
		t.Errorf("Get is not read-only: second Get = %d, want 2", got)
	}

	// Counting is per payment id, not global.
	if got := s.Increment("pay_b"); got != 1 {
		t.Errorf("first Increment on a second id = %d, want 1", got)
	}
	if got := s.Get("pay_a"); got != 2 {
		t.Errorf("pay_a count changed after incrementing pay_b: %d, want 2", got)
	}
}

// TestAttemptStoreConcurrentIncrement is the correctness bar for the store:
// under 20 simultaneous increments of the SAME id, every caller must come away
// with a distinct value and the final count must be 20. A lost update — two
// callers both returning the same number — is exactly the bug a read-then-write
// without a held lock produces, and it would silently under-count attempts and
// let a payment past its retry budget.
func TestAttemptStoreConcurrentIncrement(t *testing.T) {
	const n = 20
	s := NewInMemoryAttemptStore()

	got := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines at once to maximise contention
			got[i] = s.Increment("pay_contended")
		}(i)
	}
	close(start)
	wg.Wait()

	sort.Ints(got)
	for i, v := range got {
		if want := i + 1; v != want {
			t.Fatalf("sorted returned values = %v; value at index %d is %d, want %d (duplicate or lost increment)", got, i, v, want)
		}
	}

	if final := s.Get("pay_contended"); final != n {
		t.Errorf("final count = %d, want %d", final, n)
	}
}
