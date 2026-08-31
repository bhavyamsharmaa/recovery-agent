// Package api serves read-only JSON endpoints over the recovery agent's
// tables, so a browser can see what the agent saw and decided.
//
// Every query here comes from internal/trace, which is the same code
// cmd/trace-payment prints as text. This package only reshapes those rows into
// JSON: no SQL is written twice, so the two views can never disagree about what
// a payment's history is.
//
// Every route here is behind the shared-secret gate in auth.go — reads and
// writes alike, with no exceptions — and the server refuses to start if
// API_ACCESS_KEY is unset. NewHandler itself does not apply that gate: main
// wraps this handler with NewAuth. The routes are registered on a mux owned by
// this handler, so a route added below is covered by construction rather than
// by remembering to check.
//
// KNOWN LIMITATION — one shared secret is not an operator session. It does not
// identify who is calling, cannot be revoked for one person without rotating it
// for everyone, and appears in full in any log that records request headers.
// It is the difference between "anyone who finds the port" and "anyone who
// holds the key", which is the gap worth closing first, not the last one. The
// CORS origin below still wants tightening from its localhost default before
// this is deployed.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/trace"
)

// queryTimeout bounds a single request's database work. Without it a stalled
// database would hold browser connections open indefinitely rather than
// answering with an error the caller can see.
const queryTimeout = 10 * time.Second

// defaultFrontendOrigin is Vite's dev server. FRONTEND_ORIGIN overrides it.
const defaultFrontendOrigin = "http://localhost:5173"

// Handler serves read-only JSON endpoints backed by the database.
type Handler struct {
	db     *sql.DB
	origin string
	mux    *http.ServeMux

	// batch serialises triggered batch runs. See batch.go: two at once would
	// interleave through one attempt counter and produce figures that describe
	// nothing reproducible.
	batch batchRunner

	// webhook is the real ingest handler, used by the /api/simulate/ routes to
	// dispatch a delivery in-process. Nil unless WithWebhook is called, and the
	// simulate routes report themselves unavailable rather than pretending when
	// it is — a demo endpoint that silently did nothing would be worse than one
	// that says it is not wired.
	webhook http.Handler
}

// WithWebhook attaches the ingest handler so the demo control panel can fire
// real deliveries through it.
//
// It is a separate call rather than a NewHandler parameter for the same reason
// the ingest package uses this shape: every existing caller and test keeps the
// constructor it already uses, and an API served without it is still a complete
// read API.
func (h *Handler) WithWebhook(webhook http.Handler) *Handler {
	h.webhook = webhook
	return h
}

