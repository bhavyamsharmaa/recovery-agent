package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bhavyamsharmaa/recovery-agent/internal/decide"
)

// stubDecider returns a fixed Decision, so a test can pin the confidence the
// gate sees without an API key or a live call.
type stubDecider struct {
	decision decide.Decision
}

func (s stubDecider) Decide(context.Context, decide.DecisionInput) (decide.Decision, decide.Outcome, error) {
	return s.decision, decide.Outcome{}, nil
}

// webhookBody renders a minimal payment.failed payload. error_reason drives
// classification; insufficient_funds gives a budget of 1, so a single delivery
// clears the stopping rule and reaches the decision layer.
func webhookBody(paymentID, errorReason string) string {
	return `{"event":"payment.failed","payload":{"payment":{"entity":{` +
		`"id":"` + paymentID + `","amount":499900,"currency":"INR","method":"card",` +
		`"error_code":"BAD_REQUEST_ERROR","error_reason":"` + errorReason + `",` +
		`"error_source":"customer","error_step":"payment_authorization"}}}}`
}

// fire sends one webhook through the handler and returns the log lines it
// emitted, decoded as generic maps so a test can assert on exact JSON keys.
func fire(t *testing.T, h *Handler, body string) []map[string]any {
	t.Helper()

	var buf bytes.Buffer
	saved := logOut
	logOut = &buf
	defer func() { logOut = saved }()

	req := httptest.NewRequest(http.MethodPost, "/webhook/payment-failed", strings.NewReader(body))
	req.Header.Set(eventIDHeader, "evt_test_"+t.Name())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", l, err)
		}
		lines = append(lines, m)
	}
	return lines
}

func findLine(t *testing.T, lines []map[string]any, event string) map[string]any {
	t.Helper()
	for _, m := range lines {
		if m["event"] == event {
			return m
		}
	}
	t.Fatalf("no %q line among %v", event, lines)
	return nil
}

// TestConfidenceGateOverridesLowConfidence is the core case: the model answers
// confidently enough to parse but not confidently enough to act on.
func TestConfidenceGateOverridesLowConfidence(t *testing.T) {
	h := NewHandler(stubDecider{decision: decide.Decision{
		Action:          decide.ActionRetryNow,
		Confidence:      0.70,
		Reasoning:       "Funds may have cleared since the first attempt.",
		CustomerMessage: "We'll automatically retry your payment shortly.",
	}}, NewInMemoryAttemptStore())

	lines := fire(t, h, webhookBody("pay_lowconf", "insufficient_funds"))

	received := findLine(t, lines, "payment_received")
	if got := received["decision_action"]; got != decide.ActionEscalate {
		t.Errorf("payment_received decision_action = %v, want %q (the post-override value)", got, decide.ActionEscalate)
	}
	// The gate changes what we do, not what the model reported about itself.
	if got := received["decision_confidence"]; got != 0.70 {
		t.Errorf("payment_received decision_confidence = %v, want 0.70 untouched", got)
	}

	override := findLine(t, lines, "confidence_override")
	for field, want := range map[string]any{
		"payment_id":        "pay_lowconf",
		"category":          "insufficient_funds",
		"original_action":   decide.ActionRetryNow,
		"overridden_action": decide.ActionEscalate,
		"confidence":        0.70,
		"escalation_reason": EscalationReasonLowConfidence,
	} {
		if got := override[field]; got != want {
			t.Errorf("confidence_override %s = %v, want %v", field, got, want)
		}
	}
}

