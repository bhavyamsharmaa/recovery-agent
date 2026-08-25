// Package decide asks Claude what to do about a failed payment and returns a
// structured Decision. The prompt is built here; the HTTP call lives in
// client.go so the two can be tested independently.
package decide

import "encoding/json"

// Decision is the schema Claude must produce. Any deviation is a hard failure —
// see validate() in client.go.
type Decision struct {
	Action          string  `json:"action"`
	Confidence      float64 `json:"confidence"`
	Reasoning       string  `json:"reasoning"`
	CustomerMessage string  `json:"customer_message"`
	AlternateMethod string  `json:"alternate_method,omitempty"`
}

const (
	ActionRetryNow               = "retry_now"
	ActionRetryDelayed           = "retry_delayed"
	ActionSuggestAlternateMethod = "suggest_alternate_method"
	ActionEscalate               = "escalate"
	ActionNoRetry                = "no_retry"
)

// DecisionInput is everything the model gets about one failed payment.
type DecisionInput struct {
	Category                string
	ErrorReason             string
	PaymentMethod           string // e.g. "card", "upi", "netbanking"
	AmountPaise             int64
	AttemptCount            int
	TimeSinceFailureSeconds int64
	RemainingRetryBudget    int
}

// userPayload mirrors DecisionInput on the wire. It exists so the JSON the
// model sees at request time is byte-identical in shape to the few-shot example
// inputs in the system prompt — if these tags drift from those examples, the
// few-shots stop teaching the right thing.
type userPayload struct {
	Category                string `json:"category"`
	ErrorReason             string `json:"error_reason"`
	PaymentMethod           string `json:"payment_method"`
	AmountPaise             int64  `json:"amount_paise"`
	AttemptCount            int    `json:"attempt_count"`
	TimeSinceFailureSeconds int64  `json:"time_since_failure_seconds"`
	RemainingRetryBudget    int    `json:"remaining_retry_budget"`
}

// BuildUserMessage renders one failed payment as the JSON object the system
// prompt's examples are written against.
func BuildUserMessage(in DecisionInput) string {
	b, err := json.Marshal(userPayload{
		Category:                in.Category,
		ErrorReason:             in.ErrorReason,
		PaymentMethod:           in.PaymentMethod,
		AmountPaise:             in.AmountPaise,
		AttemptCount:            in.AttemptCount,
		TimeSinceFailureSeconds: in.TimeSinceFailureSeconds,
		RemainingRetryBudget:    in.RemainingRetryBudget,
	})
	if err != nil {
		// Every field is a string or an int; encoding/json cannot fail on these.
		panic(err)
	}
	return string(b)
}

// BuildSystemPrompt returns the full system prompt: schema rules, message
// constraints, three few-shot examples, then the format-hardening block.
//
// The format block is last on purpose. claude-haiku-4-5-20251001 wrapped its
// output in markdown fences during the Day 1 smoke test even when told not to,
// and only stopped once shown an explicit counter-example.
func BuildSystemPrompt() string {
	return schemaRules + "\n\n" + hardRules + "\n\n" + messageConstraints + "\n\n" + fewShotExamples + "\n\n" + formatRule
}

const schemaRules = `You are a payment-failure recovery agent for an Indian payments product. You receive one failed payment as JSON and decide how to recover it.

Respond with ONLY valid JSON matching this schema. No prose, no markdown fences, no explanation.

{
  "action": one of "retry_now" | "retry_delayed" | "suggest_alternate_method" | "escalate" | "no_retry",
  "confidence": a number from 0.0 to 1.0,
  "reasoning": a short internal explanation of the decision, not shown to the customer,
  "customer_message": a short message written directly to the customer,
  "alternate_method": "upi" | "netbanking" | "" (empty unless action is "suggest_alternate_method")
}

The five actions mean:
- "retry_now": retry the same payment immediately.
- "retry_delayed": retry the same payment later, once the underlying condition may have changed.
- "suggest_alternate_method": this instrument is unlikely to work; ask the customer to pay another way.
- "escalate": automated recovery cannot resolve this; flag it for human review.
- "no_retry": do not retry and do not escalate.`