// NewHandler builds the API handler and registers its routes.
//
// The routes live on a mux owned by this handler rather than on the server's,
// so mounting the API is one line in main and the CORS headers below cannot
// leak onto the webhook endpoint: nothing outside /api/ ever reaches this
// ServeHTTP.
func NewHandler(db *sql.DB) *Handler {
	origin := os.Getenv("FRONTEND_ORIGIN")
	if origin == "" {
		origin = defaultFrontendOrigin
	}

	h := &Handler{db: db, origin: origin, mux: http.NewServeMux()}
	h.mux.HandleFunc("GET /api/payments", h.listPayments)
	h.mux.HandleFunc("GET /api/payments/{payment_id}", h.getPayment)

	// The escalation queue: payments whose latest decision left them for a
	// person, with the reasoning that put them there.
	h.mux.HandleFunc("GET /api/escalations", h.listEscalations)

	// The batch-run routes. The first two are reads like everything above; the
	// POST is the one route on this surface that writes, and batch.go explains
	// what guards it.
	h.mux.HandleFunc("GET /api/batch-runs/latest", h.latestBatchRun)
	h.mux.HandleFunc("GET /api/batch-runs", h.listBatchRuns)
	h.mux.HandleFunc("POST /api/batch-runs", h.triggerBatchRun)

	// The demo control panel. All three write, and the last one can make the
	// decision layer fail — but only for its own request. See simulate.go.
	h.mux.HandleFunc("POST /api/simulate/failure", h.simulateFailure)
	h.mux.HandleFunc("POST /api/simulate/duplicate", h.simulateDuplicate)
	h.mux.HandleFunc("POST /api/simulate/llm-failure", h.simulateLLMFailure)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", h.origin)
	w.Header().Set("Vary", "Origin")

	// Preflight is answered here rather than by a route, because the mux only
	// knows about GET and would answer a browser's OPTIONS with a 405 that the
	// browser reports as an opaque CORS failure.
	if r.Method == http.MethodOptions {
		// POST is advertised because of the batch-run trigger. Without it a
		// browser's preflight for that one route fails and the button appears
		// broken for reasons nothing in the console explains.
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")

		// X-API-Key is advertised because auth.go requires it on every route
		// here. A browser will not send a header the preflight did not permit,
		// so omitting it would make every dashboard request fail the key check
		// while the header sat unsent — reported in the console as a CORS
		// error, which sends the reader looking at origins rather than at the
		// header list.
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	h.mux.ServeHTTP(w, r)
}

// paymentSummary is one row of GET /api/payments.
//
// The latest-decision fields are pointers so an undecided payment serialises
// them as null. Zero values would claim the payment was decided with an empty
// action at confidence 0, which is a different and false statement.
type paymentSummary struct {
	PaymentID     string   `json:"payment_id"`
	Category      string   `json:"category"`
	PaymentMethod string   `json:"payment_method"`
	AmountPaise   int64    `json:"amount_paise"`
	ErrorReason   string   `json:"error_reason"`
	FirstFailedAt jsonTime `json:"first_failed_at"`
	LastSeenAt    jsonTime `json:"last_seen_at"`
	AttemptCount  int      `json:"attempt_count"`

	LatestAction     *string  `json:"latest_action"`
	LatestConfidence *float64 `json:"latest_confidence"`
	LatestSource     *string  `json:"latest_source"`
}

// paymentDetail is GET /api/payments/{payment_id}: the same three sections
// cmd/trace-payment prints, as one object.
type paymentDetail struct {
	Payment   paymentRecord    `json:"payment"`
	Decisions []decisionRecord `json:"decisions"`
	Outcomes  []outcomeRecord  `json:"outcomes"`
}

type paymentRecord struct {
	PaymentID     string   `json:"payment_id"`
	Category      string   `json:"category"`
	ErrorCode     string   `json:"error_code"`
	ErrorReason   string   `json:"error_reason"`
	ErrorSource   string   `json:"error_source"`
	PaymentMethod string   `json:"payment_method"`
	AmountPaise   int64    `json:"amount_paise"`
	AttemptCount  int      `json:"attempt_count"`
	FirstFailedAt jsonTime `json:"first_failed_at"`
	LastSeenAt    jsonTime `json:"last_seen_at"`
}

type decisionRecord struct {
	ID               int64    `json:"id"`
	AttemptNumber    int      `json:"attempt_number"`
	Source           string   `json:"source"`
	Action           string   `json:"action"`
	Confidence       *float64 `json:"confidence"`
	Reasoning        *string  `json:"reasoning"`
	CustomerMessage  *string  `json:"customer_message"`
	AlternateMethod  *string  `json:"alternate_method"`
	EscalationReason *string  `json:"escalation_reason"`
	OriginalAction   *string  `json:"original_action"`
	CreatedAt        jsonTime `json:"created_at"`
}

type outcomeRecord struct {
	Outcome    string   `json:"outcome"`
	DecisionID *int64   `json:"decision_id"`
	RecordedAt jsonTime `json:"recorded_at"`
}

func (h *Handler) listPayments(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r)
	defer cancel()

	rows, err := trace.List(ctx, h.db, trace.ListFilter{
		Category: r.URL.Query().Get("category"),
		Action:   r.URL.Query().Get("action"),
	})
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, "could not read payments", err)
		return
	}

	// Built with a non-nil slice so an empty result encodes as [] and not null:
	// a caller iterating the response should not have to special-case "no rows".
	out := make([]paymentSummary, 0, len(rows))
	for _, s := range rows {
		out = append(out, paymentSummary{
			PaymentID:        s.PaymentID,
			Category:         s.Category,
			PaymentMethod:    s.PaymentMethod,
			AmountPaise:      s.AmountPaise,
			ErrorReason:      s.ErrorReason,
			FirstFailedAt:    jsonTime(s.FirstFailedAt),
			LastSeenAt:       jsonTime(s.LastSeenAt),
			AttemptCount:     s.AttemptCount,
			LatestAction:     nullString(s.LatestAction),
			LatestConfidence: nullFloat(s.LatestConfidence),
			LatestSource:     nullString(s.LatestSource),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) getPayment(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r)
	defer cancel()

	paymentID := r.PathValue("payment_id")
	full, err := trace.Load(ctx, h.db, paymentID)

	// A payment that was never ingested is a 404 with a body, not an empty 200.
	// An empty 200 would be indistinguishable from a payment that exists and
	// has no decisions yet, and the caller cannot tell a typo from a new
	// payment without that distinction.
	if errors.Is(err, trace.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":      "payment not found",
			"payment_id": paymentID,
		})
		return
	}
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, "could not read payment", err)
		return
	}

	detail := paymentDetail{
		Payment: paymentRecord{
			PaymentID:     full.Payment.PaymentID,
			Category:      full.Payment.Category,
			ErrorCode:     full.Payment.ErrorCode,
			ErrorReason:   full.Payment.ErrorReason,
			ErrorSource:   full.Payment.ErrorSource,
			PaymentMethod: full.Payment.PaymentMethod,
			AmountPaise:   full.Payment.AmountPaise,
			AttemptCount:  full.Payment.AttemptCount,
			FirstFailedAt: jsonTime(full.Payment.FirstFailedAt),
			LastSeenAt:    jsonTime(full.Payment.LastSeenAt),
		},
		Decisions: make([]decisionRecord, 0, len(full.Decisions)),
		Outcomes:  make([]outcomeRecord, 0, len(full.Outcomes)),
	}
	for _, d := range full.Decisions {
		detail.Decisions = append(detail.Decisions, decisionRecord{
			ID:               d.ID,
			AttemptNumber:    d.AttemptNumber,
			Source:           d.Source,
			Action:           d.Action,
			Confidence:       nullFloat(d.Confidence),
			Reasoning:        nullString(d.Reasoning),
			CustomerMessage:  nullString(d.CustomerMessage),
			AlternateMethod:  nullString(d.AlternateMethod),
			EscalationReason: nullString(d.EscalationReason),
			OriginalAction:   nullString(d.OriginalAction),
			CreatedAt:        jsonTime(d.CreatedAt),
		})
	}
	for _, o := range full.Outcomes {
		var decisionID *int64
		if o.DecisionID.Valid {
			id := o.DecisionID.Int64
			decisionID = &id
		}
		detail.Outcomes = append(detail.Outcomes, outcomeRecord{
			Outcome:    o.Outcome,
			DecisionID: decisionID,
			RecordedAt: jsonTime(o.RecordedAt),
		})
	}
	writeJSON(w, http.StatusOK, detail)
}

