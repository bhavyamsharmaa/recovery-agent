package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/db"
	"github.com/bhavyamsharmaa/recovery-agent/internal/decide"
	"github.com/bhavyamsharmaa/recovery-agent/internal/ingest"
)

// startupTimeout bounds connecting and migrating together. Without it a
// database that accepts the TCP connection but never answers would leave the
// process hanging before it ever listens, with no output explaining why.
const startupTimeout = 30 * time.Second

func main() {
	client, err := decide.NewClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), startupTimeout)
	pool, err := db.Connect(ctx)
	if err != nil {
		cancel()
		fmt.Fprintln(os.Stderr, "database unavailable:", err)
		os.Exit(1)
	}
	defer pool.Close()

	// Migrations run before the listener opens, not alongside it. Serving
	// against a half-migrated schema would fail per request, at which point
	// Razorpay is already retrying webhooks into a broken receiver.
	ran, err := db.Migrate(ctx, pool)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrations failed, not starting:", err)
		os.Exit(1)
	}
	if len(ran) == 0 {
		fmt.Println(`{"event":"migrations_up_to_date"}`)
	} else {
		for _, v := range ran {
			fmt.Printf("{\"event\":\"migration_applied\",\"version\":%q}\n", v)
		}
	}

	// Declared as the interface, not the concrete type, so nothing below this
	// line can reach for an implementation detail. This is the Day 5 swap the
	// interface was built for: one constructor changed, nothing else.
	//
	// InMemoryAttemptStore stays in the package for tests and for running
	// locally without a database. It answers the same interface, including the
	// first-failure timestamp, so the handler behaves identically either way —
	// only durability differs.
	var attempts ingest.AttemptStore = ingest.NewPostgresAttemptStore(pool)

	// Decisions from all three sources are stored here, alongside the JSON log
	// lines, so the audit trail survives the process.
	decisions := ingest.NewPostgresDecisionStore(pool)

	handler := ingest.NewHandler(client, attempts).WithDecisionRecorder(decisions)
	http.Handle("/webhook/payment-failed", handler)

	fmt.Println("server up")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		fmt.Fprintln(os.Stderr, "server stopped:", err)
		os.Exit(1)
	}
}
