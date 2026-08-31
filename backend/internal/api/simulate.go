package api

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/ingest"
	"github.com/bhavyamsharmaa/recovery-agent/internal/simulate"
	"github.com/bhavyamsharmaa/recovery-agent/internal/trace"
)

// The demo control panel's endpoints.
//
// These fire real webhooks through the real handler — the same classifier,
// dedupe, attempt counter, stopping rule, decision layer and confidence gate
// that production traffic goes through — and then report what actually happened
// by reading the database back. Nothing here shortcuts the pipeline or invents a
// result to display.
//
// They dispatch in-process rather than over a TCP loopback: it is the same
// http.Handler either way, so the path exercised is identical, and a demo
// clicked on camera should not be able to fail because a port moved. The one
// thing in-process dispatch makes possible is the request-scoped forced failure
// below, which cannot be carried across an HTTP boundary and must not be
// carriable by a header that the real endpoint would then also accept.
//
// LIKE EVERY ROUTE HERE, THESE ARE UNAUTHENTICATED, and unlike the read routes
// they write. They exist for a local demo. A deployment that exposes them lets
// anyone manufacture payment records.

// simulateTimeout bounds one simulated delivery, which includes a real model
// call. It is deliberately longer than queryTimeout, which bounds a plain read.
const simulateTimeout = 45 * time.Second

// decisionJSON is what the control panel shows after a click: the decision the
// real pipeline reached, not a summary invented here.
type decisionJSON struct {
	PaymentID string `json:"payment_id"`
	Category  string `json:"category"`
	// AmountPaise so the panel can render the same rupee formatting as the
	// rest of the dashboard rather than a bare id.
	AmountPaise  int64 `json:"amount_paise"`
	AttemptCount int   `json:"attempt_count"`

	Action string `json:"action"`
	Source string `json:"source"`
	// Confidence is null for stopping-rule and fallback decisions. It must stay
	// null rather than 0 here for the same reason it does everywhere else: a
	// rule-made decision had no model behind it, and 0 would claim the model was
	// certain it was wrong.
	Confidence      *float64 `json:"confidence"`
	Reasoning       *string  `json:"reasoning"`
	CustomerMessage *string  `json:"customer_message"`
	EscalationReas  *string  `json:"escalation_reason"`
	OriginalAction  *string  `json:"original_action"`
}

// deliveryJSON is one webhook delivery's result, used by the duplicate demo
// where two deliveries must be told apart on screen.
type deliveryJSON struct {
	EventID string `json:"event_id"`
	// Status is "processed" or "duplicate". It is derived from what the database
	// shows before and after the delivery, not from the HTTP response — the
	// handler answers 200 either way, by design, because Razorpay must not
	// redeliver something already handled.
	Status       string `json:"status"`
	AttemptCount int    `json:"attempt_count"`
	DecisionsFor int    `json:"decisions_for_payment"`
	HTTPStatus   int    `json:"http_status"`
}

// fireWebhook dispatches one event through the real webhook handler.
//
// ctx carries the request scope, and for the forced-failure endpoint it also
// carries the marker that makes the decision layer fail. That marker exists
// nowhere a real webhook can reach.
func (h *Handler) fireWebhook(ctx context.Context, eventID string, body []byte) (int, error) {
	if h.webhook == nil {
		return 0, errors.New("api: no webhook handler wired; simulate endpoints are unavailable")
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook/payment-failed", bytes.NewReader(body)).
		WithContext(ctx)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-razorpay-event-id", eventID)

	// Signed like any other delivery. These endpoints dispatch through the real
	// handler precisely so the demo exercises the real pipeline, and the
	// signature check is now part of that pipeline — a bypass here would make
	// the control panel demonstrate a path production does not have, and would
	// be the one hole in an otherwise closed endpoint.
	//
	// The secret is read per call rather than cached at construction, so the
	// key gate above stays the only reason these endpoints can fail at startup.
	req.Header.Set("x-razorpay-signature", ingest.Sign(os.Getenv("RAZORPAY_WEBHOOK_SECRET"), body))

	rec := httptest.NewRecorder()
	h.webhook.ServeHTTP(rec, req)
	return rec.Code, nil
}

// readDecision reads back what the pipeline decided about a payment.
func readDecision(ctx context.Context, db *sql.DB, paymentID string) (decisionJSON, error) {
	full, err := trace.Load(ctx, db, paymentID)
	if err != nil {
		return decisionJSON{}, err
	}
	out := decisionJSON{
		PaymentID:    full.Payment.PaymentID,
		Category:     full.Payment.Category,
		AmountPaise:  full.Payment.AmountPaise,
		AttemptCount: full.Payment.AttemptCount,
	}
	if len(full.Decisions) == 0 {
		return out, nil
	}
	// The last decision is the effective one: the confidence gate records its
	// override alongside the answer it overrode.
	d := full.Decisions[len(full.Decisions)-1]
	out.Action = d.Action
	out.Source = d.Source
	out.Confidence = nullFloat(d.Confidence)
	out.Reasoning = nullString(d.Reasoning)
	out.CustomerMessage = nullString(d.CustomerMessage)
	out.EscalationReas = nullString(d.EscalationReason)
	out.OriginalAction = nullString(d.OriginalAction)
	return out, nil
}

// simulateFailure serves POST /api/simulate/failure?category=X.
//
// One real webhook in the named category, then the decision it produced. The
// category is validated against the simulator's own scenario list rather than a
// second list kept here, so a category added there cannot be silently rejected.
func (h *Handler) simulateFailure(w http.ResponseWriter, r *http.Request) {
	category := r.URL.Query().Get("category")
	if category == "" || !simulate.KnownScenario(category) || category == "duplicate" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":      "unknown category",
			"categories": simulate.ScenarioNames,
		})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), simulateTimeout)
	defer cancel()

	// Clock-seeded: a demo click should produce a fresh payment every time, not
	// replay one the database already remembers.
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	paymentID := simulate.RazorpayID(rng, "pay")
	eventID := simulate.RazorpayID(rng, "evt")
	body := simulate.MustMarshal(simulate.Build(rng, category, paymentID))

	status, err := h.fireWebhook(ctx, eventID, body)
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, "could not dispatch the webhook", err)
		return
	}
	if status < 200 || status > 299 {
		h.fail(w, r, http.StatusBadGateway, "the webhook rejected the simulated failure",
			fmt.Errorf("webhook returned %d", status))
		return
	}

	decision, err := readDecision(ctx, h.db, paymentID)
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, "could not read the decision back", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"event_id": eventID,
		"decision": decision,
	})
}