// fail logs the real error and tells the caller only the summary. The database
// error text can name columns and connection strings, which is not something to
// hand to a browser even on a read-only endpoint.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, status int, summary string, err error) {
	fmt.Fprintf(os.Stderr, "{\"event\":\"api_error\",\"path\":%q,\"error\":%q}\n", r.URL.Path, err.Error())
	writeJSON(w, status, map[string]string{"error": summary})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	// The status and headers are already sent by here, so a marshalling failure
	// cannot be turned into a 500 — it is logged instead, and the caller sees a
	// truncated body, which is the honest signal that something went wrong.
	if err := json.NewEncoder(w).Encode(body); err != nil {
		fmt.Fprintf(os.Stderr, "{\"event\":\"api_encode_failed\",\"error\":%q}\n", err.Error())
	}
}

// contextWithTimeout derives the query context from the request's, so a browser
// that navigates away cancels the query it started instead of leaving it to run
// against the database with nobody waiting for the answer.
func contextWithTimeout(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), queryTimeout)
}

// jsonTime renders a timestamp as RFC 3339 in UTC.
//
// time.Time's own marshalling keeps whatever offset the driver returned, so the
// same instant could serialise as +05:30 in one deployment and Z in another.
// Every timestamp in this API is UTC with a Z, so a consumer can compare two of
// them as strings.
type jsonTime time.Time

func (t jsonTime) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Time(t).UTC().Format(time.RFC3339))
}

// UnmarshalJSON exists so the type round-trips. Nothing in the server decodes a
// response — these endpoints are read-only — but a type that can only be
// written is one a test cannot check the shape of without redeclaring it, and a
// second declaration of the response shape is a second thing to keep in step.
func (t *jsonTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return err
	}
	*t = jsonTime(parsed)
	return nil
}

func nullString(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	v := s.String
	return &v
}

func nullFloat(f sql.NullFloat64) *float64 {
	if !f.Valid {
		return nil
	}
	v := f.Float64
	return &v
}
