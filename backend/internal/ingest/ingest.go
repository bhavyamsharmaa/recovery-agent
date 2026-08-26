// Package ingest receives Razorpay payment.failed webhooks, drops redeliveries,
// classifies the failure, and asks the decision layer what to do about it.
package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/classify"
	"github.com/bhavyamsharmaa/recovery-agent/internal/decide"
)

// decideTimeout bounds the LLM call. The decide client deliberately sets no
// internal timeout and defers cancellation to its caller, which is here.
// Without this the webhook would stay open as long as the API takes to answer,
// and Razorpay would give up and redeliver.
const decideTimeout = 10 * time.Second

// retryBudgets mirrors the retry-budget table in docs/taxonomy.md. Change it
// there first. "unknown" is absent on purpose: it never reaches the decision
// layer.
var retryBudgets = map[classify.Category]int{
	classify.CategoryInsufficientFunds: 1,
	classify.CategoryBankDowntime:      3,
	classify.CategoryHardDecline:       0,
	classify.CategorySoftDecline:       2,
	classify.CategoryNetworkError:      3,
}

// Escalation reasons say WHY something was escalated, so a consumer reading a
// queue of escalations can group them by cause rather than re-deriving it.
// Expected to grow into a larger set as more escalation paths appear.
const (
	EscalationReasonBudgetExhausted = "retry_budget_exhausted"
	EscalationReasonLowConfidence   = "low_confidence"
)

// confidenceThreshold is the floor for acting on the model's own decision.
// Below it the action is overridden to escalate: a decision the model is not
// confident in is one a human should look at, whatever it recommends.
//
// Set at 0.75 based on observed score distribution (0.68-0.95) across Day 2
// test scenarios — calibrated so the escalation path is genuinely exercised,
// not theoretical.
//
// The observed scores are heavily quantised — 0.68, 0.75, 0.78, 0.82, 0.85 —
// rather than continuous, and 0.75 has been seen exactly on the boundary. The
// comparison below is >=, so an exact 0.75 is acted on rather than escalated.
// Moving this constant even slightly would move a whole cluster across the
// line at once, so it is not a dial to nudge.
const confidenceThreshold = 0.75

// escalationMessages are what the customer is told when the stopping rule
// fires. Static by necessity: the stopping rule exists to avoid the model call,
// so generating these would defeat it. They are held to the same constraints as
// the model's own customer_message (see decide.messageConstraints) — no
// timeframe, no promised outcome, always one concrete next action.
var escalationMessages = map[classify.Category]string{
	classify.CategoryHardDecline:       "Your payment couldn't be completed. Please try a different card or contact your bank.",
	classify.CategoryInsufficientFunds: "We were unable to process your payment after multiple attempts. Please try a different payment method.",
	classify.CategoryBankDowntime:      "We were unable to complete your payment after multiple attempts. Please try again later or use a different payment method.",
	classify.CategorySoftDecline:       "We were unable to process your payment after multiple attempts. Please check your payment details and try again, or use a different method.",
	classify.CategoryNetworkError:      "We were unable to complete your payment due to a technical issue. Please try again or use a different payment method.",
}

// escalationMessageFallback covers unknown and any category added to the
// taxonomy without a message being written for it. Saying less is correct here:
// with no idea why the payment failed, naming a cause would be a guess.
const escalationMessageFallback = "We were unable to complete your payment. Please contact support or try a different payment method."

// escalationMessage returns the customer-facing text for a stopped payment.
func escalationMessage(c classify.Category) string {
	if m, ok := escalationMessages[c]; ok {
		return m
	}
	return escalationMessageFallback
}

// Decider is the decision layer as this package needs it. An interface rather
// than a concrete client so the handler can be tested without an API key.
type Decider interface {
	Decide(ctx context.Context, in decide.DecisionInput) (decide.Decision, decide.Outcome, error)
}

// Event is Razorpay's payment.failed webhook envelope. Only the fields we
// actually read are declared; unknown fields are ignored on decode.
type Event struct {
	Event   string `json:"event"`
	Payload struct {
		Payment struct {
			Entity PaymentEntity `json:"entity"`
		} `json:"payment"`
	} `json:"payload"`
}

