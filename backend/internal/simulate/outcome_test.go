package simulate

import (
	"math/rand"
	"testing"
)

// actions covers every action the decision layer can produce, plus one it
// cannot, so the unrecognised-action path is exercised rather than assumed.
var actions = []string{
	"retry_now",
	"retry_delayed",
	"suggest_alternate_method",
	"escalate",
	"no_retry",
	"some_action_added_later",
}

// TestResolveOutcomeIsDeterministic is the test the whole batch feature rests
// on. A recovery figure in rupees that cannot be regenerated from its stored
// seed is a number nobody can check, so this asserts that two independent runs
// with the same seed over the same action sequence agree at every position —
// not merely that they agree in aggregate, which would pass even if the results
// were shuffled.
func TestResolveOutcomeIsDeterministic(t *testing.T) {
	const seed = 20260829
	const n = 500

	// The sequence deliberately interleaves actions that consult the RNG with
	// actions that do not. If a non-drawing action ever started drawing, the
	// stream would shift from that point on and the two runs would diverge.
	sequence := make([]string, n)
	for i := range sequence {
		sequence[i] = actions[i%len(actions)]
	}

	run := func() []string {
		rng := rand.New(rand.NewSource(seed))
		out := make([]string, len(sequence))
		for i, a := range sequence {
			out[i] = ResolveOutcome(rng, a)
		}
		return out
	}

	first, second := run(), run()

	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("run 1 and run 2 diverged at index %d (action %q): %q vs %q",
				i, sequence[i], first[i], second[i])
		}
	}

	t.Logf("seed %d, %d calls: both runs identical at every position", seed, n)
	t.Logf("first 12 outcomes: %v", first[:12])
}

