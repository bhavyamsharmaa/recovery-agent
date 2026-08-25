// Package ingest receives Razorpay payment.failed webhooks, drops redeliveries,
// classifies the failure, and asks the decision layer what to do about it.
package ingest

import (
	"context"
	"encoding/json"
	"fmt"
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

// Decider is the decision layer as this package needs it. An interface rather
// than a concrete client so the handler can be tested without an API key.
type Decider interface {
	Decide(ctx context.Context, in decide.DecisionInput) (decide.Decision, string, error)
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

// Handler serves POST /webhook/payment-failed.
type Handler struct {
	// seen holds every payment id processed so far. In-memory only: a restart
	// forgets everything and redeliveries would be reprocessed. Day 5 replaces
	// this with the database.
	seen sync.Map

	decider Decider
}

func NewHandler(d Decider) *Handler { return &Handler{decider: d} }

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

type duplicateLog struct {
	Event     string `json:"event"`
	PaymentID string `json:"payment_id"`
	TS        string `json:"ts"`
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

	// The idempotency key is the payment id, not e.Event — that field holds the
	// event *type* ("payment.failed") and is identical across every webhook.
	p := e.Payload.Payment.Entity
	if p.ID == "" {
		fmt.Fprintln(os.Stderr, "webhook parsed but payload.payment.entity.id was empty")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// LoadOrStore is one atomic step. A Load-then-Store pair would let two
	// concurrent redeliveries of the same payment both read "not seen" and both
	// be treated as new, which is the exact failure this check exists to prevent.
	if _, loaded := h.seen.LoadOrStore(p.ID, struct{}{}); loaded {
		logLine(duplicateLog{
			Event:     "duplicate",
			PaymentID: p.ID,
			TS:        now(),
		})
		ok(w)
		return
	}

	category := classify.Classify(p.ErrorReason, p.ErrorSource)

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
	if category != classify.CategoryUnknown {
		ctx, cancel := context.WithTimeout(r.Context(), decideTimeout)
		d, _, err := h.decider.Decide(ctx, decide.DecisionInput{
			Category:      string(category),
			ErrorReason:   p.ErrorReason,
			PaymentMethod: p.Method,
			AmountPaise:   int64(p.Amount),

			// TODO(day5): replace with real values once persistence exists.
			AttemptCount:            0,
			TimeSinceFailureSeconds: 0,

			RemainingRetryBudget: retryBudgets[category],
		})
		cancel()

		if err != nil {
			decideErr = err
		} else {
			confidence := d.Confidence
			received.DecisionAction = d.Action
			received.DecisionConfidence = &confidence
			received.DecisionAlternateMethod = d.AlternateMethod
		}
	}

	logLine(received)

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

// ok answers 200 for new events and duplicates alike. Razorpay retries any
// non-2xx, so returning an error for a duplicate would produce more duplicates.
func ok(w http.ResponseWriter) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, `{"status":"ok"}`)
}

func logLine(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not marshal log line: %v\n", err)
		return
	}
	fmt.Println(string(b))
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }
