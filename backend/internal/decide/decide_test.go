package decide

import (
	"strings"
	"testing"
)

// The few-shot outputs are the part of the prompt doing the teaching. If one is
// dropped or reworded, the model's behaviour changes silently — so each is
// asserted verbatim rather than by a loose keyword.
func TestBuildSystemPromptContainsFewShotExamples(t *testing.T) {
	prompt := BuildSystemPrompt()

	examples := []struct {
		name string
		text string
	}{
		{
			name: "example 1 input",
			text: `{"category":"insufficient_funds","error_reason":"insufficient_funds","payment_method":"card","amount_paise":499900,"attempt_count":0,"time_since_failure_seconds":120,"remaining_retry_budget":1}`,
		},
		{
			name: "example 1 output",
			text: `{"action":"retry_delayed","confidence":0.72,"reasoning":"First failure, funds may not have cleared yet; one retry budget remains, wait before retrying.","customer_message":"We'll automatically retry your payment shortly.","alternate_method":""}`,
		},
		{
			name: "example 2 input",
			text: `{"category":"hard_decline","error_reason":"card_declined","payment_method":"card","amount_paise":1999900,"attempt_count":0,"time_since_failure_seconds":60,"remaining_retry_budget":0}`,
		},
		{
			name: "example 2 output",
			text: `{"action":"escalate","confidence":0.91,"reasoning":"Bank-side hard decline with zero retry budget; retrying an identical card will fail identically, flag for review.","customer_message":"Your payment couldn't be completed. Please try a different card or contact your bank.","alternate_method":""}`,
		},
		{
			name: "example 3 input",
			text: `{"category":"bank_downtime","error_reason":"bank_technical_error","payment_method":"card","amount_paise":800000,"attempt_count":3,"time_since_failure_seconds":5700,"remaining_retry_budget":0}`,
		},
		{
			name: "example 3 output",
			text: `{"action":"escalate","confidence":0.85,"reasoning":"Bank downtime retry budget exhausted after 3 attempts over 95 minutes; further automated retries unlikely to succeed, escalate for manual review.","customer_message":"We were unable to complete your payment after multiple attempts. Please try a different payment method or contact your bank to confirm your account is active.","alternate_method":""}`,
		},
	}

	for _, ex := range examples {
		if !strings.Contains(prompt, ex.text) {
			t.Errorf("system prompt is missing %s", ex.name)
		}
	}
}

func TestBuildSystemPromptContainsFormatRule(t *testing.T) {
	prompt := BuildSystemPrompt()

	fragments := []string{
		"CRITICAL FORMAT RULE — READ CAREFULLY:",
		"Your entire response must be raw JSON and NOTHING else.",
		"WRONG (do not do this):",
		"```json",
		"Here is the decision:",
		"CORRECT (do exactly this):",
		"Your response must start with { and end with }.",
	}

	for _, f := range fragments {
		if !strings.Contains(prompt, f) {
			t.Errorf("format rule block is missing %q", f)
		}
	}
}

// The counter-example only works if it is the last thing the model reads.
func TestFormatRuleIsLastSection(t *testing.T) {
	prompt := BuildSystemPrompt()

	rule := strings.Index(prompt, "CRITICAL FORMAT RULE")
	if rule == -1 {
		t.Fatal("format rule block not found")
	}
	if last := strings.LastIndex(prompt, "Example 3 — output:"); last > rule {
		t.Error("few-shot examples appear after the format rule; the rule must come last")
	}
	if !strings.HasSuffix(prompt, "Your response must start with { and end with }.") {
		t.Error("format rule is not at the very end of the prompt")
	}
}

func TestBuildSystemPromptContainsAllFiveActions(t *testing.T) {
	prompt := BuildSystemPrompt()

	for _, a := range []string{
		ActionRetryNow,
		ActionRetryDelayed,
		ActionSuggestAlternateMethod,
		ActionEscalate,
		ActionNoRetry,
	} {
		if !strings.Contains(prompt, a) {
			t.Errorf("system prompt does not mention action %q", a)
		}
	}
}

func TestBuildSystemPromptContainsMessageConstraints(t *testing.T) {
	prompt := BuildSystemPrompt()

	fragments := []string{
		"Never state a specific timeframe",
		"Never imply certainty of success",
		"one concrete next action",
		"We'll automatically retry your payment shortly",
	}

	for _, f := range fragments {
		if !strings.Contains(prompt, f) {
			t.Errorf("message constraints are missing %q", f)
		}
	}
}

// The hard rules exist because the examples alone did not hold — assert each is
// actually present, not merely implied.
func TestBuildSystemPromptContainsHardRules(t *testing.T) {
	prompt := BuildSystemPrompt()

	fragments := []string{
		"If remaining_retry_budget is 0, never choose retry_now or retry_delayed",
		"action MUST be escalate. Never choose suggest_alternate_method for these two categories",
		"These categories carry no fraud risk",
		"No exceptions.",
		"prefer retry_now or retry_delayed over suggest_alternate_method",
		"customer re-entering the correct value on the SAME method",
		"Set alternate_method ONLY when action is suggest_alternate_method",
		"it must never equal payment_method",
	}

	for _, f := range fragments {
		if !strings.Contains(prompt, f) {
			t.Errorf("hard rules are missing %q", f)
		}
	}
}

// The rules must be stated before the examples, so an example never reads as
// the authority on a case a rule covers.
func TestHardRulesPrecedeExamples(t *testing.T) {
	prompt := BuildSystemPrompt()

	rules := strings.Index(prompt, "HARD RULES")
	first := strings.Index(prompt, "Example 1 — input:")
	if rules == -1 || first == -1 {
		t.Fatal("expected both a hard rules block and few-shot examples")
	}
	if rules > first {
		t.Error("hard rules appear after the examples; they must come first")
	}
}

func TestBuildUserMessage(t *testing.T) {
	got := BuildUserMessage(DecisionInput{
		Category:                "insufficient_funds",
		ErrorReason:             "insufficient_funds",
		PaymentMethod:           "card",
		AmountPaise:             499900,
		AttemptCount:            0,
		TimeSinceFailureSeconds: 120,
		RemainingRetryBudget:    1,
	})

	want := `{"category":"insufficient_funds","error_reason":"insufficient_funds","payment_method":"card","amount_paise":499900,"attempt_count":0,"time_since_failure_seconds":120,"remaining_retry_budget":1}`
	if got != want {
		t.Errorf("BuildUserMessage()\n got: %s\nwant: %s", got, want)
	}
}

// A rendered input must be shaped exactly like the few-shot example inputs, or
// the examples stop being examples of the thing the model is actually asked.
func TestBuildUserMessageMatchesFewShotInputShape(t *testing.T) {
	got := BuildUserMessage(DecisionInput{
		Category:                "hard_decline",
		ErrorReason:             "card_declined",
		PaymentMethod:           "card",
		AmountPaise:             1999900,
		AttemptCount:            0,
		TimeSinceFailureSeconds: 60,
		RemainingRetryBudget:    0,
	})

	if !strings.Contains(BuildSystemPrompt(), got) {
		t.Errorf("rendered input does not match the example 2 input in the prompt:\n%s", got)
	}
}
