package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
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

// shutdownTimeout bounds how long in-flight requests are given to finish after
// SIGTERM. Hosts allow roughly 30s before SIGKILL, so this stays well inside
// that: the process should decide when to give up, rather than being killed
// mid-request and leaving the question of what finished unanswerable.
const shutdownTimeout = 10 * time.Second

// defaultPort is used when PORT is unset, which is the local case.
const defaultPort = "8080"

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

	// The JSON API, mounted as a subtree so its CORS headers apply to every
	// /api/ route and to nothing else. The webhook above is called by
	// Razorpay, not by a browser, and has no business advertising an allowed
	// origin.
	// The API is given the webhook handler so the demo control panel can fire
	// real deliveries through the real pipeline in-process, rather than through
	// a shortcut that would demonstrate nothing.
	//
	// The whole subtree goes behind the shared-secret gate — reads included.
	// NewAuth fails when API_ACCESS_KEY is unset, and that is fatal here rather
	// than a warning: a server that came up serving payment data to anyone who
	// found the port would look entirely healthy doing it.
	//
	// The webhook registered above is deliberately outside this. Razorpay
	// cannot send our header, and that endpoint's authenticity problem is
	// signature verification, which this secret does not solve.
	guarded, err := api.NewAuth(api.NewHandler(pool).WithWebhook(handler))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	http.Handle("/api/", guarded)

	server := &http.Server{Addr: ":" + port()}

	// Shutdown is driven from a goroutine so ListenAndServe stays on the main
	// one: Shutdown makes it return ErrServerClosed, which is how the two halves
	// synchronise without a second channel.
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)

		stop := make(chan os.Signal, 1)
		// Buffered, and registered before serving starts. signal.Notify drops
		// signals on an unbuffered channel nobody is receiving on yet, and the
		// signal that arrives while the process is still booting is exactly the
		// one a fast redeploy sends.
		signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
		<-stop

		fmt.Println(`{"event":"shutdown_started"}`)

		// Shutdown stops accepting new connections and waits for in-flight
		// requests to finish. That wait is the point: a webhook delivery
		// mid-processing has already been counted as an attempt and may have
		// spent a model call, and killing it there leaves a payment recorded
		// with no decision — Razorpay would redeliver into a system that had
		// already charged an attempt against it.
		//
		// The timeout bounds how long that patience lasts. A batch run holds
		// its request open for far longer than this and will be cut off; that
		// is the right trade, because the host sends SIGKILL on its own
		// deadline regardless and an unbounded wait would simply hand the
		// decision to the host.
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			// Deadline exceeded means requests were still running when the
			// timeout expired; they are abandoned now. Logged rather than
			// swallowed, because it is the tell that shutdownTimeout is too
			// short for real traffic.
			fmt.Fprintln(os.Stderr, "graceful shutdown incomplete:", err)
			return
		}
		fmt.Println(`{"event":"shutdown_complete"}`)
	}()

	fmt.Printf("{\"event\":\"server_up\",\"addr\":%q}\n", server.Addr)

	// ErrServerClosed is the expected result of Shutdown, not a failure. Any
	// other error means the listener itself died.
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "server stopped:", err)
		os.Exit(1)
	}

	// Wait for Shutdown to finish before returning: ListenAndServe returns as
	// soon as the listener closes, which is before in-flight requests are done.
	// Exiting here would defeat the whole mechanism.
	<-shutdownDone
}

// port returns the port to listen on, from PORT, falling back to 8080.
//
// Most hosts assign a port dynamically and expect the process to read it from
// the environment; a hardcoded one binds somewhere the platform is not routing
// to, and the deploy fails a health check with the process looking fine in its
// own logs.
func port() string {
	if p := os.Getenv("PORT"); p != "" {
		return p
	}
	return defaultPort
}
