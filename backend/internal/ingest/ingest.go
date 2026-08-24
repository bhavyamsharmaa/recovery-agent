// Package ingest receives Razorpay payment.failed webhooks and drops
// redeliveries. It does not classify or act on failures — that is the
// classifier's job, wired in separately.
package ingest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/classify"
)

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
}

func NewHandler() *Handler { return &Handler{} }

type receivedLog struct {
	Event       string            `json:"event"`
	PaymentID   string            `json:"payment_id"`
	Category    classify.Category `json:"category"`
	AmountPaise int               `json:"amount_paise"`
	ErrorCode   string            `json:"error_code"`
	ErrorReason string            `json:"error_reason"`
	TS          string            `json:"ts"`
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

	logLine(receivedLog{
		Event:       "payment_received",
		PaymentID:   p.ID,
		Category:    classify.Classify(p.ErrorReason, p.ErrorSource),
		AmountPaise: p.Amount,
		ErrorCode:   p.ErrorCode,
		ErrorReason: p.ErrorReason,
		TS:          now(),
	})
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
