// Command decide-harness runs a fixed set of scenarios through the real Claude
// API and prints every raw response for manual review.
//
// This is a manual diagnostic tool, not a test. It calls the live API and costs
// money. It never halts on failure and always exits 0 — the point is visibility
// into failure patterns across the whole set, which a fast-fail would hide.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/decide"
)

const delayBetweenCalls = 250 * time.Millisecond

var scenarios = []decide.DecisionInput{
	{Category: "insufficient_funds", ErrorReason: "insufficient_funds", AmountPaise: 49900, AttemptCount: 0, TimeSinceFailureSeconds: 120, RemainingRetryBudget: 1},
	{Category: "insufficient_funds", ErrorReason: "insufficient_funds", AmountPaise: 4500000, AttemptCount: 0, TimeSinceFailureSeconds: 300, RemainingRetryBudget: 1},
	{Category: "insufficient_funds", ErrorReason: "insufficient_funds", AmountPaise: 99900, AttemptCount: 1, TimeSinceFailureSeconds: 1200, RemainingRetryBudget: 0},
	{Category: "insufficient_funds", ErrorReason: "insufficient_funds", AmountPaise: 1500000, AttemptCount: 1, TimeSinceFailureSeconds: 3600, RemainingRetryBudget: 0},
	{Category: "bank_downtime", ErrorReason: "bank_technical_error", AmountPaise: 299900, AttemptCount: 0, TimeSinceFailureSeconds: 60, RemainingRetryBudget: 3},
	{Category: "bank_downtime", ErrorReason: "bank_technical_error", AmountPaise: 1200000, AttemptCount: 1, TimeSinceFailureSeconds: 2100, RemainingRetryBudget: 2},
	{Category: "bank_downtime", ErrorReason: "bank_technical_error", AmountPaise: 350000, AttemptCount: 2, TimeSinceFailureSeconds: 3900, RemainingRetryBudget: 1},
	{Category: "bank_downtime", ErrorReason: "bank_technical_error", AmountPaise: 800000, AttemptCount: 3, TimeSinceFailureSeconds: 5700, RemainingRetryBudget: 0},
	{Category: "hard_decline", ErrorReason: "card_declined", AmountPaise: 199900, AttemptCount: 0, TimeSinceFailureSeconds: 60, RemainingRetryBudget: 0},
	{Category: "hard_decline", ErrorReason: "payment_risk_check_failed", AmountPaise: 6000000, AttemptCount: 0, TimeSinceFailureSeconds: 120, RemainingRetryBudget: 0},
	{Category: "hard_decline", ErrorReason: "card_expired", AmountPaise: 79900, AttemptCount: 0, TimeSinceFailureSeconds: 180, RemainingRetryBudget: 0},
	{Category: "hard_decline", ErrorReason: "debit_instrument_blocked", AmountPaise: 2500000, AttemptCount: 0, TimeSinceFailureSeconds: 60, RemainingRetryBudget: 0},
	{Category: "soft_decline", ErrorReason: "authentication_failed", AmountPaise: 349900, AttemptCount: 0, TimeSinceFailureSeconds: 30, RemainingRetryBudget: 2},
	{Category: "soft_decline", ErrorReason: "incorrect_cvv", AmountPaise: 99900, AttemptCount: 1, TimeSinceFailureSeconds: 300, RemainingRetryBudget: 1},
	{Category: "soft_decline", ErrorReason: "payment_timed_out", AmountPaise: 750000, AttemptCount: 2, TimeSinceFailureSeconds: 480, RemainingRetryBudget: 0},
	{Category: "network_error", ErrorReason: "gateway_technical_error", AmountPaise: 249900, AttemptCount: 0, TimeSinceFailureSeconds: 60, RemainingRetryBudget: 3},
	{Category: "network_error", ErrorReason: "gateway_technical_error", AmountPaise: 1800000, AttemptCount: 1, TimeSinceFailureSeconds: 360, RemainingRetryBudget: 2},
	{Category: "network_error", ErrorReason: "gateway_technical_error", AmountPaise: 499900, AttemptCount: 2, TimeSinceFailureSeconds: 660, RemainingRetryBudget: 1},
	{Category: "network_error", ErrorReason: "gateway_technical_error", AmountPaise: 99900, AttemptCount: 3, TimeSinceFailureSeconds: 960, RemainingRetryBudget: 0},
	{Category: "insufficient_funds", ErrorReason: "insufficient_funds", AmountPaise: 9999900, AttemptCount: 0, TimeSinceFailureSeconds: 600, RemainingRetryBudget: 1},
}

type failure struct {
	scenario int
	reason   string
}

func main() {
	client, err := decide.NewClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	ctx := context.Background()
	var failures []failure

	for i, in := range scenarios {
		n := i + 1
		if i > 0 {
			time.Sleep(delayBetweenCalls)
		}

		fmt.Printf("========== SCENARIO %d/%d ==========\n", n, len(scenarios))
		fmt.Printf("input: %s\n", decide.BuildUserMessage(in))

		d, raw, err := client.Decide(ctx, in)

		// Printed on every path. A rejected response is only diagnosable if you
		// can see exactly what the model said.
		fmt.Printf("raw response:\n%s\n", raw)

		if err != nil {
			fmt.Printf("FAIL: %v\n\n", err)
			failures = append(failures, failure{scenario: n, reason: describeFailure(err, raw)})
			continue
		}

		fmt.Println("parsed decision:")
		fmt.Printf("  action:           %s\n", d.Action)
		fmt.Printf("  confidence:       %v\n", d.Confidence)
		fmt.Printf("  reasoning:        %s\n", d.Reasoning)
		fmt.Printf("  customer_message: %s\n", d.CustomerMessage)
		fmt.Printf("  alternate_method: %q\n", d.AlternateMethod)
		fmt.Println()
	}

	passed := len(scenarios) - len(failures)
	fmt.Println("========== SUMMARY ==========")
	fmt.Printf("%d/%d scenarios produced valid, schema-conformant decisions.\n", passed, len(scenarios))

	if len(failures) == 0 {
		fmt.Println("No failures.")
		return
	}
	fmt.Println("Failures:")
	for _, f := range failures {
		fmt.Printf("  scenario %d: %s\n", f.scenario, f.reason)
	}
}

// describeFailure turns an error plus the raw text into one reviewable line.
// The specific shapes are called out because they need different fixes: a fence
// is a prompt problem, an out-of-range confidence is a judgement problem.
func describeFailure(err error, raw string) string {
	trimmed := strings.TrimSpace(raw)

	switch {
	case strings.HasPrefix(trimmed, "```"):
		return "response wrapped in markdown fence"
	case strings.Contains(err.Error(), "out of range"):
		return "confidence outside [0.0, 1.0]"
	case strings.Contains(err.Error(), "invalid action"):
		return "action not one of the five allowed values"
	case strings.Contains(err.Error(), "unmarshal decision") && trimmed != "" && !strings.HasPrefix(trimmed, "{"):
		return "prose before the JSON object"
	case strings.Contains(err.Error(), "unmarshal decision"):
		return "response was not parseable as the Decision schema"
	case strings.Contains(err.Error(), "http "):
		return "API returned a non-200 status"
	default:
		return err.Error()
	}
}
