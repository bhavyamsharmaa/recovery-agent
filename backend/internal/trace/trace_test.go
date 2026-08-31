package trace

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/db"
)

// Escalations() is the query behind the whole escalation queue view. Until now
// it was the only part of that page with no test at all.
//
// Live only: what is under test is which decision the SQL considers "latest" and
// how it treats NULLs, neither of which a fake demonstrates.

func liveDB(t *testing.T) *sql.DB {
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
	t.Cleanup(func() { pool.Close() })
	return pool
}

// seedDecision is one decision to attach to a seeded payment. A nil confidence
// or reason means the column is NULL, which is the distinction most of these
// tests turn on.
type seedDecision struct {
	attemptNumber int
	action        string
	source        string
	confidence    *float64
	reason        *string
	reasoning     *string
}

// seedPayment inserts a payment and its decisions, and registers cleanup.
func seedPayment(t *testing.T, pool *sql.DB, suffix string, decisions []seedDecision) string {
	t.Helper()

	paymentID := fmt.Sprintf("pay_trace_%s_%d", suffix, time.Now().UnixNano())
	now := time.Now().UTC()

	t.Cleanup(func() {
		// Children first: the schema deliberately has no ON DELETE CASCADE.
		if _, err := pool.Exec(`DELETE FROM outcomes WHERE payment_id = $1`, paymentID); err != nil {
			t.Errorf("cleanup outcomes: %v", err)
		}
		if _, err := pool.Exec(`DELETE FROM decisions WHERE payment_id = $1`, paymentID); err != nil {
			t.Errorf("cleanup decisions: %v", err)
		}
		if _, err := pool.Exec(`DELETE FROM failed_payments WHERE payment_id = $1`, paymentID); err != nil {
			t.Errorf("cleanup failed_payments: %v", err)
		}
	})

	if _, err := pool.Exec(`
		INSERT INTO failed_payments (payment_id, category, error_code, error_reason,
		    error_source, payment_method, amount_paise, first_failed_at, last_seen_at,
		    attempt_count)
		VALUES ($1, 'insufficient_funds', 'TEST', 'insufficient_funds', 'customer',
		        'card', 123400, $2, $2, $3)`,
		paymentID, now, len(decisions)); err != nil {
		t.Fatalf("seed payment: %v", err)
	}

	for _, d := range decisions {
		if _, err := pool.Exec(`
			INSERT INTO decisions (payment_id, attempt_number, action, source,
			    confidence, escalation_reason, reasoning, customer_message)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'told the customer something')`,
			paymentID, d.attemptNumber, d.action, d.source, d.confidence, d.reason, d.reasoning); err != nil {
			t.Fatalf("seed decision: %v", err)
		}
	}
	return paymentID
}

func find(rows []Escalation, paymentID string) *Escalation {
	for i := range rows {
		if rows[i].PaymentID == paymentID {
			return &rows[i]
		}
	}
	return nil
}

func f64(v float64) *float64 { return &v }
func str(v string) *string   { return &v }

// TestEscalationsIncludesBothStoppingActions: escalate and no_retry both end
// automated handling, and a queue showing only escalate would hide exactly the
// cases where the agent knew least.
func TestEscalationsIncludesBothStoppingActions(t *testing.T) {
	pool := liveDB(t)

	escalated := seedPayment(t, pool, "escalate", []seedDecision{
		{attemptNumber: 1, action: "escalate", source: "stopping_rule", reason: str("retry_budget_exhausted")},
	})
	noRetry := seedPayment(t, pool, "noretry", []seedDecision{
		{attemptNumber: 1, action: "no_retry", source: "fallback_rule",
			reasoning: str("LLM decision layer failed after retry")},
	})

	rows, err := Escalations(context.Background(), pool)
	if err != nil {
		t.Fatalf("Escalations: %v", err)
	}

	if find(rows, escalated) == nil {
		t.Error("an escalate case is missing from the queue")
	}
	if find(rows, noRetry) == nil {
		t.Error("a no_retry case is missing from the queue")
	}
}

