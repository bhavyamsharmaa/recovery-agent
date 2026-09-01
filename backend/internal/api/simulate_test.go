package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/decide"
	"github.com/bhavyamsharmaa/recovery-agent/internal/ingest"
)

// The demo control panel's endpoints, and the isolation property they must not
// break.
//
// These use a stub decider rather than the live model, because what is under
// test is the plumbing — that a real webhook goes through the real handler, that
// a duplicate is detected from observed state, and above all that the forced
// failure reaches nothing it should not. The model's own behaviour is covered in
// the decide package.

// newSimulateHandler wires an api.Handler to a real ingest.Handler backed by the
// live database, exactly as cmd/server does.
func newSimulateHandler(t *testing.T) (*Handler, *countingDecider) {
	t.Helper()
	pool := liveDB(t)

	decider := &countingDecider{}
	// The Postgres attempt store, not the in-memory one, because only it
	// implements PaymentRecorder — the capability that writes the
	// failed_payments row. With the in-memory store the handler counts an
	// attempt against a row that was never created, and the simulate endpoints
	// then fail to read any decision back. That is exactly how production is
	// wired in cmd/server, and this wiring must match it or the test exercises
	// a shape that does not ship.
	attempts := ingest.NewPostgresAttemptStore(pool)

	// The webhook verifies signatures in production, and fireWebhook signs with
	// RAZORPAY_WEBHOOK_SECRET, so the test wiring has to verify against the same
	// variable — otherwise the simulate endpoints would be exercising a handler
	// that skips a check the real one performs.
	//
	// Only defaulted, never overridden. TestTriggerBatchRunRunsAndStoresARun
	// shares this helper but drives the batch runner, which posts over real HTTP
	// to a separately started server: forcing a test-only secret here would sign
	// with one value while that server verified with another, and every delivery
	// would be rejected. Defaulting keeps the in-process tests runnable with
	// only DATABASE_URL set, while an environment that has the real secret is
	// left alone.
	if os.Getenv("RAZORPAY_WEBHOOK_SECRET") == "" {
		t.Setenv("RAZORPAY_WEBHOOK_SECRET", testWebhookSecret)
	}
	verifier, err := ingest.NewVerifier()
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	webhook := ingest.NewHandler(&ingest.ForcedFailureDecider{Real: decider}, attempts).
		WithDecisionRecorder(ingest.NewPostgresDecisionStore(pool)).
		WithEventStore(ingest.NewPostgresEventStore(pool)).
		WithVerifier(verifier)

	// PUBLIC_BASE_URL is what triggerBatchRun builds its webhook address from
	// (issue #3). TestTriggerBatchRunRunsAndStoresARun shares this helper and
	// drives a real batch, which posts over HTTP to a separately started server
	// on 8080 — so that is the address it needs. Defaulted rather than forced,
	// like the webhook secret above, so an environment that already points
	// somewhere else is left alone.
	if os.Getenv(publicBaseURLEnv) == "" {
		t.Setenv(publicBaseURLEnv, "http://localhost:8080")
	}
	api, err := NewHandlerWithBatchURL(pool)
	if err != nil {
		t.Fatalf("NewHandlerWithBatchURL: %v", err)
	}

	return api.WithWebhook(webhook), decider
}

// testWebhookSecret is what the simulate endpoints sign with and the test
// handler verifies against. Declared here rather than shared with the ingest
// package's constant of the same name, which is unexported: a test secret is
// not something to widen a package's API for.
const testWebhookSecret = "api-test-webhook-secret"

// postJSON posts and records every id in the response, so cleanup can delete
// exactly what the call created.
func postJSON(t *testing.T, h *Handler, target string, created *createdRows) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader("{}"))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v\nbody: %s", target, err, rec.Body.String())
	}
	return rec, body
}

// decisionOf pulls the decision object out of a simulate response.
func decisionOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	d, ok := body["decision"].(map[string]any)
	if !ok {
		t.Fatalf("response has no decision object: %v", body)
	}
	return d
}

