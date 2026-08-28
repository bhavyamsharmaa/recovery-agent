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

// Decision sources say WHO decided, so no line in the log leaves that to be
// inferred from which fields happen to be present. Every resolved payment
// carries exactly one of these.
const (
	DecisionSourceLLM          = "llm"
	DecisionSourceStoppingRule = "stopping_rule"
	DecisionSourceFallbackRule = "fallback_rule"
)

// fallbackDecision is applied when the decision layer fails on both its
// original attempt and its retry. It is static by necessity: the failure being
// handled is the model being unusable, so asking it again is the one thing that
// cannot work.
//
// no_retry rather than escalate or a retry, because nothing is known about this
// payment beyond that the system could not reason about it. Retrying acts on a
// judgement never made; escalating claims a policy reason that was never
// reached. no_retry stops and says so.
//
// The message is deliberately generic, unlike the category-specific stopping
// rule messages: this is a system-level outage, not a policy decision about
// this category of failure, and telling the customer something specific would
// imply an understanding of their payment that was never obtained.
//
// Confidence is 0 and is not a model score. It is not passed through the
// confidence gate — the gate exists to second-guess the model, and there is no
// model output here to second-guess.
func fallbackDecision() decide.Decision {
	return decide.Decision{
		Action:          decide.ActionNoRetry,
		Confidence:      0,
		Reasoning:       "LLM decision layer failed after retry; falling back to conservative no-retry policy pending manual review.",
		CustomerMessage: "We were unable to complete your payment. Please try a different payment method or contact support.",
		AlternateMethod: "",
	}
}

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

	// decisions is optional. Nil means decisions are logged but not stored,
	// which is how every test that predates persistence still builds a Handler.
	decisions DecisionRecorder
}

// NewHandler takes the AttemptStore interface rather than a concrete store, so
// Day 5's Postgres implementation drops in at the construction site without
// this package changing.
func NewHandler(d Decider, a AttemptStore) *Handler {
	return &Handler{decider: d, attempts: a}
}

// WithDecisionRecorder attaches durable decision storage. It is a separate
// call rather than a third parameter so that NewHandler keeps the signature
// every existing caller and test already uses.
func (h *Handler) WithDecisionRecorder(r DecisionRecorder) *Handler {
	h.decisions = r
	return h
}

