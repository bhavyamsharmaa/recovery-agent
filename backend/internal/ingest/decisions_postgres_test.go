package ingest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/db"
	"github.com/bhavyamsharmaa/recovery-agent/internal/decide"
)

// decisionRow mirrors the nullable shape of the decisions table, so a test can
// tell NULL apart from an empty string or a zero. That distinction is the point
// of most of what follows.
type decisionRow struct {
	attemptNumber    int
	action           string
	source           string
	confidence       sql.NullFloat64
	reasoning        sql.NullString
	customerMessage  sql.NullString
	alternateMethod  sql.NullString
	escalationReason sql.NullString
	originalAction   sql.NullString
}

// newDecisionTestHandler builds a handler wired to the real database, with
// whichever decider the case needs, and returns a payment id unique to the run.
//
// Live only, like the other Postgres tests here: the behaviour under test is
// what reaches the table, which a fake database cannot demonstrate.
func newDecisionTestHandler(t *testing.T, decider Decider) (*Handler, *sql.DB, string) {
	t.Helper()

	if os.Getenv("RECOVERY_LIVE_TESTS") != "1" {
		t.Skip("set RECOVERY_LIVE_TESTS=1 to run against the real database")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	paymentID := fmt.Sprintf("pay_dec_%s_%d", t.Name(), time.Now().UnixNano())

	t.Cleanup(func() {
		// Children first: decisions references failed_payments.
		if _, err := pool.Exec(`DELETE FROM decisions WHERE payment_id = $1`, paymentID); err != nil {
			t.Errorf("cleanup decisions: %v", err)
		}
		if _, err := pool.Exec(`DELETE FROM failed_payments WHERE payment_id = $1`, paymentID); err != nil {
			t.Errorf("cleanup failed_payments: %v", err)
		}
		pool.Close()
	})

	h := NewHandler(decider, NewPostgresAttemptStore(pool)).
		WithDecisionRecorder(NewPostgresDecisionStore(pool)).
		WithVerifier(testVerifier())

	return h, pool, paymentID
}

func readDecisions(t *testing.T, pool *sql.DB, paymentID string) []decisionRow {
	t.Helper()

	rows, err := pool.Query(`
		SELECT attempt_number, action, source, confidence, reasoning,
		       customer_message, alternate_method, escalation_reason, original_action
		FROM decisions WHERE payment_id = $1 ORDER BY id`, paymentID)
	if err != nil {
		t.Fatalf("read decisions: %v", err)
	}
	defer rows.Close()

	var out []decisionRow
	for rows.Next() {
		var d decisionRow
		if err := rows.Scan(&d.attemptNumber, &d.action, &d.source, &d.confidence,
			&d.reasoning, &d.customerMessage, &d.alternateMethod,
			&d.escalationReason, &d.originalAction); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func exactlyOne(t *testing.T, rows []decisionRow) decisionRow {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("got %d decision rows, want exactly 1: %+v", len(rows), rows)
	}
	return rows[0]
}

// TestDecisionPersistedFromLLM is the ordinary path: the model answered and the
// gate left it alone.
func TestDecisionPersistedFromLLM(t *testing.T) {
	h, pool, paymentID := newDecisionTestHandler(t, &stubDecider{decision: decide.Decision{
		Action:          decide.ActionRetryDelayed,
		Confidence:      0.78,
		Reasoning:       "First failure, one retry remains.",
		CustomerMessage: "We will automatically retry your payment shortly.",
	}})

	fire(t, h, webhookBody(paymentID, "insufficient_funds"))

	got := exactlyOne(t, readDecisions(t, pool, paymentID))
	if got.source != DecisionSourceLLM {
		t.Errorf("source = %q, want %q", got.source, DecisionSourceLLM)
	}
	if got.action != decide.ActionRetryDelayed {
		t.Errorf("action = %q, want %q", got.action, decide.ActionRetryDelayed)
	}
	if !got.confidence.Valid || got.confidence.Float64 != 0.78 {
		t.Errorf("confidence = %+v, want a stored 0.78", got.confidence)
	}
	if got.attemptNumber != 1 {
		t.Errorf("attempt_number = %d, want 1", got.attemptNumber)
	}
	// A decision the gate never touched was not escalated and had no earlier
	// action, so both columns must be NULL rather than empty strings.
	if got.escalationReason.Valid {
		t.Errorf("escalation_reason = %q, want NULL", got.escalationReason.String)
	}
	if got.originalAction.Valid {
		t.Errorf("original_action = %q, want NULL", got.originalAction.String)
	}
	if got.alternateMethod.Valid {
		t.Errorf("alternate_method = %q, want NULL on a non-suggest action", got.alternateMethod.String)
	}
}

// TestDecisionPersistedFromLLMOverride covers the gate firing: the stored
// action is what was done, and the model's own score and original choice
// survive as the evidence for why.
func TestDecisionPersistedFromLLMOverride(t *testing.T) {
	h, pool, paymentID := newDecisionTestHandler(t, &stubDecider{decision: decide.Decision{
		Action:          decide.ActionRetryNow,
		Confidence:      0.68,
		Reasoning:       "Funds may have cleared.",
		CustomerMessage: "We will automatically retry your payment shortly.",
	}})

	fire(t, h, webhookBody(paymentID, "insufficient_funds"))

	got := exactlyOne(t, readDecisions(t, pool, paymentID))
	if got.source != DecisionSourceLLM {
		t.Errorf("source = %q, want %q", got.source, DecisionSourceLLM)
	}
	if got.action != decide.ActionEscalate {
		t.Errorf("action = %q, want the post-gate %q", got.action, decide.ActionEscalate)
	}
	if !got.confidence.Valid || got.confidence.Float64 != 0.68 {
		t.Errorf("confidence = %+v, want the model's own 0.68", got.confidence)
	}
	if got.escalationReason.String != EscalationReasonLowConfidence {
		t.Errorf("escalation_reason = %+v, want %q", got.escalationReason, EscalationReasonLowConfidence)
	}
	if got.originalAction.String != decide.ActionRetryNow {
		t.Errorf("original_action = %+v, want %q", got.originalAction, decide.ActionRetryNow)
	}
}

// TestDecisionPersistedFromStoppingRule is the path that never reaches the
// model at all — Day 3 built it to log to stdout and nothing else.
func TestDecisionPersistedFromStoppingRule(t *testing.T) {
	decider := &stubDecider{decision: decide.Decision{Action: decide.ActionRetryNow, Confidence: 0.9}}
	h, pool, paymentID := newDecisionTestHandler(t, decider)

	// hard_decline budgets 0, so the first delivery trips the stopping rule.
	fire(t, h, webhookBody(paymentID, "card_declined"))

	got := exactlyOne(t, readDecisions(t, pool, paymentID))
	if got.source != DecisionSourceStoppingRule {
		t.Errorf("source = %q, want %q", got.source, DecisionSourceStoppingRule)
	}
	if got.action != decide.ActionEscalate {
		t.Errorf("action = %q, want %q", got.action, decide.ActionEscalate)
	}
	// The central claim of this row: no model ran, so there is no score. NULL,
	// not 0, which a reader of this column alone would take for a real one.
	if got.confidence.Valid {
		t.Errorf("confidence = %v, want NULL — no model produced this decision", got.confidence.Float64)
	}
	if got.escalationReason.String != EscalationReasonBudgetExhausted {
		t.Errorf("escalation_reason = %+v, want %q", got.escalationReason, EscalationReasonBudgetExhausted)
	}
	if got.customerMessage.String != escalationMessage("hard_decline") {
		t.Errorf("customer_message = %q, want the hard_decline escalation text", got.customerMessage.String)
	}
	if decider.calls != 0 {
		t.Errorf("decider was called %d times; the stopping rule must not consult the model", decider.calls)
	}
}

// TestDecisionPersistedFromFallbackRule is Day 4's path: the model failed twice
// and a static decision stood in for it.
func TestDecisionPersistedFromFallbackRule(t *testing.T) {
	h, pool, paymentID := newDecisionTestHandler(t,
		&failingDecider{err: errors.New("decide: unmarshal decision: format")})

	fire(t, h, webhookBody(paymentID, "insufficient_funds"))

	got := exactlyOne(t, readDecisions(t, pool, paymentID))
	if got.source != DecisionSourceFallbackRule {
		t.Errorf("source = %q, want %q", got.source, DecisionSourceFallbackRule)
	}
	if got.action != decide.ActionNoRetry {
		t.Errorf("action = %q, want %q", got.action, decide.ActionNoRetry)
	}
	// The JSON log reports this decision as confidence 0 by design. The column
	// must not: 0 there is indistinguishable from a real low score.
	if got.confidence.Valid {
		t.Errorf("confidence = %v, want NULL — the log's 0 must not reach the column", got.confidence.Float64)
	}
	if got.reasoning.String != fallbackDecision().Reasoning {
		t.Errorf("reasoning = %q, want the fallback reasoning", got.reasoning.String)
	}
	if got.escalationReason.Valid {
		t.Errorf("escalation_reason = %q, want NULL — a fallback is not an escalation", got.escalationReason.String)
	}
}

// TestDecisionsAccumulatePerAttempt checks the table records a history rather
// than a current state: two deliveries of one payment leave two rows, numbered.
func TestDecisionsAccumulatePerAttempt(t *testing.T) {
	h, pool, paymentID := newDecisionTestHandler(t, &stubDecider{decision: decide.Decision{
		Action:          decide.ActionRetryNow,
		Confidence:      0.90,
		Reasoning:       "Customer-fixable, budget remains.",
		CustomerMessage: "Please check your details and try again.",
	}})

	// soft_decline budgets 2, so both deliveries reach the model.
	body := webhookBody(paymentID, "authentication_failed")
	fireEvent(t, h, "evt_dec_1", body)
	fireEvent(t, h, "evt_dec_2", body)

	rows := readDecisions(t, pool, paymentID)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 — one per attempt: %+v", len(rows), rows)
	}
	for i, r := range rows {
		if want := i + 1; r.attemptNumber != want {
			t.Errorf("row %d attempt_number = %d, want %d", i, r.attemptNumber, want)
		}
	}
}
