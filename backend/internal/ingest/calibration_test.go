package ingest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/decide"
)

// ambiguousScenarios are inputs measured against the live API on 2026-08-26 and
// found to score below confidenceThreshold. Both are insufficient_funds with one
// attempt spent and one left, on a large amount that has gone stale — the case
// where retrying may be premature and escalating may be unnecessary, and the
// model's own confidence says so.
//
// The confidences are recorded observations, not guesses, and they are not
// stable: the same input re-measured returns 0.68 most of the time and 0.78 or
// exactly 0.75 otherwise. observedBelow records how often each scenario landed
// under the threshold across repeated live calls, which is the honest form of
// this claim — the gate fires on these inputs usually, not always.
//
// This matters more than it looks. The scores are quantised to a handful of
// values (0.68, 0.75, 0.78, 0.82, 0.85) and 0.75 sits exactly on the boundary,
// so a scenario can flip between escalating and not without anything changing.
var ambiguousScenarios = []struct {
	name string
	in   decide.DecisionInput

	// observedConfidence is the modal live measurement; observedBelow is how many
	// of observedRuns calls fell under the threshold.
	observedConfidence float64
	observedAction     string
	observedBelow      int
	observedRuns       int
}{
	{
		name: "insufficient funds, large amount stale 90 minutes",
		in: decide.DecisionInput{
			Category: "insufficient_funds", ErrorReason: "insufficient_funds",
			PaymentMethod: "card", AmountPaise: 25000000,
			AttemptCount: 1, TimeSinceFailureSeconds: 5400, RemainingRetryBudget: 1,
		},
		observedConfidence: 0.68,
		observedAction:     decide.ActionRetryDelayed,
		observedBelow:      6, observedRuns: 6,
	},
	{
		name: "insufficient funds, large amount stale 3 hours",
		in: decide.DecisionInput{
			Category: "insufficient_funds", ErrorReason: "insufficient_funds",
			PaymentMethod: "card", AmountPaise: 10000000,
			AttemptCount: 1, TimeSinceFailureSeconds: 10800, RemainingRetryBudget: 1,
		},
		observedConfidence: 0.68,
		observedAction:     decide.ActionRetryDelayed,
		observedBelow:      4, observedRuns: 6,
	},
}

// TestAmbiguousScenariosEscalate is the deterministic half: given the confidence
// these inputs were measured to produce, the gate must override them. Runs
// everywhere, with no API key and no network.
func TestAmbiguousScenariosEscalate(t *testing.T) {
	for _, sc := range ambiguousScenarios {
		t.Run(sc.name, func(t *testing.T) {
			if sc.observedConfidence >= confidenceThreshold {
				t.Fatalf("scenario is no longer ambiguous: observed %v is not below the %v threshold", sc.observedConfidence, confidenceThreshold)
			}

			gated, overridden := applyConfidenceGate(decide.Decision{
				Action:          sc.observedAction,
				Confidence:      sc.observedConfidence,
				Reasoning:       "measured live",
				CustomerMessage: "We'll automatically retry your payment shortly.",
			})

			if !overridden {
				t.Errorf("confidence %v did not trigger the gate", sc.observedConfidence)
			}
			if gated.Action != decide.ActionEscalate {
				t.Errorf("action = %q, want %q", gated.Action, decide.ActionEscalate)
			}
			if gated.Confidence != sc.observedConfidence {
				t.Errorf("confidence = %v, want %v untouched", gated.Confidence, sc.observedConfidence)
			}
		})
	}
}

// TestAmbiguousScenariosStillScoreBelowThreshold re-measures against the real
// API and fails if a scenario has drifted above the threshold in the majority of
// runs. It asks for a majority rather than every run because the scores are not
// deterministic — a single call is not evidence either way.
//
// Skipped unless RECOVERY_LIVE_TESTS=1: it costs money and needs the network.
// It is the only thing that can tell you the calibration has gone stale after a
// model or prompt change, which would quietly turn the gate back into a branch
// that never executes.
func TestAmbiguousScenariosStillScoreBelowThreshold(t *testing.T) {
	if os.Getenv("RECOVERY_LIVE_TESTS") != "1" {
		t.Skip("set RECOVERY_LIVE_TESTS=1 to re-measure against the live API")
	}

	client, err := decide.NewClient()
	if err != nil {
		t.Fatalf("live test needs an API key: %v", err)
	}

	const runs = 5
	for _, sc := range ambiguousScenarios {
		t.Run(sc.name, func(t *testing.T) {
			below := 0
			scores := make([]float64, 0, runs)
			for i := 0; i < runs; i++ {
				d, _, err := client.Decide(context.Background(), sc.in)
				if err != nil {
					t.Fatalf("Decide: %v", err)
				}
				scores = append(scores, d.Confidence)
				if d.Confidence < confidenceThreshold {
					below++
				}
				time.Sleep(150 * time.Millisecond)
			}

			t.Logf("live scores %v — %d/%d below %v (recorded: %d/%d)", scores, below, runs, confidenceThreshold, sc.observedBelow, sc.observedRuns)
			if below*2 <= runs {
				t.Errorf("only %d/%d runs scored below %v — the calibration is stale and the gate no longer fires on this input", below, runs, confidenceThreshold)
			}
		})
	}
}