// simulateDuplicate serves POST /api/simulate/duplicate.
//
// The same event id twice in quick succession. Both deliveries answer 200 —
// that is the contract, because a redelivery Razorpay is told to retry is worse
// than one it is told was handled — so the demonstration is what the database
// shows: the first delivery counts an attempt and records a decision, the second
// changes nothing.
func (h *Handler) simulateDuplicate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), simulateTimeout)
	defer cancel()

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	paymentID := simulate.RazorpayID(rng, "pay")
	eventID := simulate.RazorpayID(rng, "evt")
	// One body, marshalled once and sent twice: a redelivery is the identical
	// event arriving again, byte for byte, not a new event about the same
	// payment. Rebuilding it would change the amount and make this a different
	// test.
	body := simulate.MustMarshal(simulate.Build(rng, "duplicate", paymentID))

	deliveries := make([]deliveryJSON, 0, 2)
	for i := 0; i < 2; i++ {
		status, err := h.fireWebhook(ctx, eventID, body)
		if err != nil {
			h.fail(w, r, http.StatusInternalServerError, "could not dispatch the webhook", err)
			return
		}

		after, err := readDecision(ctx, h.db, paymentID)
		if err != nil {
			h.fail(w, r, http.StatusInternalServerError, "could not read state back", err)
			return
		}
		decisions, err := countDecisions(ctx, h.db, paymentID)
		if err != nil {
			h.fail(w, r, http.StatusInternalServerError, "could not count decisions", err)
			return
		}

		// Duplicate is inferred from observed state, not claimed: the second
		// delivery is a duplicate precisely because it left the attempt count
		// and the decision count where the first one put them.
		state := "processed"
		if i > 0 {
			prev := deliveries[0]
			if after.AttemptCount == prev.AttemptCount && decisions == prev.DecisionsFor {
				state = "duplicate"
			}
		}
		deliveries = append(deliveries, deliveryJSON{
			EventID:      eventID,
			Status:       state,
			AttemptCount: after.AttemptCount,
			DecisionsFor: decisions,
			HTTPStatus:   status,
		})
	}

	decision, err := readDecision(ctx, h.db, paymentID)
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, "could not read the decision back", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"event_id":   eventID,
		"deliveries": deliveries,
		"decision":   decision,
	})
}

// simulateLLMFailure serves POST /api/simulate/llm-failure.
//
// The decision layer is made to fail for this one request, and only this one:
// the instruction is a value on the request's context, put there here, and
// nothing that arrives on /webhook/payment-failed carries it. See
// ingest/forcefail.go for why this replaced the FORCE_DECIDE_FAILURE
// environment variable that was removed before the Day 4 merge — a global
// switch is one deployment mistake away from failing production decisions
// silently, and this cannot be switched on for traffic that did not ask.
//
// No restart and no configuration change is needed, which is the other half of
// the point: the capability exists only along this code path.
func (h *Handler) simulateLLMFailure(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), simulateTimeout)
	defer cancel()

	// The marker. Everything downstream is the ordinary pipeline.
	ctx = ingest.WithForcedDecideFailure(ctx)

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	paymentID := simulate.RazorpayID(rng, "pay")
	eventID := simulate.RazorpayID(rng, "evt")

	// A category with retry budget left, so the stopping rule does not answer
	// before the decision layer is ever reached. A hard_decline would escalate
	// at budget 0 and never demonstrate the fallback at all.
	body := simulate.MustMarshal(simulate.Build(rng, "insufficient_funds", paymentID))

	status, err := h.fireWebhook(ctx, eventID, body)
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, "could not dispatch the webhook", err)
		return
	}

	decision, err := readDecision(ctx, h.db, paymentID)
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, "could not read the decision back", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"event_id": eventID,
		// The webhook still answers 200. That is the guarantee being shown: the
		// model failing twice does not become Razorpay's problem, and the
		// payment still gets a decision and a customer message.
		"webhook_http_status": status,
		"forced":              true,
		"decision":            decision,
	})
}

func countDecisions(ctx context.Context, db *sql.DB, paymentID string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM decisions WHERE payment_id = $1`, paymentID).Scan(&n)
	return n, err
}
