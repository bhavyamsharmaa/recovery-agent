package api

import (
	"net/http"

	"github.com/bhavyamsharmaa/recovery-agent/internal/trace"
)

// The escalation queue: payments whose latest decision stopped automated
// handling and left them for a person.
//
// The reasoning and the customer message are sent inline rather than behind a
// second request. The queue exists to answer "why does a human need to look at
// this", and an answer that needs another round trip is one a reviewer scanning
// a list will not read.

// escalationJSON is one row of GET /api/escalations.
type escalationJSON struct {
	PaymentID     string   `json:"payment_id"`
	Category      string   `json:"category"`
	PaymentMethod string   `json:"payment_method"`
	AmountPaise   int64    `json:"amount_paise"`
	ErrorReason   string   `json:"error_reason"`
	AttemptCount  int      `json:"attempt_count"`
	FirstFailedAt jsonTime `json:"first_failed_at"`
	LastSeenAt    jsonTime `json:"last_seen_at"`

	DecisionID    int64    `json:"decision_id"`
	AttemptNumber int      `json:"attempt_number"`
	Action        string   `json:"action"`
	Source        string   `json:"source"`
	DecidedAt     jsonTime `json:"decided_at"`

	// Confidence is null for stopping-rule and fallback decisions, as everywhere
	// else — a rule-made decision had no model behind it, and 0 would claim the
	// model was certain it was wrong.
	Confidence *float64 `json:"confidence"`

	// EscalationReason is null for a genuine fallback_rule case. That is not a
	// missing value: the fallback fires when the system could not reason at all,
	// so there is no policy reason to name. The UI shows that as its own third
	// category rather than as a blank.
	EscalationReason *string `json:"escalation_reason"`

	// OriginalAction is set when the confidence gate overrode the model, naming
	// what it replaced.
	OriginalAction *string `json:"original_action"`

	Reasoning       *string `json:"reasoning"`
	CustomerMessage *string `json:"customer_message"`
}

// listEscalations serves GET /api/escalations.
func (h *Handler) listEscalations(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r)
	defer cancel()

	rows, err := trace.Escalations(ctx, h.db)
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, "could not read the escalation queue", err)
		return
	}

	// Non-nil slice so an empty queue encodes as [] and not null. An empty
	// escalation queue is a good state, and a caller iterating it should not
	// have to special-case it.
	out := make([]escalationJSON, 0, len(rows))
	for _, e := range rows {
		out = append(out, escalationJSON{
			PaymentID:     e.PaymentID,
			Category:      e.Category,
			PaymentMethod: e.PaymentMethod,
			AmountPaise:   e.AmountPaise,
			ErrorReason:   e.ErrorReason,
			AttemptCount:  e.AttemptCount,
			FirstFailedAt: jsonTime(e.FirstFailedAt),
			LastSeenAt:    jsonTime(e.LastSeenAt),

			DecisionID:    e.DecisionID,
			AttemptNumber: e.AttemptNumber,
			Action:        e.Action,
			Source:        e.Source,
			DecidedAt:     jsonTime(e.DecidedAt),

			Confidence:       nullFloat(e.Confidence),
			EscalationReason: nullString(e.EscalationReason),
			OriginalAction:   nullString(e.OriginalAction),
			Reasoning:        nullString(e.Reasoning),
			CustomerMessage:  nullString(e.CustomerMessage),
		})
	}
	writeJSON(w, http.StatusOK, out)
}