// recordDecision persists one decision, if storage is attached.
//
// A failure to store is reported and swallowed. The webhook has already been
// answered on the strength of the decision being made and logged; failing it
// now would make Razorpay redeliver a payment that was handled correctly, and
// turn a storage outage into a duplicate-processing problem.
func (h *Handler) recordDecision(ctx context.Context, d DecisionRecord) {
	if h.decisions == nil {
		return
	}
	if err := h.decisions.RecordDecision(ctx, d); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

type receivedLog struct {
	Event       string            `json:"event"`
	PaymentID   string            `json:"payment_id"`
	Category    classify.Category `json:"category"`
	AmountPaise int               `json:"amount_paise"`
	ErrorCode   string            `json:"error_code"`
	ErrorReason string            `json:"error_reason"`

	// AttemptCount and TimeSinceFailureSeconds are the two inputs the decision
	// depends on that are not visible anywhere else in the line. Logged so a
	// decision can be re-read later against what the model was actually told.
	AttemptCount            int   `json:"attempt_count"`
	TimeSinceFailureSeconds int64 `json:"time_since_failure_seconds"`

	// Absent when the category was unknown (no call made) or the call failed.
	// Confidence is a pointer so a genuine 0.0 is still emitted rather than
	// silently dropped by omitempty.
	DecisionAction          string   `json:"decision_action,omitempty"`
	DecisionConfidence      *float64 `json:"decision_confidence,omitempty"`
	DecisionAlternateMethod string   `json:"decision_alternate_method,omitempty"`

	// DecisionSource distinguishes a decision the model made from one the
	// fallback rule supplied. Both carry an action and a confidence, and without
	// this they would be indistinguishable in the log.
	DecisionSource string `json:"decision_source,omitempty"`

	TS string `json:"ts"`
}

// fallbackDecisionLog records the resolution of a payment the decision layer
// could not answer for. It follows the decision_failed line rather than
// replacing it: that one carries the raw error, this one carries what was done
// about it.
type fallbackDecisionLog struct {
	Event         string            `json:"event"`
	PaymentID     string            `json:"payment_id"`
	Category      classify.Category `json:"category"`
	Source        string            `json:"source"`
	OriginalError string            `json:"original_error"`
	TS            string            `json:"ts"`
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

	// Source is always DecisionSourceStoppingRule. Carried anyway so that one
	// field answers "who decided this" across every line, rather than the
	// answer being encoded in which event name was used.
	Source string `json:"source"`

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
	// seen — not per category. Counting happens before the budget check so that
	// a stopped payment still records the attempt that tripped the rule.
	//
	// Increment is one atomic step and the check reads its return value, so two
	// concurrent deliveries of the same payment cannot both pass a budget with
	// one attempt left. A Get-then-Increment pair would read the same count in
	// both and let both through, which is why Get stays out of this path.
	// What the payment was, before how often it has been seen. Recording first
	// means the row is complete from the start, so Increment's placeholder
	// branch never fires in production and failed_payments never holds a row
	// that says only that something failed twice without saying what.
	//
	// A failure here is reported and swallowed: the attempt still has to be
	// counted, and losing the descriptive columns is a smaller harm than losing
	// the count that enforces the retry budget.
	if recorder, ok := h.attempts.(PaymentRecorder); ok {
		err := recorder.RecordPayment(r.Context(), PaymentDetails{
			PaymentID:     p.ID,
			Category:      category,
			ErrorCode:     p.ErrorCode,
			ErrorReason:   p.ErrorReason,
			ErrorSource:   p.ErrorSource,
			PaymentMethod: p.Method,
			AmountPaise:   int64(p.Amount),
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}

	// A store that can also report the first failure answers both from one
	// statement; one that cannot leaves the timestamp zero and the elapsed time
	// is reported as unknown rather than as zero seconds.
	var attemptCount int
	var firstFailedAt time.Time
	if tracker, ok := h.attempts.(FirstFailureTracker); ok {
		attemptCount, firstFailedAt = tracker.IncrementAndFirstFailure(p.ID)
	} else {
		attemptCount = h.attempts.Increment(p.ID)
	}

	// A category absent from the table budgets 0, which is the safe direction:
	// hard_decline and unknown both land here and both must stop.
	budget := retryBudgets[category]

	// The stopping rule runs before the decision layer, not after. Once the
	// budget is spent there is no automated action left to take on this payment,
	// so asking the model would spend a call on a question whose answer cannot
	// be used.
	if attemptCount > budget {
		logLine(stoppingRuleLog{
			Event:            "stopping_rule_triggered",
			PaymentID:        p.ID,
			Category:         category,
			AttemptCount:     attemptCount,
			Budget:           budget,
			EscalationReason: EscalationReasonBudgetExhausted,
			Source:           DecisionSourceStoppingRule,

			EscalationCustomerMessage: escalationMessage(category),

			TS: now(),
		})

		// Stored as escalate: the stopping rule takes no action beyond handing
		// the payment to a human, and the action column has to say what was
		// done. Confidence is nil — nothing was inferred, so there is no score.
		// Reasoning is empty for the same reason: none was produced.
		h.recordDecision(r.Context(), DecisionRecord{
			PaymentID:        p.ID,
			AttemptNumber:    attemptCount,
			Action:           decide.ActionEscalate,
			Source:           DecisionSourceStoppingRule,
			CustomerMessage:  escalationMessage(category),
			EscalationReason: EscalationReasonBudgetExhausted,
		})

		ok(w)
		return
	}

	received := receivedLog{
		Event:                   "payment_received",
		PaymentID:               p.ID,
		Category:                category,
		AmountPaise:             p.Amount,
		ErrorCode:               p.ErrorCode,
		ErrorReason:             p.ErrorReason,
		AttemptCount:            attemptCount,
		TimeSinceFailureSeconds: secondsSince(firstFailedAt),
		TS:                      now(),
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

			TimeSinceFailureSeconds: secondsSince(firstFailedAt),
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
			received.DecisionSource = DecisionSourceLLM

			// The model's own confidence, not the gate's verdict on it. The gate
			// changes the action; the score stays the evidence for why.
			modelConfidence := d.Confidence
			rec := DecisionRecord{
				PaymentID:       p.ID,
				AttemptNumber:   attemptCount,
				Action:          gated.Action,
				Source:          DecisionSourceLLM,
				Confidence:      &modelConfidence,
				Reasoning:       gated.Reasoning,
				CustomerMessage: gated.CustomerMessage,
				AlternateMethod: gated.AlternateMethod,
			}
			// Only an overridden decision has these. A decision the model made
			// and the gate left alone was never escalated and had no earlier
			// action, so both columns stay NULL.
			if overridden {
				rec.EscalationReason = EscalationReasonLowConfidence
				rec.OriginalAction = d.Action
			}
			h.recordDecision(r.Context(), rec)
		}

		// Both the original call and its retry failed to produce a usable
		// Decision. Leaving the payment here would mean no action and no message
		// for a customer whose payment failed — the model being unavailable is
		// not a reason to tell them nothing. See issue #1.
		if decideErr != nil {
			fb := fallbackDecision()
			confidence := fb.Confidence
			received.DecisionAction = fb.Action
			received.DecisionConfidence = &confidence
			received.DecisionAlternateMethod = fb.AlternateMethod
			received.DecisionSource = DecisionSourceFallbackRule

			// Confidence is deliberately nil here while the JSON log reports 0.
			// The log carries decision_source on the same line, so 0 cannot be
			// misread there; a column read on its own can be.
			h.recordDecision(r.Context(), DecisionRecord{
				PaymentID:       p.ID,
				AttemptNumber:   attemptCount,
				Action:          fb.Action,
				Source:          DecisionSourceFallbackRule,
				Reasoning:       fb.Reasoning,
				CustomerMessage: fb.CustomerMessage,
			})
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

	// A failed decision does not fail the webhook. Razorpay retries any non-2xx,
	// so returning an error because the model was unavailable would just produce
	// redeliveries of a payment we already recorded.
	//
	// Two lines, not one: decision_failed captures the raw error, and
	// fallback_decision_applied records the resolution. Keeping them separate
	// means a rising failure rate stays visible even while every payment is
	// still being resolved.
	if decideErr != nil {
		logLine(decisionFailedLog{
			Event:     "decision_failed",
			PaymentID: p.ID,
			Category:  category,
			Error:     decideErr.Error(),
			TS:        now(),
		})
		logLine(fallbackDecisionLog{
			Event:         "fallback_decision_applied",
			PaymentID:     p.ID,
			Category:      category,
			Source:        DecisionSourceFallbackRule,
			OriginalError: decideErr.Error(),
			TS:            now(),
		})
	}

	ok(w)
}

// secondsSince reports how long ago a payment first failed, for the model to
// weigh. A zero timestamp means the store could not say, and 0 is the honest
// answer: it is the same value the field carried before any store could
// answer, and it reads as "no elapsed time to take into account".
//
// A negative result is clamped. first_failed_at comes from the database's
// clock and time.Now() from this process's, and a few milliseconds of skew
// between them should not reach the model as a payment that fails in the
// future.
func secondsSince(firstFailedAt time.Time) int64 {
	if firstFailedAt.IsZero() {
		return 0
	}
	elapsed := int64(time.Since(firstFailedAt).Seconds())
	if elapsed < 0 {
		return 0
	}
	return elapsed
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