func TestSimulateFailureRunsTheRealPipeline(t *testing.T) {
	h, decider := newSimulateHandler(t)
	created := &createdRows{}
	cleanupSimulated(t, h, created)

	for _, category := range []string{"insufficient_funds", "soft_decline", "network_error"} {
		t.Run(category, func(t *testing.T) {
			before := decider.calls
			rec, body := postJSON(t, h, "/api/simulate/failure?category="+category, created)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %v", rec.Code, body)
			}
			d := decisionOf(t, body)
			if d["category"] != category {
				t.Errorf("category = %v, want %s", d["category"], category)
			}
			// The real decision layer was consulted — not a shortcut that
			// invented an action for display.
			if decider.calls != before+1 {
				t.Error("the decider was not reached; the endpoint short-circuited the pipeline")
			}
			if d["payment_id"] == "" || d["action"] == "" {
				t.Errorf("decision is missing payment_id or action: %v", d)
			}
			if body["event_id"] == "" {
				t.Error("no event_id reported")
			}
		})
	}
}

// TestSimulateFailureRejectsUnknownCategory guards the one input this endpoint
// takes. "duplicate" is a delivery behaviour rather than a failure mode and must
// not be reachable here even though it exists in the scenario table.
func TestSimulateFailureRejectsUnknownCategory(t *testing.T) {
	h, _ := newSimulateHandler(t)
	// Nothing is created: every call is rejected before the pipeline runs.

	for _, c := range []string{"", "not_a_category", "duplicate"} {
		rec, body := postJSON(t, h, "/api/simulate/failure?category="+c, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("category %q: status = %d, want 400 (body %v)", c, rec.Code, body)
		}
	}
}

// TestSimulateDuplicateShowsOneProcessedAndOneDuplicate is the idempotency
// demonstration. Both deliveries must answer 200 — that is the contract — so the
// distinction has to come from observed state.
func TestSimulateDuplicateShowsOneProcessedAndOneDuplicate(t *testing.T) {
	h, _ := newSimulateHandler(t)
	created := &createdRows{}
	cleanupSimulated(t, h, created)

	rec, body := postJSON(t, h, "/api/simulate/duplicate", created)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %v", rec.Code, body)
	}

	deliveries, ok := body["deliveries"].([]any)
	if !ok || len(deliveries) != 2 {
		t.Fatalf("want exactly 2 deliveries, got %v", body["deliveries"])
	}

	first := deliveries[0].(map[string]any)
	second := deliveries[1].(map[string]any)

	if first["status"] != "processed" {
		t.Errorf("delivery 1 status = %v, want processed", first["status"])
	}
	if second["status"] != "duplicate" {
		t.Errorf("delivery 2 status = %v, want duplicate", second["status"])
	}
	// The same event id both times — that is what makes it a redelivery rather
	// than a second failure of the same payment.
	if first["event_id"] != second["event_id"] {
		t.Errorf("the two deliveries used different event ids: %v vs %v", first["event_id"], second["event_id"])
	}
	// Both answer 200. Telling Razorpay to retry something already handled is
	// worse than acknowledging it.
	if first["http_status"] != float64(200) || second["http_status"] != float64(200) {
		t.Errorf("http statuses = %v / %v, want 200 / 200", first["http_status"], second["http_status"])
	}
	// And the proof: the second delivery moved nothing.
	if first["attempt_count"] != second["attempt_count"] {
		t.Errorf("attempt_count changed on the duplicate: %v -> %v",
			first["attempt_count"], second["attempt_count"])
	}
	if first["decisions_for_payment"] != second["decisions_for_payment"] {
		t.Errorf("decision count changed on the duplicate: %v -> %v",
			first["decisions_for_payment"], second["decisions_for_payment"])
	}
}

// TestSimulateLLMFailureProducesTheFallback covers the demo path end to end.
func TestSimulateLLMFailureProducesTheFallback(t *testing.T) {
	h, decider := newSimulateHandler(t)
	created := &createdRows{}
	cleanupSimulated(t, h, created)

	before := decider.calls
	rec, body := postJSON(t, h, "/api/simulate/llm-failure", created)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %v", rec.Code, body)
	}

	d := decisionOf(t, body)
	if d["source"] != "fallback_rule" {
		t.Errorf("source = %v, want fallback_rule", d["source"])
	}
	if d["action"] != "no_retry" {
		t.Errorf("action = %v, want no_retry", d["action"])
	}
	// Null, never 0: the fallback had no model behind it.
	if d["confidence"] != nil {
		t.Errorf("confidence = %v, want null", d["confidence"])
	}
	// The customer is still told something.
	if msg, _ := d["customer_message"].(string); msg == "" {
		t.Error("no customer message on the fallback decision")
	}
	// The webhook still answered 200 — a model outage must not become
	// Razorpay's problem.
	if body["webhook_http_status"] != float64(200) {
		t.Errorf("webhook_http_status = %v, want 200", body["webhook_http_status"])
	}
	// And the real decider was never consulted, because the failure is forced
	// before it is reached.
	if decider.calls != before {
		t.Error("the real decider was consulted during a forced failure")
	}
}

// TestSimulateEndpointsDoNotLeakTheForcedFailure is the isolation guarantee at
// the API layer.
//
// The forced-failure endpoint marks its own context. This asserts that marking
// does not persist into the handler and affect the NEXT request — a leak that
// would turn one demo click into every subsequent payment failing, which is
// exactly the Day 4 hazard in a subtler form.
func TestSimulateEndpointsDoNotLeakTheForcedFailure(t *testing.T) {
	h, decider := newSimulateHandler(t)
	created := &createdRows{}
	cleanupSimulated(t, h, created)

	// Force one failure.
	if rec, body := postJSON(t, h, "/api/simulate/llm-failure", created); rec.Code != http.StatusOK {
		t.Fatalf("forced failure did not succeed: %v", body)
	}

	// Then an ordinary simulated failure through the same handler.
	before := decider.calls
	rec, body := postJSON(t, h, "/api/simulate/failure?category=insufficient_funds", created)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %v", rec.Code, body)
	}

	d := decisionOf(t, body)
	if d["source"] == "fallback_rule" {
		t.Fatal("the request AFTER a forced failure also fell back — the marker leaked " +
			"beyond its own request, which is the Day 4 hazard in a subtler shape")
	}
	if decider.calls != before+1 {
		t.Error("the real decider was not reached on the following request")
	}
}

// TestWebhookThroughAPIHandlerIsNeverForced closes the loop at the mount point:
// the api.Handler holds the webhook handler, and a request routed to the webhook
// path must never pick up the marker.
func TestWebhookThroughAPIHandlerIsNeverForced(t *testing.T) {
	h, decider := newSimulateHandler(t)
	created := &createdRows{}
	cleanupSimulated(t, h, created)

	// Force a failure first, so any shared state would be contaminated.
	postJSON(t, h, "/api/simulate/llm-failure", created)

	before := decider.calls
	// Unique per run and registered for cleanup. Fixed ids would survive in
	// webhook_events — dedupe is persistent — and every later run would be
	// answered as a duplicate without ever reaching the decider.
	paymentID := fmt.Sprintf("pay_api_isolation_probe_%d", time.Now().UnixNano())
	eventID := fmt.Sprintf("evt_api_isolation_probe_%d", time.Now().UnixNano())
	created.payment(paymentID)
	created.event(eventID)

	body := `{"event":"payment.failed","payload":{"payment":{"entity":{` +
		`"id":"` + paymentID + `","amount":50000,"method":"card",` +
		`"error_code":"BAD_REQUEST_ERROR","error_reason":"insufficient_funds",` +
		`"error_source":"customer","error_step":"payment_authorization"}}}}`

	req := httptest.NewRequest(http.MethodPost, "/webhook/payment-failed", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-razorpay-event-id", eventID)
	// Signed like a real delivery: this test is about the forced-failure marker
	// not leaking, and an unsigned request would now be rejected before it could
	// demonstrate anything about that.
	//
	// Read from the environment rather than using the constant, because
	// newSimulateHandler only defaults that variable and leaves a real secret in
	// place — signing with the constant would fail wherever one is set.
	req.Header.Set("x-razorpay-signature", ingest.Sign(os.Getenv("RAZORPAY_WEBHOOK_SECRET"), []byte(body)))
	rec := httptest.NewRecorder()
	h.webhook.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("webhook answered %d, want 200", rec.Code)
	}
	if decider.calls != before+1 {
		t.Fatal("the webhook did not reach the real decider after a forced-failure call — " +
			"the demo endpoint contaminated the production path")
	}
}

// countingDecider is a stand-in for the model that records whether it was
// actually consulted. Several tests here turn on that distinction: "the
// pipeline ran" and "something invented an answer" look identical in the
// response body.
type countingDecider struct{ calls int }

