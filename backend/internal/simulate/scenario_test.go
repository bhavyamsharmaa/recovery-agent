package simulate

import (
	"math/rand"
	"testing"
)

// Scenario generation, held to the same standard as outcome.go's determinism
// tests: the same seed must produce the same sequence, compared position by
// position rather than in aggregate.
//
// This matters because a batch run's "total at risk" is the sum of amounts drawn
// here. If Build's draws were not reproducible from the stored seed, the rupee
// figure on the dashboard could not be regenerated, and the whole reproducibility
// claim would be false in its first term.

// TestBuildIsDeterministic runs the same generation twice under one seed and
// compares every field of every event. An aggregate comparison would pass on
// shuffled output.
func TestBuildIsDeterministic(t *testing.T) {
	const seed = 20260831
	const n = 200

	run := func() []Event {
		rng := rand.New(rand.NewSource(seed))
		out := make([]Event, n)
		for i := range out {
			// The scenario is drawn from the same stream, deliberately: it is
			// how a batch run picks its mix, so the interleaving of scenario
			// draws and amount draws is part of what must be reproducible.
			out[i] = Build(rng, PickScenario(rng), "")
		}
		return out
	}

	first, second := run(), run()

	for i := range first {
		a, b := first[i].Payload.Payment.Entity, second[i].Payload.Payment.Entity
		if a.ID != b.ID {
			t.Fatalf("payment id diverged at index %d: %q vs %q", i, a.ID, b.ID)
		}
		if a.Amount != b.Amount {
			t.Fatalf("amount diverged at index %d: %d vs %d", i, a.Amount, b.Amount)
		}
		if a.ErrorReason != b.ErrorReason || a.ErrorSource != b.ErrorSource {
			t.Fatalf("scenario diverged at index %d: %s/%s vs %s/%s",
				i, a.ErrorReason, a.ErrorSource, b.ErrorReason, b.ErrorSource)
		}
		if a.OrderID != b.OrderID || a.Method != b.Method {
			t.Fatalf("order id or method diverged at index %d", i)
		}
	}

	t.Logf("seed %d, %d events: identical at every position", seed, n)
	t.Logf("first three amounts (paise): %d, %d, %d",
		first[0].Payload.Payment.Entity.Amount,
		first[1].Payload.Payment.Entity.Amount,
		first[2].Payload.Payment.Entity.Amount)
}

// TestBuildDiffersUnderDifferentSeeds guards the other direction. A "deterministic"
// generator that ignored the RNG would pass the test above trivially.
func TestBuildDiffersUnderDifferentSeeds(t *testing.T) {
	amounts := func(seed int64) []int {
		rng := rand.New(rand.NewSource(seed))
		out := make([]int, 50)
		for i := range out {
			out[i] = Build(rng, "insufficient_funds", "").Payload.Payment.Entity.Amount
		}
		return out
	}

	a, b := amounts(1), amounts(2)
	same := true
	for i := range a {
		if a[i] != b[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("two different seeds produced identical amounts — the RNG is not being consulted")
	}
}

// TestBuildAmountsAreInRange pins the documented bounds. The amount is what the
// "at risk" total sums, so a generator drifting outside its range would move
// every figure on the dashboard.
func TestBuildAmountsAreInRange(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 2000; i++ {
		amount := Build(rng, "insufficient_funds", "").Payload.Payment.Entity.Amount
		// 100 to 50000 rupees, sent as paise.
		if amount < 100*100 || amount > 50000*100 {
			t.Fatalf("amount %d paise is outside the documented 100..50000 rupee range", amount)
		}
		if amount%100 != 0 {
			t.Fatalf("amount %d paise is not a whole number of rupees", amount)
		}
	}
}

// TestBuildHonoursAnExplicitPaymentID: a non-empty id is reused verbatim, which
// is what lets the same payment be delivered repeatedly under fresh event ids.
func TestBuildHonoursAnExplicitPaymentID(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	e := Build(rng, "soft_decline", "pay_explicit_id")
	if got := e.Payload.Payment.Entity.ID; got != "pay_explicit_id" {
		t.Errorf("payment id = %q, want pay_explicit_id", got)
	}
}

// TestBuildSetsScenarioFieldsFromTheTaxonomy checks that each scenario carries
// the error_reason and error_source that make it classify as its namesake
// category. These come from docs/taxonomy.md; a drift here would silently change
// what a batch is measuring.
func TestBuildSetsScenarioFieldsFromTheTaxonomy(t *testing.T) {
	rng := rand.New(rand.NewSource(11))

	cases := map[string]struct{ reason, source, method string }{
		"insufficient_funds": {"insufficient_funds", "customer", "card"},
		"bank_downtime":      {"bank_technical_error", "bank", "netbanking"},
		"hard_decline":       {"card_declined", "bank", "card"},
		"soft_decline":       {"authentication_failed", "customer", "card"},
		"network_error":      {"gateway_technical_error", "gateway", "upi"},
	}

	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			e := Build(rng, name, "").Payload.Payment.Entity
			if e.ErrorReason != want.reason {
				t.Errorf("error_reason = %q, want %q", e.ErrorReason, want.reason)
			}
			if e.ErrorSource != want.source {
				t.Errorf("error_source = %q, want %q", e.ErrorSource, want.source)
			}
			if e.Method != want.method {
				t.Errorf("method = %q, want %q", e.Method, want.method)
			}
			if e.Status != "failed" || e.Currency != "INR" {
				t.Errorf("status/currency = %q/%q, want failed/INR", e.Status, e.Currency)
			}
		})
	}
}