// TestEscalationsExcludesRetryingPayments is the other half: a payment the agent
// is still working on must not appear in a queue of things waiting on a person.
func TestEscalationsExcludesRetryingPayments(t *testing.T) {
	pool := liveDB(t)

	retrying := seedPayment(t, pool, "retry", []seedDecision{
		{attemptNumber: 1, action: "retry_now", source: "llm", confidence: f64(0.85)},
	})

	rows, err := Escalations(context.Background(), pool)
	if err != nil {
		t.Fatalf("Escalations: %v", err)
	}
	if find(rows, retrying) != nil {
		t.Error("a retry_now payment appeared in the escalation queue")
	}

	// And every returned row really is a stopping action.
	for _, e := range rows {
		if e.Action != "escalate" && e.Action != "no_retry" {
			t.Errorf("payment %s has action %q", e.PaymentID, e.Action)
		}
	}
}

// TestEscalationsUsesTheLatestDecisionOnly is the case the whole DISTINCT ON
// exists for. A payment that escalated and was then retried is no longer
// waiting on a person; one that was retried and then escalated is.
func TestEscalationsUsesTheLatestDecisionOnly(t *testing.T) {
	pool := liveDB(t)

	// Escalated first, then retried. Latest is retry_now, so it must NOT appear.
	resolved := seedPayment(t, pool, "resolved", []seedDecision{
		{attemptNumber: 1, action: "escalate", source: "stopping_rule", reason: str("retry_budget_exhausted")},
		{attemptNumber: 2, action: "retry_now", source: "llm", confidence: f64(0.88)},
	})

	// Retried first, then escalated. Latest is escalate, so it MUST appear.
	stopped := seedPayment(t, pool, "stopped", []seedDecision{
		{attemptNumber: 1, action: "retry_now", source: "llm", confidence: f64(0.82)},
		{attemptNumber: 2, action: "escalate", source: "stopping_rule", reason: str("retry_budget_exhausted")},
	})

	rows, err := Escalations(context.Background(), pool)
	if err != nil {
		t.Fatalf("Escalations: %v", err)
	}

	if find(rows, resolved) != nil {
		t.Error("a payment whose LATEST decision is retry_now appeared in the queue; " +
			"the query is matching any decision rather than the most recent one")
	}
	e := find(rows, stopped)
	if e == nil {
		t.Fatal("a payment whose latest decision is escalate is missing from the queue")
	}
	if e.AttemptNumber != 2 {
		t.Errorf("attempt_number = %d, want 2 — the row must carry the latest decision", e.AttemptNumber)
	}
	if e.Action != "escalate" {
		t.Errorf("action = %q, want escalate", e.Action)
	}
}

// TestEscalationsAppearsOncePerPayment guards against the join multiplying rows:
// a payment with several decisions must contribute exactly one queue entry.
func TestEscalationsAppearsOncePerPayment(t *testing.T) {
	pool := liveDB(t)

	id := seedPayment(t, pool, "multi", []seedDecision{
		{attemptNumber: 1, action: "retry_now", source: "llm", confidence: f64(0.8)},
		{attemptNumber: 2, action: "retry_delayed", source: "llm", confidence: f64(0.79)},
		{attemptNumber: 3, action: "escalate", source: "stopping_rule", reason: str("retry_budget_exhausted")},
	})

	rows, err := Escalations(context.Background(), pool)
	if err != nil {
		t.Fatalf("Escalations: %v", err)
	}

	count := 0
	for _, e := range rows {
		if e.PaymentID == id {
			count++
		}
	}
	if count != 1 {
		t.Errorf("payment with 3 decisions appears %d times in the queue, want 1", count)
	}
}