// TestConfidenceGateLeavesConfidentDecisionAlone guards the other direction:
// a gate that fires on everything would be indistinguishable from one that
// works, since escalate is a plausible answer for most inputs.
func TestConfidenceGateLeavesConfidentDecisionAlone(t *testing.T) {
	h := NewHandler(stubDecider{decision: decide.Decision{
		Action:          decide.ActionRetryDelayed,
		Confidence:      0.75, // exactly at the threshold: acted on, not overridden
		Reasoning:       "First failure, one retry remains.",
		CustomerMessage: "We'll automatically retry your payment shortly.",
	}}, NewInMemoryAttemptStore())

	lines := fire(t, h, webhookBody("pay_confident", "insufficient_funds"))

	received := findLine(t, lines, "payment_received")
	if got := received["decision_action"]; got != decide.ActionRetryDelayed {
		t.Errorf("decision_action = %v, want %q left alone", got, decide.ActionRetryDelayed)
	}
	for _, m := range lines {
		if m["event"] == "confidence_override" {
			t.Errorf("confidence 0.75 was overridden; threshold is a floor, not an exclusive bound")
		}
	}
}

// TestConfidenceGateAndStoppingRuleAreDistinct pins the separation the two
// mechanisms are supposed to have: an exhausted budget stops before the model
// is consulted at all, so it produces no decision and no override, even though
// both paths end in escalation.
func TestConfidenceGateAndStoppingRuleAreDistinct(t *testing.T) {
	h := NewHandler(stubDecider{decision: decide.Decision{
		Action:     decide.ActionRetryNow,
		Confidence: 0.70,
	}}, NewInMemoryAttemptStore())

	// hard_decline budgets 0, so the very first delivery trips the stopping rule.
	lines := fire(t, h, webhookBody("pay_exhausted", "card_declined"))

	stop := findLine(t, lines, "stopping_rule_triggered")
	if got := stop["escalation_reason"]; got != EscalationReasonBudgetExhausted {
		t.Errorf("escalation_reason = %v, want %v", got, EscalationReasonBudgetExhausted)
	}
	for _, m := range lines {
		if m["event"] == "confidence_override" {
			t.Errorf("stopping rule produced a confidence_override; the model should never have been called")
		}
		if m["event"] == "payment_received" {
			t.Errorf("stopping rule produced a payment_received line; it returns before that")
		}
	}
}

// TestConfidenceGateClearsAlternateMethod covers the one field the gate does
// not carry over. suggest_alternate_method is the only action for which
// alternate_method is meaningful, so overriding the action to escalate while
// leaving "upi" in place would produce a Decision that decide.validate() would
// have rejected — a live payment suggestion attached to a case that means
// "needs human review". The value survives in the log rather than the Decision.
func TestConfidenceGateClearsAlternateMethod(t *testing.T) {
	h := NewHandler(stubDecider{decision: decide.Decision{
		Action:          decide.ActionSuggestAlternateMethod,
		Confidence:      0.68,
		AlternateMethod: "upi",
		Reasoning:       "Card has exhausted its retries; another method may work.",
		CustomerMessage: "Please try paying with UPI instead.",
	}}, NewInMemoryAttemptStore())

	lines := fire(t, h, webhookBody("pay_altcleared", "insufficient_funds"))

	received := findLine(t, lines, "payment_received")
	if got := received["decision_action"]; got != decide.ActionEscalate {
		t.Errorf("decision_action = %v, want %q", got, decide.ActionEscalate)
	}
	// omitempty means a cleared value is absent from the line entirely; either
	// absent or explicitly empty is correct, "upi" is not.
	if got, ok := received["decision_alternate_method"]; ok && got != "" {
		t.Errorf("decision_alternate_method = %v, want cleared", got)
	}

	override := findLine(t, lines, "confidence_override")
	if got := override["original_action"]; got != decide.ActionSuggestAlternateMethod {
		t.Errorf("original_action = %v, want %q", got, decide.ActionSuggestAlternateMethod)
	}
	if got := override["original_alternate_method"]; got != "upi" {
		t.Errorf("original_alternate_method = %v, want \"upi\" — the cleared value must stay auditable", got)
	}
	if got := override["confidence"]; got != 0.68 {
		t.Errorf("confidence = %v, want 0.68 untouched", got)
	}
}
