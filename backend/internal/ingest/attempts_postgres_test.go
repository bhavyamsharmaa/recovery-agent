package ingest

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/classify"
	"github.com/bhavyamsharmaa/recovery-agent/internal/db"
)

// newTestStore opens the real database and hands back a store plus a payment id
// unique to this run. Uniqueness matters: these tests write to a shared
// database, and a fixed id would make two runs — or a run against a row a
// previous failure left behind — interfere invisibly.
//
// Skipped unless RECOVERY_LIVE_TESTS=1, matching the calibration test in this
// package and the migration test in internal/db. There is no dockerized
// Postgres available here, and the property under test is atomicity, which a
// fake cannot demonstrate.
func newTestStore(t *testing.T) (*PostgresAttemptStore, string) {
	t.Helper()

	if os.Getenv("RECOVERY_LIVE_TESTS") != "1" {
		t.Skip("set RECOVERY_LIVE_TESTS=1 to run against the real database")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	paymentID := fmt.Sprintf("pay_test_%s_%d", t.Name(), time.Now().UnixNano())

	t.Cleanup(func() {
		if _, err := pool.Exec(`DELETE FROM failed_payments WHERE payment_id = $1`, paymentID); err != nil {
			t.Errorf("cleanup %s: %v", paymentID, err)
		}
		pool.Close()
	})

	return NewPostgresAttemptStore(pool), paymentID
}

func TestPostgresAttemptStoreGetUnseenIsZero(t *testing.T) {
	s, paymentID := newTestStore(t)

	if got := s.Get(paymentID); got != 0 {
		t.Errorf("Get on an unseen payment = %d, want 0", got)
	}
}

func TestPostgresAttemptStoreIncrementCounts(t *testing.T) {
	s, paymentID := newTestStore(t)

	// The first Increment returns 1, not 0 — the same contract the in-memory
	// store holds, since the handler treats the return value as "this attempt's
	// number".
	for want := 1; want <= 3; want++ {
		if got := s.Increment(paymentID); got != want {
			t.Errorf("Increment #%d returned %d, want %d", want, got, want)
		}
	}

	// Get must observe what Increment wrote, and must not itself increment.
	if got := s.Get(paymentID); got != 3 {
		t.Errorf("Get after 3 increments = %d, want 3", got)
	}
	if got := s.Get(paymentID); got != 3 {
		t.Errorf("Get is not read-only: second Get = %d, want 3", got)
	}
}

func TestPostgresAttemptStoreCountsPerPaymentID(t *testing.T) {
	s, paymentID := newTestStore(t)
	other := paymentID + "_other"
	t.Cleanup(func() { s.db.Exec(`DELETE FROM failed_payments WHERE payment_id = $1`, other) })

	s.Increment(paymentID)
	s.Increment(paymentID)

	if got := s.Increment(other); got != 1 {
		t.Errorf("first Increment on a second payment = %d, want 1 — counts must not be global", got)
	}
	if got := s.Get(paymentID); got != 2 {
		t.Errorf("first payment's count changed after incrementing another: %d, want 2", got)
	}
}

// TestPostgresAttemptStoreConcurrentIncrement is the reason this store uses an
// UPSERT with RETURNING rather than a SELECT followed by an UPDATE. Twenty
// simultaneous deliveries of one payment must come away with twenty distinct
// numbers; a lost update would let a payment take more attempts than its budget
// allows.
func TestPostgresAttemptStoreConcurrentIncrement(t *testing.T) {
	const n = 20
	s, paymentID := newTestStore(t)

	// Without headroom the pool serialises the callers itself and the test
	// proves nothing about the SQL.
	s.db.SetMaxOpenConns(n)

	got := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release together, to maximise contention
			got[i] = s.Increment(paymentID)
		}(i)
	}
	close(start)
	wg.Wait()

	sort.Ints(got)
	for i, v := range got {
		if want := i + 1; v != want {
			t.Fatalf("sorted values = %v; index %d is %d, want %d (duplicate, gap, or lost update)", got, i, v, want)
		}
	}

	if final := s.Get(paymentID); final != n {
		t.Errorf("final count = %d, want %d", final, n)
	}
}

// TestPostgresAttemptStoreRecordPaymentFillsDetails covers the half of the row
// an attempt count cannot supply, and the placeholder path: Increment on an
// unseen payment writes empty strings to satisfy NOT NULL, and RecordPayment
// must replace them rather than leave a half-blank row behind.
func TestPostgresAttemptStoreRecordPaymentFillsDetails(t *testing.T) {
	s, paymentID := newTestStore(t)
	ctx := context.Background()

	// Increment first, so the row exists with placeholders — the order the
	// handler will not use, which is exactly why it needs covering.
	if got := s.Increment(paymentID); got != 1 {
		t.Fatalf("Increment = %d, want 1", got)
	}

	details := PaymentDetails{
		PaymentID:     paymentID,
		Category:      classify.CategoryInsufficientFunds,
		ErrorCode:     "BAD_REQUEST_ERROR",
		ErrorReason:   "insufficient_funds",
		ErrorSource:   "customer",
		PaymentMethod: "card",
		AmountPaise:   499900,
	}
	if err := s.RecordPayment(ctx, details); err != nil {
		t.Fatalf("RecordPayment: %v", err)
	}

	var (
		category, code, reason, source, method string
		amount                                 int64
		attempts                               int
		firstFailed, lastSeen                  time.Time
	)
	err := s.db.QueryRow(`
		SELECT category, error_code, error_reason, error_source, payment_method,
		       amount_paise, attempt_count, first_failed_at, last_seen_at
		FROM failed_payments WHERE payment_id = $1`, paymentID).
		Scan(&category, &code, &reason, &source, &method, &amount, &attempts, &firstFailed, &lastSeen)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	if category != string(details.Category) || code != details.ErrorCode ||
		reason != details.ErrorReason || source != details.ErrorSource ||
		method != details.PaymentMethod || amount != details.AmountPaise {
		t.Errorf("row = (%s %s %s %s %s %d), want (%s %s %s %s %s %d) — placeholders were not replaced",
			category, code, reason, source, method, amount,
			details.Category, details.ErrorCode, details.ErrorReason,
			details.ErrorSource, details.PaymentMethod, details.AmountPaise)
	}

	// RecordPayment must not disturb the count. The two concerns are separate
	// methods precisely so that one cannot silently reset the other.
	if attempts != 1 {
		t.Errorf("attempt_count = %d after RecordPayment, want 1 unchanged", attempts)
	}
	if !lastSeen.After(firstFailed) && !lastSeen.Equal(firstFailed) {
		t.Errorf("last_seen_at %v is before first_failed_at %v", lastSeen, firstFailed)
	}

	// A second record moves last_seen_at but must leave first_failed_at where it
	// was: the first failure is a fact about the payment, not about this call.
	time.Sleep(50 * time.Millisecond)
	if err := s.RecordPayment(ctx, details); err != nil {
		t.Fatalf("second RecordPayment: %v", err)
	}
	var firstAgain, lastAgain time.Time
	if err := s.db.QueryRow(
		`SELECT first_failed_at, last_seen_at FROM failed_payments WHERE payment_id = $1`,
		paymentID).Scan(&firstAgain, &lastAgain); err != nil {
		t.Fatal(err)
	}
	if !firstAgain.Equal(firstFailed) {
		t.Errorf("first_failed_at moved from %v to %v", firstFailed, firstAgain)
	}
	if !lastAgain.After(lastSeen) {
		t.Errorf("last_seen_at did not advance: %v then %v", lastSeen, lastAgain)
	}
}
