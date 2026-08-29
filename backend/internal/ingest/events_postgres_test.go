package ingest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/db"
)

// newTestEventStore builds a store against the real database and returns an
// event id unique to this run.
//
// Live only, like the other Postgres tests here. The behaviour under test is
// whether one SQL statement is atomic under concurrency, which is a property of
// PostgreSQL and not of the Go around it — a fake would prove nothing.
func newTestEventStore(t *testing.T) (*PostgresEventStore, string) {
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

	eventID := fmt.Sprintf("evt_%s_%d", t.Name(), time.Now().UnixNano())

	t.Cleanup(func() {
		if _, err := pool.Exec(`DELETE FROM webhook_events WHERE event_id LIKE $1`, eventID+"%"); err != nil {
			t.Errorf("cleanup webhook_events: %v", err)
		}
		pool.Close()
	})

	return NewPostgresEventStore(pool), eventID
}

// TestPostgresEventStoreFirstDeliveryIsNew is the base case: an event id never
// seen before is new, and the row lands in the table.
func TestPostgresEventStoreFirstDeliveryIsNew(t *testing.T) {
	s, eventID := newTestEventStore(t)

	if !s.RecordEvent(eventID, "pay_first") {
		t.Fatal("first delivery reported as a duplicate")
	}

	var paymentID string
	if err := s.db.QueryRow(
		`SELECT payment_id FROM webhook_events WHERE event_id = $1`, eventID,
	).Scan(&paymentID); err != nil {
		t.Fatalf("read back: %v", err)
	}
	// The payment id is recorded alongside so a duplicate can be traced back to
	// what it was a duplicate of.
	if paymentID != "pay_first" {
		t.Errorf("payment_id = %q, want pay_first", paymentID)
	}
}

// TestPostgresEventStoreRedeliveryIsDuplicate is the whole point of the store:
// the same event id, a second time, is not new.
func TestPostgresEventStoreRedeliveryIsDuplicate(t *testing.T) {
	s, eventID := newTestEventStore(t)

	if !s.RecordEvent(eventID, "pay_dup") {
		t.Fatal("first delivery reported as a duplicate")
	}
	for i := 2; i <= 4; i++ {
		if s.RecordEvent(eventID, "pay_dup") {
			t.Errorf("delivery %d reported as new, want duplicate", i)
		}
	}
}

// TestPostgresEventStoreDistinctEventsAreIndependent guards against a check so
// eager it drops genuine traffic: two different deliveries are both new, even
// when they concern the same payment. A payment that fails twice produces two
// events, and the second is a real failure to be counted, not a redelivery.
func TestPostgresEventStoreDistinctEventsAreIndependent(t *testing.T) {
	s, eventID := newTestEventStore(t)

	if !s.RecordEvent(eventID+"_a", "pay_same") {
		t.Error("first event reported as a duplicate")
	}
	if !s.RecordEvent(eventID+"_b", "pay_same") {
		t.Error("second event on the same payment reported as a duplicate")
	}
}

// TestPostgresEventStoreConcurrentSameEventID is the reason RecordEvent is a
// single INSERT ... ON CONFLICT DO NOTHING RETURNING rather than a SELECT
// followed by an INSERT.
//
// Twenty simultaneous deliveries of one event id must produce exactly one "new"
// and nineteen duplicates. With a naive check-then-insert, several callers read
// an empty table before any of them writes, and every one of those is treated
// as new — which in production means several attempt counts incremented for a
// single delivery.
//
// This is the same race already proven twice on this project: the in-memory
// Load-then-Store on Day 3 and the attempt upsert on Day 5. The count of
// goroutines and the released-together start match
// TestPostgresAttemptStoreConcurrentIncrement deliberately, because it is the
// same test of the same property.
func TestPostgresEventStoreConcurrentSameEventID(t *testing.T) {
	const n = 20
	s, eventID := newTestEventStore(t)

	// Headroom alone is not enough. database/sql opens connections lazily, so
	// twenty goroutines released at once against a cold pool spend their first
	// moments queued behind connection setup and end up serialised — at which
	// point even a naive SELECT-then-INSERT passes, because no two callers are
	// ever actually inside the gap together. The pool is warmed to n live
	// connections first so the contention under test is contention in the
	// database rather than contention for a socket.
	s.db.SetMaxOpenConns(n)
	s.db.SetMaxIdleConns(n)
	warmPool(t, s.db, n)

	got := make([]bool, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release together, to maximise contention
			got[i] = s.RecordEvent(eventID, "pay_concurrent")
		}(i)
	}
	close(start)
	wg.Wait()

	newCount := 0
	for _, isNew := range got {
		if isNew {
			newCount++
		}
	}

	if newCount != 1 {
		t.Fatalf("%d of %d concurrent deliveries were treated as new, want exactly 1 — "+
			"exclusivity lost, the check and the insert are not one operation", newCount, n)
	}

	// And exactly one row exists, so the winner is the one that wrote.
	var rows int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM webhook_events WHERE event_id = $1`, eventID,
	).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("webhook_events holds %d rows for one event id, want 1", rows)
	}
}

// TestPostgresEventStoreSurvivesRestart is the regression this whole change
// exists to fix.
//
// A new store on a new connection stands in for a restarted process: it shares
// no memory with the one that recorded the event, exactly as a fresh binary
// would not. Before this change the dedupe state lived in a sync.Map on the
// Handler, so this scenario returned "new" and incremented an attempt count for
// a delivery that had already been counted.
func TestPostgresEventStoreSurvivesRestart(t *testing.T) {
	s, eventID := newTestEventStore(t)

	if !s.RecordEvent(eventID, "pay_restart") {
		t.Fatal("first delivery reported as a duplicate")
	}

	restarted := freshStore(t)
	if restarted.RecordEvent(eventID, "pay_restart") {
		t.Error("redelivery after a restart was treated as new — dedupe did not survive")
	}
}

// warmPool forces the pool to open n connections and hold them, so a
// concurrency test measures contention in the database rather than the cost of
// establishing sockets to it.
//
// Every connection is acquired before any is released: acquiring and releasing
// one at a time would be satisfied by a single connection reused n times, which
// is the situation this exists to avoid.
func warmPool(t *testing.T, pool *sql.DB, n int) {
	t.Helper()

	ctx := context.Background()
	conns := make([]*sql.Conn, 0, n)
	for i := 0; i < n; i++ {
		c, err := pool.Conn(ctx)
		if err != nil {
			t.Fatalf("warm pool: %v", err)
		}
		if err := c.PingContext(ctx); err != nil {
			t.Fatalf("warm pool ping: %v", err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		if err := c.Close(); err != nil {
			t.Errorf("release warmed connection: %v", err)
		}
	}
}

// freshStore opens an independent connection, sharing no state with the caller's
// store.
func freshStore(t *testing.T) *PostgresEventStore {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.Connect(ctx)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	var _ *sql.DB = pool
	return NewPostgresEventStore(pool)
}