type PaymentEntity struct {
	ID               string `json:"id"`
	Amount           int    `json:"amount"` // paise
	Currency         string `json:"currency"`
	Method           string `json:"method"`
	ErrorCode        string `json:"error_code"`
	ErrorDescription string `json:"error_description"`
	ErrorReason      string `json:"error_reason"`
	ErrorSource      string `json:"error_source"`
	ErrorStep        string `json:"error_step"`
	OrderID          string `json:"order_id"`
}

// eventIDHeader is where Razorpay carries the id of the webhook delivery. It
// is the only stable handle a receiver has for recognising a redelivery. The
// payment id is not one: a payment that fails a second time genuinely produces
// a second event, which must be counted as an attempt rather than dropped.
const eventIDHeader = "X-Razorpay-Event-Id"

// Handler serves POST /webhook/payment-failed.
type Handler struct {
	// seen holds every event id processed so far. In-memory only: a restart
	// forgets everything and redeliveries would be reprocessed. Day 5 replaces
	// this with the database.
	seen sync.Map

	attempts AttemptStore
	decider  Decider
}

// NewHandler takes the AttemptStore interface rather than a concrete store, so
// Day 5's Postgres implementation drops in at the construction site without
// this package changing.
func NewHandler(d Decider, a AttemptStore) *Handler {
	return &Handler{decider: d, attempts: a}
}

type receivedLog struct {
	Event       string            `json:"event"`
	PaymentID   string            `json:"payment_id"`
	Category    classify.Category `json:"category"`
	AmountPaise int               `json:"amount_paise"`
	ErrorCode   string            `json:"error_code"`
	ErrorReason string            `json:"error_reason"`

	// Absent when the category was unknown (no call made) or the call failed.
	// Confidence is a pointer so a genuine 0.0 is still emitted rather than
	// silently dropped by omitempty.
	DecisionAction          string   `json:"decision_action,omitempty"`
	DecisionConfidence      *float64 `json:"decision_confidence,omitempty"`
	DecisionAlternateMethod string   `json:"decision_alternate_method,omitempty"`

	TS string `json:"ts"`
}

type decisionFailedLog struct {
	Event     string            `json:"event"`
	PaymentID string            `json:"payment_id"`
	Category  classify.Category `json:"category"`
	Error     string            `json:"error"`
	TS        string            `json:"ts"`
}

// decisionRetryLog records that the model had to be asked twice. Decide() does
// not know the payment id, so it reports only that a retry happened and the
// handler — which owns the id — writes the line.
type decisionRetryLog struct {
	Event     string `json:"event"`
	PaymentID string `json:"payment_id"`
	Reason    string `json:"reason"`
	TS        string `json:"ts"`
}

type duplicateLog struct {
	Event     string `json:"event"`
	EventID   string `json:"event_id"`
	PaymentID string `json:"payment_id"`
	TS        string `json:"ts"`
}

// confidenceOverrideLog records a decision the model made and the gate
// replaced. It is emitted alongside the payment_received line rather than
// instead of it: received carries the action actually used, this carries the
// fact that the model wanted something else.
type confidenceOverrideLog struct {
	Event            string            `json:"event"`
	PaymentID        string            `json:"payment_id"`
	Category         classify.Category `json:"category"`
	OriginalAction   string            `json:"original_action"`
	OverriddenAction string            `json:"overridden_action"`
	Confidence       float64           `json:"confidence"`
	EscalationReason string            `json:"escalation_reason"`

	// OriginalAlternateMethod is what the gate cleared, kept so a reviewer can
	// see what was overridden away rather than only that something was. Omitted
	// when the overridden action never carried one, which is most of the time.
	OriginalAlternateMethod string `json:"original_alternate_method,omitempty"`

	TS string `json:"ts"`
}

