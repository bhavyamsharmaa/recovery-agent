package ingest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bhavyamsharmaa/recovery-agent/internal/decide"
)

// failingDecider stands in for the decision layer having exhausted its own
// internal retry: Decide returns an error, which is what the handler sees when
// both the original call and its retry failed to produce a usable Decision.
type failingDecider struct {
	err   error
	calls int
}

func (f *failingDecider) Decide(context.Context, decide.DecisionInput) (decide.Decision, decide.Outcome, error) {
	f.calls++
	return decide.Decision{}, decide.Outcome{Retried: true}, f.err
}

// TestFallbackAppliedOnDoubleDecisionFailure is issue #1: a payment the model
// could not answer for used to end with a logged error and nothing else — no
// action, no message, nothing a customer would ever see.
func TestFallbackAppliedOnDoubleDecisionFailure(t *testing.T) {
	decideErr := errors.New("decide: unmarshal decision: invalid character '`': format")
	decider := &failingDecider{err: decideErr}
	h := NewHandler(decider, NewInMemoryAttemptStore())

	lines := fire(t, h, webhookBody("pay_double_failure", "insufficient_funds"))

	// The raw error is still reported. The fallback resolves the payment; it does
	// not paper over the fact that the decision layer failed.
	failed := findLine(t, lines, "decision_failed")
	if got := failed["error"]; got != decideErr.Error() {
		t.Errorf("decision_failed error = %v, want %v", got, decideErr.Error())
	}

	applied := findLine(t, lines, "fallback_decision_applied")
	for field, want := range map[string]any{
		"payment_id":     "pay_double_failure",
		"category":       "insufficient_funds",
		"source":         DecisionSourceFallbackRule,
		"original_error": decideErr.Error(),
	} {
		if got := applied[field]; got != want {
			t.Errorf("fallback_decision_applied %s = %v, want %v", field, got, want)
		}
	}

	// The payment is resolved in the received line too, not only in a separate
	// event — anything reading decisions per payment has to see one here.
	received := findLine(t, lines, "payment_received")
	if got := received["decision_action"]; got != decide.ActionNoRetry {
		t.Errorf("decision_action = %v, want %q", got, decide.ActionNoRetry)
	}
	if got := received["decision_confidence"]; got != float64(0) {
		t.Errorf("decision_confidence = %v, want 0 — a fallback carries no model score", got)
	}
	if got := received["decision_source"]; got != DecisionSourceFallbackRule {
		t.Errorf("decision_source = %v, want %q", got, DecisionSourceFallbackRule)
	}

	// The failure was already retried inside the decision layer; the fallback
	// must not add a third call.
	if decider.calls != 1 {
		t.Errorf("decider called %d times, want 1 — the fallback must not retry", decider.calls)
	}
}

// TestFallbackStillAnswers200 keeps the webhook contract: Razorpay retries any
// non-2xx, so failing the request because the model was unavailable would turn
// one unanswerable payment into a stream of redeliveries.
func TestFallbackStillAnswers200(t *testing.T) {
	h := NewHandler(&failingDecider{err: errors.New("boom")}, NewInMemoryAttemptStore())

	req := httptest.NewRequest(http.MethodPost, "/webhook/payment-failed",
		strings.NewReader(webhookBody("pay_status_check", "insufficient_funds")))
	req.Header.Set(eventIDHeader, "evt_status_check")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestFallbackDecisionValues pins the static Decision itself. It is the text a
// customer actually receives when the system cannot reason about their payment,
// so it is held to the same rules as every other customer-facing message.
func TestFallbackDecisionValues(t *testing.T) {
	fb := fallbackDecision()

	if fb.Action != decide.ActionNoRetry {
		t.Errorf("action = %q, want %q", fb.Action, decide.ActionNoRetry)
	}
	if fb.Confidence != 0 {
		t.Errorf("confidence = %v, want 0", fb.Confidence)
	}
	if fb.AlternateMethod != "" {
		t.Errorf("alternate_method = %q, want empty", fb.AlternateMethod)
	}
	if fb.CustomerMessage == "" || fb.Reasoning == "" {
		t.Error("fallback must carry both a customer message and a reasoning")
	}

	// Distinct from the category-specific stopping-rule text: this failure is a
	// system outage, not a policy decision about a category of failure.
	for c, m := range escalationMessages {
		if fb.CustomerMessage == m {
			t.Errorf("fallback message is identical to the %s stopping-rule message", c)
		}
	}
	if fb.CustomerMessage == escalationMessageFallback {
		t.Error("fallback message is identical to the stopping-rule fallback message")
	}

	// Same constraints the model's own customer_message follows.
	lower := strings.ToLower(fb.CustomerMessage)
	if strings.ContainsAny(fb.CustomerMessage, "0123456789") {
		t.Errorf("message contains a digit, which would be a timeframe: %q", fb.CustomerMessage)
	}
	for _, f := range []string{"will go through", "will work", "will succeed", "guarantee"} {
		if strings.Contains(lower, f) {
			t.Errorf("message implies success: %q contains %q", fb.CustomerMessage, f)
		}
	}
	if !strings.Contains(lower, "please") {
		t.Errorf("message gives no next action: %q", fb.CustomerMessage)
	}

	// A no_retry with a dangling alternate_method would be rejected anywhere else
	// in the system; the fallback must not be the one place that produces it.
	if fb.Action != decide.ActionSuggestAlternateMethod && fb.AlternateMethod != "" {
		t.Error("alternate_method set on a non-suggest action")
	}
}

// TestNormalDecisionIsTaggedLLM is the other half of unambiguous provenance: a
// real model decision must say so, or "no source" becomes the tell for the LLM
// path and the field is worthless.
func TestNormalDecisionIsTaggedLLM(t *testing.T) {
	h := NewHandler(&stubDecider{decision: decide.Decision{
		Action:          decide.ActionRetryDelayed,
		Confidence:      0.90,
		Reasoning:       "First failure, one retry remains.",
		CustomerMessage: "We'll automatically retry your payment shortly.",
	}}, NewInMemoryAttemptStore())

	lines := fire(t, h, webhookBody("pay_llm_source", "insufficient_funds"))

	received := findLine(t, lines, "payment_received")
	if got := received["decision_source"]; got != DecisionSourceLLM {
		t.Errorf("decision_source = %v, want %q", got, DecisionSourceLLM)
	}
	for _, m := range lines {
		if m["event"] == "fallback_decision_applied" {
			t.Error("a successful decision produced a fallback line")
		}
	}
}

// TestStoppingRuleIsTaggedStoppingRule completes the set, so every resolved
// payment carries exactly one of the three sources.
func TestStoppingRuleIsTaggedStoppingRule(t *testing.T) {
	h := NewHandler(&stubDecider{}, NewInMemoryAttemptStore())

	lines := fire(t, h, webhookBody("pay_stop_source", "card_declined"))

	stop := findLine(t, lines, "stopping_rule_triggered")
	if got := stop["source"]; got != DecisionSourceStoppingRule {
		t.Errorf("source = %v, want %q", got, DecisionSourceStoppingRule)
	}
}
