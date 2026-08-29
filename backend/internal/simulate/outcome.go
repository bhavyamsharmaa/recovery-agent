// Package simulate models whether a decision would have recovered a payment.
//
// SIMULATION BOUNDARY — READ THIS BEFORE TRUSTING ANY NUMBER THIS PRODUCES.
//
// Nothing here calls a payment gateway. No card is charged, no retry is
// attempted against Razorpay, and no money moves. Every "recovered" this package
// reports is a seeded coin flip against a probability written down below by
// hand. The purpose is to compare two routing policies — this agent's
// category-aware decisions against a blind retry-everything strategy — over the
// same payments and the same random stream. It measures a policy, not an
// outcome.
//
// This is a real scope boundary, not a placeholder to be quietly upgraded: the
// day something here does call a gateway, these probabilities must be deleted
// rather than kept alongside it, because a mixture of measured and invented
// outcomes in one table is worse than either alone.
//
// The randomness is passed in rather than taken from the global source, so a
// whole batch run is reproducible from the seed stored on its batch_runs row. A
// recovery figure in rupees that cannot be regenerated is a number nobody can
// check.
package simulate

import "math/rand"

// The three outcomes this package can report. They are the values written to
// outcomes.outcome, so they are declared once here rather than spelled out at
// each call site.
const (
	// OutcomeRecovered: the simulated attempt succeeded.
	OutcomeRecovered = "recovered"
	// OutcomeStillFailed: the simulated attempt was made and did not succeed.
	OutcomeStillFailed = "still_failed"
	// OutcomeEscalatedPending: no automated attempt was made at all. This is not
	// a failure — it is the absence of a result, and it stays distinct from
	// still_failed for the same reason a NULL confidence stays distinct from 0.
	OutcomeEscalatedPending = "escalated_pending"
)

// RecoveryProbability returns the simulated chance a given action resolves the
// payment successfully. These are declared, reasoned assumptions for demo
// purposes, NOT measured real-world rates — ordering matters more than the exact
// digits: suggest_alternate_method > retry_now > retry_delayed reflects that a
// different payment method recovers more reliably than a blind retry, and an
// immediate customer-input retry (wrong CVV/OTP) beats waiting on an uncertain
// bank-side condition to clear.
var actionRecoveryProbability = map[string]float64{
	"retry_now":                0.55,
	"retry_delayed":            0.40,
	"suggest_alternate_method": 0.65,
	// escalate and no_retry are intentionally absent — they never
	// auto-resolve, see ResolveOutcome below.
}

// naiveBaselineRecoveryProbability is the comparison strategy: retry every
// failure every two hours, regardless of category.
//
// DELIBERATELY set lower than the ~30% naive-retry recovery rate cited in the
// PRD. That industry figure describes retriable failures broadly, while this
// baseline blindly retries structurally-dead declines too — a hard_decline is a
// blocked card or a failed fraud check, and no number of retries on the same
// instrument will ever succeed. Those doomed attempts are counted in the
// denominator here and are not in the PRD's, which is the entire difference.
// The gap between 20% and 30% is category-aware routing declining to spend
// attempts it knows cannot work; it is not an inconsistency between the two
// numbers. This reasoning is also recorded in docs/README.md, because a reader
// comparing the two figures will be looking there, not here.
const naiveBaselineRecoveryProbability = 0.20

// RecoveryProbability reports the configured chance for an action, and whether
// the action has one at all.
//
// The second return value distinguishes "this action is not simulated as
// recovering" from "this action recovers 0% of the time". Nothing currently
// declares 0, but a caller reading a bare float64 could not tell an absent entry
// from a deliberate zero, and that is exactly the confusion this project keeps
// refusing to introduce elsewhere.
func RecoveryProbability(action string) (float64, bool) {
	p, ok := actionRecoveryProbability[action]
	return p, ok
}

// ResolveOutcome uses a seeded RNG (passed in, not global, so a batch run is
// fully reproducible from its stored rng_seed) to decide whether a decision's
// action "recovered" the payment. Returns one of: "recovered", "still_failed",
// "escalated_pending". escalate and no_retry ALWAYS return "escalated_pending" —
// never auto-resolved, consistent with the rest of this project's philosophy of
// not fabricating a status for something no signal actually confirms.
//
// An unrecognised action takes the same path as escalate. A new action added to
// the decision layer without a probability being chosen for it must not silently
// inherit one; reporting that nothing was attempted is the honest answer until
// somebody decides what its rate should be.
//
// Note that the RNG is only consulted for actions that have a probability. That
// keeps the random stream aligned with the attempts actually simulated, so two
// runs over the same payments with the same seed draw the same numbers for the
// same decisions.
func ResolveOutcome(rng *rand.Rand, action string) string {
	p, ok := actionRecoveryProbability[action]
	if !ok {
		return OutcomeEscalatedPending
	}
	if rng.Float64() < p {
		return OutcomeRecovered
	}
	return OutcomeStillFailed
}

// NaiveBaselineOutcome simulates a blind, fixed-interval retry strategy (retry
// every 2 hours regardless of category) applied to EVERY failure — including
// hard_decline, which this project's system would never retry at all.
//
// It takes no action, which is the point: the baseline does not look at what
// failed. Every payment gets the same draw against the same probability, which
// is precisely the behaviour category-aware routing exists to beat.
func NaiveBaselineOutcome(rng *rand.Rand) string {
	if rng.Float64() < naiveBaselineRecoveryProbability {
		return OutcomeRecovered
	}
	return OutcomeStillFailed
}