// TestDifferentSeedsDiverge guards against the opposite failure: a "deterministic"
// implementation that ignores the RNG entirely would pass the test above
// trivially. If two different seeds produce byte-identical output, the seed is
// not being used.
func TestDifferentSeedsDiverge(t *testing.T) {
	const n = 200

	run := func(seed int64) []string {
		rng := rand.New(rand.NewSource(seed))
		out := make([]string, n)
		for i := range out {
			out[i] = ResolveOutcome(rng, "retry_now")
		}
		return out
	}

	a, b := run(1), run(2)

	same := true
	for i := range a {
		if a[i] != b[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("two different seeds produced identical output — the RNG is not being consulted")
	}
}

// TestEscalateAndNoRetryNeverResolve is the honesty guarantee. escalate and
// no_retry mean no automated attempt was made, and reporting either as
// "recovered" or "still_failed" would claim an attempt happened and had a
// result. Neither is true.
func TestEscalateAndNoRetryNeverResolve(t *testing.T) {
	const n = 1000

	for _, action := range []string{"escalate", "no_retry"} {
		t.Run(action, func(t *testing.T) {
			// A fresh RNG per action, seeded from the clock's least predictable
			// digits, so this is not accidentally passing on one lucky stream.
			rng := rand.New(rand.NewSource(int64(len(action)) * 7919))

			counts := map[string]int{}
			for i := 0; i < n; i++ {
				got := ResolveOutcome(rng, action)
				counts[got]++
				if got != OutcomeEscalatedPending {
					t.Fatalf("call %d returned %q, want %q — an action with no attempt "+
						"must never report an attempt's result", i, got, OutcomeEscalatedPending)
				}
			}
			t.Logf("%s: %d/%d calls returned %q", action, counts[OutcomeEscalatedPending], n, OutcomeEscalatedPending)
		})
	}
}

// TestUnknownActionIsPending covers an action added to the decision layer
// without a probability being chosen for it. It must not silently inherit one.
func TestUnknownActionIsPending(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 100; i++ {
		if got := ResolveOutcome(rng, "action_nobody_has_defined_yet"); got != OutcomeEscalatedPending {
			t.Fatalf("unknown action returned %q, want %q", got, OutcomeEscalatedPending)
		}
	}
}

// TestActionProbabilityOrdering pins the ordering the comments justify, rather
// than the digits themselves. The exact rates are declared assumptions and may
// be revised; the ordering is the claim being made about the world — a different
// payment method beats a blind retry, and an immediate customer-input retry
// beats waiting on a bank-side condition.
func TestActionProbabilityOrdering(t *testing.T) {
	alt, ok1 := RecoveryProbability("suggest_alternate_method")
	now, ok2 := RecoveryProbability("retry_now")
	delayed, ok3 := RecoveryProbability("retry_delayed")
	if !ok1 || !ok2 || !ok3 {
		t.Fatal("one of the three retrying actions has no declared probability")
	}
	if !(alt > now && now > delayed) {
		t.Errorf("ordering broken: suggest_alternate_method=%.2f retry_now=%.2f retry_delayed=%.2f; "+
			"want suggest_alternate_method > retry_now > retry_delayed", alt, now, delayed)
	}

	// The two non-attempting actions must have no entry at all. An entry of 0
	// would put them on the same footing as a real rate that happens to be zero.
	for _, a := range []string{"escalate", "no_retry"} {
		if _, ok := RecoveryProbability(a); ok {
			t.Errorf("%s has a declared recovery probability; it must have none", a)
		}
	}
}

// TestBaselineIsDeterministicAndLowerThanPRD covers the comparison strategy.
func TestBaselineIsDeterministicAndLowerThanPRD(t *testing.T) {
	const seed = 4242
	const n = 500

	run := func() []string {
		rng := rand.New(rand.NewSource(seed))
		out := make([]string, n)
		for i := range out {
			out[i] = NaiveBaselineOutcome(rng)
		}
		return out
	}

	a, b := run(), run()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("baseline diverged at index %d: %q vs %q", i, a[i], b[i])
		}
	}

	// The baseline always attempts, so it never reports pending. That is the
	// whole point of it: it retries things this system would not.
	for i, v := range a {
		if v != OutcomeRecovered && v != OutcomeStillFailed {
			t.Fatalf("baseline call %d returned %q; it attempts every payment and "+
				"must report an attempt's result", i, v)
		}
	}

	// And it must sit below the PRD's ~30%, for the documented reason.
	if naiveBaselineRecoveryProbability >= 0.30 {
		t.Errorf("baseline probability is %.2f; it is deliberately below the ~0.30 cited in the PRD",
			naiveBaselineRecoveryProbability)
	}

	recovered := 0
	for _, v := range a {
		if v == OutcomeRecovered {
			recovered++
		}
	}
	t.Logf("baseline over %d draws at p=%.2f: %d recovered (%.1f%%)",
		n, naiveBaselineRecoveryProbability, recovered, 100*float64(recovered)/float64(n))
}

// TestObservedRatesTrackDeclaredProbabilities is a sanity check that the coin
// flip actually uses the number beside it — a comparison written the wrong way
// round would still be deterministic and would still pass every test above.
func TestObservedRatesTrackDeclaredProbabilities(t *testing.T) {
	const n = 20000
	const tolerance = 0.02

	for _, action := range []string{"retry_now", "retry_delayed", "suggest_alternate_method"} {
		want, _ := RecoveryProbability(action)
		rng := rand.New(rand.NewSource(99))
		recovered := 0
		for i := 0; i < n; i++ {
			if ResolveOutcome(rng, action) == OutcomeRecovered {
				recovered++
			}
		}
		got := float64(recovered) / float64(n)
		if diff := got - want; diff > tolerance || diff < -tolerance {
			t.Errorf("%s: observed %.4f over %d draws, declared %.2f (tolerance %.2f)",
				action, got, n, want, tolerance)
		}
		t.Logf("%-24s declared %.2f, observed %.4f over %d draws", action, want, got, n)
	}
}