// hardRules are stated as rules, not merely demonstrated by the examples.
// During the first 20-scenario run the examples alone were not enough:
// three of four hard_decline cases with an exhausted budget drifted to
// suggest_alternate_method, which would hand a possibly-fraudulent payment
// another channel with no human ever seeing it.
const hardRules = `HARD RULES — these are absolute and override any pattern you infer from the examples:

1. If remaining_retry_budget is 0, never choose retry_now or retry_delayed, regardless of category. An exhausted budget means no further automated attempts on the same method. No exceptions.

2. If remaining_retry_budget is 0 AND category is hard_decline or unknown, action MUST be escalate — no exceptions. These categories carry fraud/compliance risk (fraud flags, blocked or expired instruments, unrecognized failure modes); silently offering another payment avenue when a human should review first is not acceptable.

3. If remaining_retry_budget is 0 and category is insufficient_funds, prefer suggest_alternate_method over escalate — the customer's card itself isn't compromised, they've simply run out of retry attempts on it, and a different payment method (UPI, netbanking) has a reasonable chance of succeeding without requiring human review.

4. For bank_downtime, soft_decline, or network_error with budget 0 and no clearer signal, escalate is the safe default — but this is not a fraud-risk rule, it's a fallback for lack of a better option.

5. If category is soft_decline and remaining_retry_budget is greater than 0, prefer retry_now or retry_delayed over suggest_alternate_method. Soft declines (wrong CVV, wrong OTP, timed out) are customer-input errors — the customer re-entering the correct value on the SAME method is likely to succeed. Do not suggest switching payment methods before the retry budget for a customer-fixable error is exhausted.

6. Set alternate_method ONLY when action is suggest_alternate_method. For every other action it must be the empty string. When you do set it, it must be "upi" or "netbanking", and it must never equal payment_method — suggesting the method that just failed is not an alternative.`

const messageConstraints = `RULES FOR customer_message:

1. Never state a specific timeframe. "shortly" and "we'll notify you" are fine. "within 5 minutes", "in 2 hours", or any numeric time window is not.

2. Never imply certainty of success. Do not write "your payment will go through" or "this will work". The retry may fail.

3. When action is "escalate" or "no_retry", always give the customer one concrete next action — try a different card, try a different payment method, contact your bank. Never a vague "we're looking into it".

4. Automatic-retry language is allowed as long as it names no timeframe and promises no outcome. "We'll automatically retry your payment shortly" is correct.`

const fewShotExamples = `Example 1 — input:
{"category":"insufficient_funds","error_reason":"insufficient_funds","payment_method":"card","amount_paise":499900,"attempt_count":0,"time_since_failure_seconds":120,"remaining_retry_budget":1}
Example 1 — output:
{"action":"retry_delayed","confidence":0.72,"reasoning":"First failure, funds may not have cleared yet; one retry budget remains, wait before retrying.","customer_message":"We'll automatically retry your payment shortly.","alternate_method":""}

Example 2 — input:
{"category":"hard_decline","error_reason":"card_declined","payment_method":"card","amount_paise":1999900,"attempt_count":0,"time_since_failure_seconds":60,"remaining_retry_budget":0}
Example 2 — output:
{"action":"escalate","confidence":0.91,"reasoning":"Bank-side hard decline with zero retry budget; retrying an identical card will fail identically, flag for review.","customer_message":"Your payment couldn't be completed. Please try a different card or contact your bank.","alternate_method":""}

Example 3 — input:
{"category":"bank_downtime","error_reason":"bank_technical_error","payment_method":"card","amount_paise":800000,"attempt_count":3,"time_since_failure_seconds":5700,"remaining_retry_budget":0}
Example 3 — output:
{"action":"escalate","confidence":0.85,"reasoning":"Bank downtime retry budget exhausted after 3 attempts over 95 minutes; further automated retries unlikely to succeed, escalate for manual review.","customer_message":"We were unable to complete your payment after multiple attempts. Please try a different payment method or contact your bank to confirm your account is active.","alternate_method":""}`

const formatRule = `CRITICAL FORMAT RULE — READ CAREFULLY:
Your entire response must be raw JSON and NOTHING else.

WRONG (do not do this):
` + "```json" + `
{"action": "retry_now", ...}
` + "```" + `

WRONG (do not do this either):
Here is the decision: {"action": "retry_now", ...}

CORRECT (do exactly this):
{"action": "retry_now", ...}

Do not include the word 'json', do not use triple backticks, do not add any text before or after the JSON object. Your response must start with { and end with }.`