// TestPickScenarioIsDeterministicAndCoversEveryCategory.
//
// The coverage half matters as much as the determinism half: a batch that only
// ever produced one category would still be reproducible and would measure
// nothing about category-aware routing.
func TestPickScenarioIsDeterministic(t *testing.T) {
	const seed = 424242
	const n = 300

	run := func() []string {
		rng := rand.New(rand.NewSource(seed))
		out := make([]string, n)
		for i := range out {
			out[i] = PickScenario(rng)
		}
		return out
	}

	first, second := run(), run()
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("diverged at index %d: %q vs %q", i, first[i], second[i])
		}
	}

	seen := map[string]int{}
	for _, s := range first {
		seen[s]++
	}
	for _, name := range ScenarioNames {
		if seen[name] == 0 {
			t.Errorf("scenario %q was never picked across %d draws", name, n)
		}
	}
	// "duplicate" is a delivery behaviour, not a failure mode, and must never be
	// picked as one.
	if seen["duplicate"] > 0 {
		t.Error("PickScenario returned \"duplicate\", which is a delivery behaviour rather than a category")
	}
	t.Logf("seed %d, %d draws: identical at every position; distribution %v", seed, n, seen)
}

// TestRazorpayIDIsDeterministicAndWellFormed.
//
// Determinism here is what lets a caller choose: a batch run deliberately draws
// ids from a clock-seeded stream so reruns do not collide, and that choice only
// means something if a seeded stream would in fact repeat.
func TestRazorpayIDIsDeterministic(t *testing.T) {
	const seed = 99

	run := func() []string {
		rng := rand.New(rand.NewSource(seed))
		out := make([]string, 100)
		for i := range out {
			out[i] = RazorpayID(rng, "pay")
		}
		return out
	}

	first, second := run(), run()
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("diverged at index %d: %q vs %q", i, first[i], second[i])
		}
	}

	// Shape: <prefix>_<14 alphanumeric>.
	for i, id := range first {
		if len(id) != len("pay_")+14 {
			t.Fatalf("id %d is %q, want a 14-character suffix", i, id)
		}
		if id[:4] != "pay_" {
			t.Fatalf("id %d is %q, want a pay_ prefix", i, id)
		}
		for _, c := range id[4:] {
			isAlnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
			if !isAlnum {
				t.Fatalf("id %q contains a non-alphanumeric character %q", id, c)
			}
		}
	}

	// And they do not repeat within one stream.
	seen := map[string]bool{}
	for _, id := range first {
		if seen[id] {
			t.Fatalf("RazorpayID produced a duplicate within one stream: %q", id)
		}
		seen[id] = true
	}
}

// TestKnownScenario covers the guard the simulate endpoint uses to validate its
// one input.
func TestKnownScenario(t *testing.T) {
	for _, name := range ScenarioNames {
		if !KnownScenario(name) {
			t.Errorf("KnownScenario(%q) = false", name)
		}
	}
	if !KnownScenario("duplicate") {
		t.Error("duplicate should be buildable even though it is not a pick-list category")
	}
	for _, name := range []string{"", "nonsense", "INSUFFICIENT_FUNDS"} {
		if KnownScenario(name) {
			t.Errorf("KnownScenario(%q) = true", name)
		}
	}
}