// TestEscalationsPreservesNulls is the null-versus-zero rule at the query layer.
func TestEscalationsPreservesNulls(t *testing.T) {
	pool := liveDB(t)

	// A fallback case: no escalation_reason, no confidence, but it does carry
	// reasoning.
	fallback := seedPayment(t, pool, "fallback", []seedDecision{
		{attemptNumber: 1, action: "no_retry", source: "fallback_rule",
			reasoning: str("LLM decision layer failed after retry")},
	})
	// A stopping-rule case: a reason, but no confidence and no reasoning.
	rule := seedPayment(t, pool, "rule", []seedDecision{
		{attemptNumber: 1, action: "escalate", source: "stopping_rule",
			reason: str("retry_budget_exhausted")},
	})
	// A confidence-gate case: everything present, plus an override.
	gate := seedPayment(t, pool, "gate", []seedDecision{
		{attemptNumber: 1, action: "escalate", source: "llm",
			confidence: f64(0.68), reason: str("low_confidence"),
			reasoning: str("Insufficient funds on first attempt")},
	})

	rows, err := Escalations(context.Background(), pool)
	if err != nil {
		t.Fatalf("Escalations: %v", err)
	}

	fb := find(rows, fallback)
	if fb == nil {
		t.Fatal("fallback case missing")
	}
	if fb.EscalationReason.Valid {
		t.Errorf("fallback escalation_reason = %q, want NULL — the system could not reason at all, "+
			"so there is no policy reason to name", fb.EscalationReason.String)
	}
	if fb.Confidence.Valid {
		t.Error("fallback confidence is non-NULL; no model stood behind it")
	}
	if !fb.Reasoning.Valid {
		t.Error("fallback reasoning is NULL; the fallback does record why it fired")
	}

	r := find(rows, rule)
	if r == nil {
		t.Fatal("stopping-rule case missing")
	}
	if r.Confidence.Valid {
		t.Error("stopping-rule confidence is non-NULL; the rule fired before the model was asked")
	}
	if !r.EscalationReason.Valid || r.EscalationReason.String != "retry_budget_exhausted" {
		t.Errorf("stopping-rule reason = %v, want retry_budget_exhausted", r.EscalationReason)
	}
	if r.Reasoning.Valid {
		t.Error("stopping-rule reasoning is non-NULL; there is no model reasoning to record")
	}

	g := find(rows, gate)
	if g == nil {
		t.Fatal("confidence-gate case missing")
	}
	if !g.Confidence.Valid || g.Confidence.Float64 != 0.68 {
		t.Errorf("gate confidence = %v, want 0.68", g.Confidence)
	}
	if !g.Reasoning.Valid {
		t.Error("gate reasoning is NULL; the model did reason before being overridden")
	}
}

// TestEscalationsOrdersMostRecentFirst: this is a work queue, and what a
// reviewer wants at the top is the case that most recently started needing them.
func TestEscalationsOrdersMostRecentFirst(t *testing.T) {
	pool := liveDB(t)

	seedPayment(t, pool, "ordera", []seedDecision{
		{attemptNumber: 1, action: "escalate", source: "stopping_rule", reason: str("retry_budget_exhausted")},
	})
	time.Sleep(10 * time.Millisecond)
	newest := seedPayment(t, pool, "orderb", []seedDecision{
		{attemptNumber: 1, action: "escalate", source: "stopping_rule", reason: str("retry_budget_exhausted")},
	})

	rows, err := Escalations(context.Background(), pool)
	if err != nil {
		t.Fatalf("Escalations: %v", err)
	}
	if len(rows) < 2 {
		t.Fatal("not enough rows to check ordering")
	}

	for i := 1; i < len(rows); i++ {
		if rows[i-1].DecidedAt.Before(rows[i].DecidedAt) {
			t.Fatalf("queue is not ordered most recent first at index %d: %s before %s",
				i, rows[i-1].DecidedAt, rows[i].DecidedAt)
		}
	}

	// The most recently decided of the two seeded must outrank the other.
	var newestPos, olderPos = -1, -1
	for i, e := range rows {
		if e.PaymentID == newest {
			newestPos = i
		}
		if e.PaymentID != newest && olderPos == -1 && e.Category == "insufficient_funds" {
			olderPos = i
		}
	}
	if newestPos == -1 {
		t.Error("the newest seeded escalation is missing from the queue")
	}
}

// TestEscalationsCarriesThePaymentColumns: the queue shows category, amount and
// attempt count beside each case, so the embedded Payment must be populated.
func TestEscalationsCarriesThePaymentColumns(t *testing.T) {
	pool := liveDB(t)

	id := seedPayment(t, pool, "columns", []seedDecision{
		{attemptNumber: 1, action: "escalate", source: "stopping_rule", reason: str("retry_budget_exhausted")},
	})

	rows, err := Escalations(context.Background(), pool)
	if err != nil {
		t.Fatalf("Escalations: %v", err)
	}
	e := find(rows, id)
	if e == nil {
		t.Fatal("seeded escalation missing")
	}

	if e.Category != "insufficient_funds" {
		t.Errorf("category = %q", e.Category)
	}
	if e.AmountPaise != 123400 {
		t.Errorf("amount_paise = %d, want 123400", e.AmountPaise)
	}
	if e.PaymentMethod != "card" {
		t.Errorf("payment_method = %q, want card", e.PaymentMethod)
	}
	if e.DecisionID == 0 {
		t.Error("decision_id is zero; an outcome could not be attributed to a decision")
	}
	if e.FirstFailedAt.IsZero() || e.LastSeenAt.IsZero() {
		t.Error("timestamps are zero")
	}
}