// stoppingRuleLog records a payment stopped before the decision layer was
// consulted. It carries no decision fields because no decision was made.
type stoppingRuleLog struct {
	Event            string            `json:"event"`
	PaymentID        string            `json:"payment_id"`
	Category         classify.Category `json:"category"`
	AttemptCount     int               `json:"attempt_count"`
	Budget           int               `json:"budget"`
	EscalationReason string            `json:"escalation_reason"`

	// EscalationCustomerMessage is the static text above, carried on the line so
	// the audit trail shows what the customer was told — the same visibility a
	// normal decision's customer_message gets. No action or confidence field
	// accompanies it: the action is implicitly escalate, and nothing here was
	// inferred, so there is no confidence to report.
	EscalationCustomerMessage string `json:"escalation_customer_message"`

	TS string `json:"ts"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var e Event
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		fmt.Fprintf(os.Stderr, "webhook body did not parse: %v\n", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	p := e.Payload.Payment.Entity
	if p.ID == "" {
		fmt.Fprintln(os.Stderr, "webhook parsed but payload.payment.entity.id was empty")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// The idempotency key is the delivery's event id, not the payment id and not
	// e.Event — that field holds the event *type* ("payment.failed") and is
	// identical across every webhook. Keying on the payment id would conflate a
	// redelivery of one failure with a genuine second failure of the same
	// payment, and the second is exactly what the attempt counter below exists
	// to count.
	//
	// A delivery with no event id header cannot be deduplicated, so it is
	// processed rather than dropped. Dropping it would lose a real failure; the
	// retry budget still bounds what happens next.
	eventID := r.Header.Get(eventIDHeader)
	if eventID != "" {
		// LoadOrStore is one atomic step. A Load-then-Store pair would let two
		// concurrent redeliveries of the same event both read "not seen" and both
		// be treated as new, which is the exact failure this check exists to
		// prevent.
		if _, loaded := h.seen.LoadOrStore(eventID, struct{}{}); loaded {
			logLine(duplicateLog{
				Event:     "duplicate",
				EventID:   eventID,
				PaymentID: p.ID,
				TS:        now(),
			})
			ok(w)
			return
		}
	}

	category := classify.Classify(p.ErrorReason, p.ErrorSource)

	// Attempts are counted per payment id — how many times THIS payment has been
	// seen — not per category.
	//
	// The count is READ first and only written once the payment is going to be
	// acted on, so a stopped payment does not consume an attempt it never got.
	// Note that this read and the write below are not one atomic step: two
	// concurrent deliveries of the same payment can both read the same count and
	// both proceed. See the note in docs/README.md.
	priorAttempts := h.attempts.Get(p.ID)

	// A category absent from the table budgets 0, which is the safe direction:
	// hard_decline and unknown both land here and both must stop.
	budget := retryBudgets[category]

	// The stopping rule runs before the decision layer, not after. Once the
	// budget is spent there is no automated action left to take on this payment,
	// so asking the model would spend a call on a question whose answer cannot
	// be used.
	//
	// >= rather than >: priorAttempts counts attempts already made, so having
	// made as many as the budget allows means there are none left.
	if priorAttempts >= budget {
		logLine(stoppingRuleLog{
			Event:            "stopping_rule_triggered",
			PaymentID:        p.ID,
			Category:         category,
			AttemptCount:     priorAttempts,
			Budget:           budget,
			EscalationReason: EscalationReasonBudgetExhausted,

			EscalationCustomerMessage: escalationMessage(category),

			TS: now(),
		})
		ok(w)
		return
	}

	// Past the budget check, so this payment is being acted on: record the
	// attempt. attemptCount is this attempt's own number, 1 for the first.
	attemptCount := h.attempts.Increment(p.ID)

	received := receivedLog{
		Event:       "payment_received",
		PaymentID:   p.ID,
		Category:    category,
		AmountPaise: p.Amount,
		ErrorCode:   p.ErrorCode,
		ErrorReason: p.ErrorReason,
		TS:          now(),
	}

	// An unknown category means no rule matched, so there is nothing to reason
	// from. Asking the model anyway would be inviting a guess.
	var decideErr error
	var decideRetried bool
	var override *confidenceOverrideLog
	if category != classify.CategoryUnknown {
		ctx, cancel := context.WithTimeout(r.Context(), decideTimeout)
		d, outcome, err := h.decider.Decide(ctx, decide.DecisionInput{
			Category:      string(category),
			ErrorReason:   p.ErrorReason,
			PaymentMethod: p.Method,
			AmountPaise:   int64(p.Amount),

			AttemptCount: attemptCount,

			// Remaining counts the retries still available after this sighting.
			// The stopping rule above guarantees attemptCount <= budget here, so
			// this never goes negative.
			RemainingRetryBudget: budget - (attemptCount - 1),

			// TODO(day5): needs the timestamp of the first failure, which needs
			// persistence.
			TimeSinceFailureSeconds: 0,
		})
		cancel()

		decideRetried = outcome.Retried
		if err != nil {
			decideErr = err
		} else {
			// The confidence gate is deliberately separate from the stopping rule
			// above. Both can end in escalate, but they answer different questions:
			// the stopping rule asks whether any automated action is left to take,
			// this asks whether the model's answer is trustworthy enough to act on.
			// Collapsing them would lose that distinction in the logs, which is
			// exactly what escalation_reason exists to preserve.
			gated, overridden := applyConfidenceGate(d)
			if overridden {
				override = &confidenceOverrideLog{
					Event:            "confidence_override",
					PaymentID:        p.ID,
					Category:         category,
					OriginalAction:   d.Action,
					OverriddenAction: gated.Action,
					Confidence:       d.Confidence,
					EscalationReason: EscalationReasonLowConfidence,

					// Read from d, not gated: gated is the post-clear copy.
					OriginalAlternateMethod: d.AlternateMethod,

					TS: now(),
				}
			}

			// received reports the action actually used, which is the post-gate one.
			confidence := gated.Confidence
			received.DecisionAction = gated.Action
			received.DecisionConfidence = &confidence
			received.DecisionAlternateMethod = gated.AlternateMethod
		}
	}

	logLine(received)

	if override != nil {
		logLine(*override)
	}

	// Logged whether or not the second attempt succeeded — a rising retry rate is
	// worth seeing even while the outcomes stay good.
	if decideRetried {
		logLine(decisionRetryLog{
			Event:     "decision_retry",
			PaymentID: p.ID,
			Reason:    "parse_error",
			TS:        now(),
		})
	}

	// A failed decision is logged and dropped. Razorpay retries any non-2xx, so
	// failing the webhook because the model was unavailable would just produce
	// redeliveries of a payment we already recorded.
	if decideErr != nil {
		logLine(decisionFailedLog{
			Event:     "decision_failed",
			PaymentID: p.ID,
			Category:  category,
			Error:     decideErr.Error(),
			TS:        now(),
		})
	}

	ok(w)
}

// applyConfidenceGate overrides a low-confidence decision to escalate and
// reports whether it did. The returned Decision is the one to use downstream.
//
// Confidence itself is left untouched: it records how sure the model was about
// the answer it gave, and overwriting it would destroy the very signal that
// triggered the override.
//
// reasoning and customer_message are also left as written. A customer_message
// that reads oddly next to an escalate action is a known gap.
func applyConfidenceGate(d decide.Decision) (decide.Decision, bool) {
	if d.Confidence >= confidenceThreshold {
		return d, false
	}
	d.Action = decide.ActionEscalate

	// alternate_method only means anything alongside suggest_alternate_method.
	// Carrying it onto an escalate would leave the dangling suggestion that
	// decide.validate() rejects everywhere else: a consumer reading the field
	// without checking the action would act on advice no longer being given.
	d.AlternateMethod = ""

	return d, true
}

// ok answers 200 for new events and duplicates alike. Razorpay retries any
// non-2xx, so returning an error for a duplicate would produce more duplicates.
func ok(w http.ResponseWriter) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, `{"status":"ok"}`)
}

// logOut is where log lines go. A package var rather than os.Stdout inline so
// tests can capture what the handler emitted; production never reassigns it.
var logOut io.Writer = os.Stdout

func logLine(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not marshal log line: %v\n", err)
		return
	}
	fmt.Fprintln(logOut, string(b))
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }
