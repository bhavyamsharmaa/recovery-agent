package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	webhook := ingest.NewHandler(&ingest.ForcedFailureDecider{Real: decider}, attempts).
		WithDecisionRecorder(ingest.NewPostgresDecisionStore(pool)).
		WithEventStore(ingest.NewPostgresEventStore(pool))

	return NewHandler(pool).WithWebhook(webhook), decider
}

func postJSON(t *testing.T, h *Handler, target string) (*httptest.ResponseRecorder, map[string]any) {
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
	cleanupSimulated(t, h)

	for _, category := range []string{"insufficient_funds", "soft_decline", "network_error"} {
		t.Run(category, func(t *testing.T) {
			before := decider.calls
			rec, body := postJSON(t, h, "/api/simulate/failure?category="+category)

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

	for _, c := range []string{"", "not_a_category", "duplicate"} {
		rec, body := postJSON(t, h, "/api/simulate/failure?category="+c)
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
	cleanupSimulated(t, h)

	rec, body := postJSON(t, h, "/api/simulate/duplicate")
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
	cleanupSimulated(t, h)

	before := decider.calls
	rec, body := postJSON(t, h, "/api/simulate/llm-failure")
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
	cleanupSimulated(t, h)

	// Force one failure.
	if rec, body := postJSON(t, h, "/api/simulate/llm-failure"); rec.Code != http.StatusOK {
		t.Fatalf("forced failure did not succeed: %v", body)
	}

	// Then an ordinary simulated failure through the same handler.
	before := decider.calls
	rec, body := postJSON(t, h, "/api/simulate/failure?category=insufficient_funds")
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
	cleanupSimulated(t, h)

	// Force a failure first, so any shared state would be contaminated.
	postJSON(t, h, "/api/simulate/llm-failure")

	before := decider.calls
	body := `{"event":"payment.failed","payload":{"payment":{"entity":{` +
		`"id":"pay_api_isolation_probe","amount":50000,"method":"card",` +
		`"error_code":"BAD_REQUEST_ERROR","error_reason":"insufficient_funds",` +
		`"error_source":"customer","error_step":"payment_authorization"}}}}`

	req := httptest.NewRequest(http.MethodPost, "/webhook/payment-failed", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-razorpay-event-id", "evt_api_isolation_probe")
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

// cleanupSimulated removes every row a simulate test created. The endpoints
// generate their own random ids, so the sweep is by shape rather than by a list
// of ids the test kept: anything created during the test window that these
// endpoints could have made.
func cleanupSimulated(t *testing.T, h *Handler) {
	t.Helper()
	start := time.Now().UTC().Add(-time.Second)
	t.Cleanup(func() {
		// Children first: the schema deliberately has no ON DELETE CASCADE.
		if _, err := h.db.Exec(
			`DELETE FROM outcomes WHERE payment_id IN (SELECT payment_id FROM failed_payments WHERE created_at >= $1)`, start); err != nil {
			t.Errorf("cleanup outcomes: %v", err)
		}
		if _, err := h.db.Exec(
			`DELETE FROM decisions WHERE payment_id IN (SELECT payment_id FROM failed_payments WHERE created_at >= $1)`, start); err != nil {
			t.Errorf("cleanup decisions: %v", err)
		}
		if _, err := h.db.Exec(`DELETE FROM webhook_events WHERE received_at >= $1`, start); err != nil {
			t.Errorf("cleanup webhook_events: %v", err)
		}
		if _, err := h.db.Exec(`DELETE FROM failed_payments WHERE created_at >= $1`, start); err != nil {
			t.Errorf("cleanup failed_payments: %v", err)
		}
	})
}
