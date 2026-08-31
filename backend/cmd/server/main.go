package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/api"
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

	// Webhook deduplication, in the database rather than in a map on the
	// handler. This is the other half of the Day 5 swap, and it was a real bug
	// to have left behind: with counts persisted and dedupe in memory, a restart
	// kept the attempt count for an event while forgetting that the event had
	// been handled, so a redelivery incremented a count for a delivery that was
	// not new.
	events := ingest.NewPostgresEventStore(pool)

	// The decider is wrapped so that a request which explicitly asks for a
	// forced decision failure gets one. Nothing arriving on the webhook endpoint
	// can ask: the marker is a context value that only /api/simulate/llm-failure
	// sets, and it replaces the FORCE_DECIDE_FAILURE environment variable that
	// was removed before the Day 4 merge precisely because a global switch could
	// be left on by accident. For every other request this wrapper is a
	// pass-through to the real client.
	decider := &ingest.ForcedFailureDecider{Real: client}

	handler := ingest.NewHandler(decider, attempts).
		WithDecisionRecorder(decisions).
		WithEventStore(events)
	http.Handle("/webhook/payment-failed", handler)

	// The read-only JSON API, mounted as a subtree so its CORS headers apply to
	// every /api/ route and to nothing else. The webhook above is called by
	// Razorpay, not by a browser, and has no business advertising an allowed
	// origin.
	// The API is given the webhook handler so the demo control panel can fire
	// real deliveries through the real pipeline in-process, rather than through
	// a shortcut that would demonstrate nothing.
	http.Handle("/api/", api.NewHandler(pool).WithWebhook(handler))

	fmt.Println("server up")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		fmt.Fprintln(os.Stderr, "server stopped:", err)
		os.Exit(1)
	}
}