func (d *countingDecider) Decide(ctx context.Context, in decide.DecisionInput) (decide.Decision, decide.Outcome, error) {
	d.calls++
	return decide.Decision{
		Action:          decide.ActionRetryDelayed,
		Confidence:      0.90,
		Reasoning:       "Stubbed for tests.",
		CustomerMessage: "We'll automatically retry your payment shortly.",
	}, decide.Outcome{}, nil
}

// createdRows collects the ids one test created, so cleanup can delete exactly
// those and nothing else.
//
// The endpoints under test generate their own random ids internally, so the test
// cannot know them in advance — it records them from the responses instead.
type createdRows struct {
	paymentIDs []string
	eventIDs   []string
	runIDs     []int64
}

func (c *createdRows) payment(id string) {
	if id != "" {
		c.paymentIDs = append(c.paymentIDs, id)
	}
}

func (c *createdRows) event(id string) {
	if id != "" {
		c.eventIDs = append(c.eventIDs, id)
	}
}

// track records every id present in a simulate endpoint's response body.
func (c *createdRows) track(body map[string]any) {
	if d, ok := body["decision"].(map[string]any); ok {
		if id, ok := d["payment_id"].(string); ok {
			c.payment(id)
		}
	}
	if id, ok := body["event_id"].(string); ok {
		c.event(id)
	}
	if id, ok := body["id"].(float64); ok {
		c.runIDs = append(c.runIDs, int64(id))
	}
}

// cleanupSimulated removes exactly the rows a test created, by id.
//
// It used to delete by timestamp window — anything created since the test
// started. That is wrong in a way that only shows up under load: `go test ./...`
// runs packages in parallel, so one package's cleanup would delete another
// package's live rows mid-test, and the victim varied from run to run. It broke
// TestDecisionsAccumulatePerAttempt and TestDecisionPersistedFromStoppingRule in
// internal/ingest, neither of which this package should be able to touch.
//
// The same reasoning already applies to the queries this project writes for
// production: a timestamp window is not an identifier, and anything that shares
// the window is collateral. Scoping by id is what every other test helper here
// already does.
//
// A run also writes rows this list cannot name — a batch triggered through the
// API creates its own payments internally. Those are collected from the
// batch_runs id instead, via the outcomes that point at them.
func cleanupSimulated(t *testing.T, h *Handler, created *createdRows) {
	t.Helper()
	t.Cleanup(func() {
		// A batch run's payments are discovered through its outcomes rows, since
		// the batch generated the ids itself and never reported them.
		for _, runID := range created.runIDs {
			rows, err := h.db.Query(`
				SELECT DISTINCT o.payment_id
				FROM outcomes o
				JOIN batch_runs b ON b.id = $1
				WHERE o.recorded_at BETWEEN b.started_at AND COALESCE(b.completed_at, now())`, runID)
			if err != nil {
				t.Errorf("collect batch payments: %v", err)
				continue
			}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err == nil {
					created.payment(id)
				}
			}
			rows.Close()
		}

		// Children first: the schema deliberately has no ON DELETE CASCADE.
		if len(created.paymentIDs) > 0 {
			if _, err := h.db.Exec(`DELETE FROM outcomes WHERE payment_id = ANY($1)`, created.paymentIDs); err != nil {
				t.Errorf("cleanup outcomes: %v", err)
			}
			if _, err := h.db.Exec(`DELETE FROM decisions WHERE payment_id = ANY($1)`, created.paymentIDs); err != nil {
				t.Errorf("cleanup decisions: %v", err)
			}
			if _, err := h.db.Exec(`DELETE FROM failed_payments WHERE payment_id = ANY($1)`, created.paymentIDs); err != nil {
				t.Errorf("cleanup failed_payments: %v", err)
			}
		}
		if len(created.eventIDs) > 0 {
			if _, err := h.db.Exec(`DELETE FROM webhook_events WHERE event_id = ANY($1)`, created.eventIDs); err != nil {
				t.Errorf("cleanup webhook_events: %v", err)
			}
		}
		for _, runID := range created.runIDs {
			if _, err := h.db.Exec(`DELETE FROM batch_runs WHERE id = $1`, runID); err != nil {
				t.Errorf("cleanup batch_runs: %v", err)
			}
		}
	})
}
