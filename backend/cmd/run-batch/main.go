// Command run-batch fires N simulated failures through the real pipeline and
// scores what the agent's routing recovered against a blind retry baseline.
//
// SIMULATION BOUNDARY. The failures are simulated and so are the outcomes: no
// gateway is called and no money moves. What is NOT simulated is everything in
// between — each payment goes through the real webhook handler, so the real
// classifier, dedupe, attempt counter, stopping rule, decision layer and
// confidence gate all run exactly as they do in production. The shortcut would
// be to pick an action here and score it; that would measure nothing, because
// the action is the thing under test. See internal/simulate for the outcome
// probabilities and docs/README.md for why the baseline sits at 20%.
//
// REPRODUCIBILITY. Two things must be true for a rupee figure to be checkable,
// and they pull in opposite directions:
//
//   - The scenario mix, the amounts and the outcome draws must come from the
//     stored seed, so the same seed produces the same numbers.
//   - The payment and event ids must NOT come from the seed. If they did, a
//     rerun would replay the same payment ids into a database that still
//     remembers them: attempt counts would climb, the stopping rule would fire
//     on payments that had budget the first time, and the second run would
//     measure a different system. Ids therefore come from a separate clock-
//     seeded source and differ every run, which is what keeps each run a clean
//     first observation of a fresh payment.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/db"
	"github.com/bhavyamsharmaa/recovery-agent/internal/simulate"
	"github.com/bhavyamsharmaa/recovery-agent/internal/trace"
)

const dbTimeout = 30 * time.Second

func main() {
	size := flag.Int("size", 100, "number of simulated failures to run through the pipeline")
	seed := flag.Int64("seed", 0, "RNG seed; 0 (the default) uses the current unix timestamp")
	url := flag.String("url", "http://localhost:8080/webhook/payment-failed", "webhook endpoint to POST to")
	flag.Parse()

	if *size < 1 {
		fmt.Fprintln(os.Stderr, "--size must be at least 1")
		os.Exit(2)
	}
	if *seed == 0 {
		*seed = time.Now().Unix()
	}

	// Printed before any work starts, not only in the summary, so a run that
	// crashes halfway still leaves the seed needed to reproduce it.
	fmt.Printf("Batch starting: %d payments, seed=%d\n\n", *size, *seed)

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	pool, err := db.Connect(ctx)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "database unavailable:", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := run(pool, *url, *size, *seed); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// totals accumulates the figures written to batch_runs.
type totals struct {
	atRisk            int64
	recovered         int64
	baselineRecovered int64

	// Counts by outcome, for the per-run breakdown. Not stored — they are a
	// reader's aid, and the outcomes table holds the authoritative per-payment
	// record.
	byOutcome map[string]int
	skipped   int
}

func run(pool *sql.DB, url string, size int, seed int64) error {
	// Ids come from the clock, deliberately, and never from the seed. See the
	// package comment: a seeded id would make a rerun collide with the previous
	// run's payments and measure a different system.
	ids := rand.New(rand.NewSource(time.Now().UnixNano()))

	// Generation is seeded, so the scenario mix and the amounts — and therefore
	// the "at risk" total — are identical for a given seed.
	gen := rand.New(rand.NewSource(seed))

	runID, err := startRun(pool, size, seed)
	if err != nil {
		return err
	}

	t := totals{byOutcome: map[string]int{}}

	for i := 0; i < size; i++ {
		scenario := simulate.PickScenario(gen)
		paymentID := simulate.RazorpayID(ids, "pay")
		eventID := simulate.RazorpayID(ids, "evt")

		body := simulate.MustMarshal(simulate.Build(gen, scenario, paymentID))

		status, err := simulate.Send(url, eventID, body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  [%d/%d] %s: send failed: %v\n", i+1, size, paymentID, err)
			t.skipped++
			continue
		}
		if status < 200 || status > 299 {
			fmt.Fprintf(os.Stderr, "  [%d/%d] %s: webhook returned %d\n", i+1, size, paymentID, status)
			t.skipped++
			continue
		}

		// Read back what the real pipeline decided, through the same queries
		// cmd/trace-payment and the API use.
		ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
		full, err := trace.Load(ctx, pool, paymentID)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "  [%d/%d] %s: could not read back: %v\n", i+1, size, paymentID, err)
			t.skipped++
			continue
		}
		if len(full.Decisions) == 0 {
			// Nothing decided means nothing to score. Counting it as a failed
			// recovery would blame the routing for a payment it never ruled on.
			fmt.Fprintf(os.Stderr, "  [%d/%d] %s: no decision recorded\n", i+1, size, paymentID)
			t.skipped++
			continue
		}

		// The last decision is the effective one: the confidence gate records
		// its override alongside the model answer it overrode, and the override
		// is what actually happened to the payment.
		decision := full.Decisions[len(full.Decisions)-1]
		amount := full.Payment.AmountPaise

		// A fresh RNG per payment, derived from the batch seed and the payment's
		// index, rather than one shared stream.
		//
		// A shared stream would be reproducible but brittle: escalate and
		// no_retry consume no draw, so if the model returned a different action
		// for one payment between runs, every subsequent payment's draw would
		// shift and the two runs would diverge completely. Deriving per payment
		// confines that to the one payment it happened to, which is the
		// difference between a summary that is reproducible in practice and one
		// that is only reproducible in theory.
		outcomeRNG := rand.New(rand.NewSource(derive(seed, "outcome", i)))
		baselineRNG := rand.New(rand.NewSource(derive(seed, "baseline", i)))

		outcome := simulate.ResolveOutcome(outcomeRNG, decision.Action)
		baseline := simulate.NaiveBaselineOutcome(baselineRNG)

		if err := recordOutcome(pool, paymentID, decision.ID, outcome); err != nil {
			return fmt.Errorf("record outcome for %s: %w", paymentID, err)
		}

		t.atRisk += amount
		t.byOutcome[outcome]++
		if outcome == simulate.OutcomeRecovered {
			t.recovered += amount
		}
		// The baseline is accumulated only. It is not written to outcomes,
		// because that table is a record of what happened to real payments in
		// this system, and the baseline is a strategy this system did not run.
		if baseline == simulate.OutcomeRecovered {
			t.baselineRecovered += amount
		}

		fmt.Printf("  [%d/%d] %-19s %-19s %-22s -> %-18s (baseline %s) %s\n",
			i+1, size, paymentID, scenario, decision.Action, outcome, baseline, rupees(amount))
	}

	return finishRun(pool, runID, t, size, seed)
}

// derive produces a per-payment seed from the batch seed, a stream name and an
// index, so the agent's draw and the baseline's draw for one payment are
// independent of each other and of every other payment.
//
// The mixing is a plain FNV-1a over the three inputs — not cryptographic, and
// it does not need to be. It only needs to be deterministic and to spread
// neighbouring indices apart, so that payment 7 and payment 8 do not draw
// near-identical values.
func derive(seed int64, stream string, index int) int64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	mix := func(b byte) {
		h ^= uint64(b)
		h *= prime64
	}
	for i := 0; i < 8; i++ {
		mix(byte(uint64(seed) >> (8 * i)))
	}
	for i := 0; i < len(stream); i++ {
		mix(stream[i])
	}
	for i := 0; i < 8; i++ {
		mix(byte(uint64(index) >> (8 * i)))
	}
	return int64(h)
}

// startRun writes the batch_runs row before any payment is processed, so a run
// that crashes leaves evidence it was attempted. The seed is stored here, at
// insert, because a row without one describes a result nobody can reproduce.
func startRun(pool *sql.DB, size int, seed int64) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	var id int64
	err := pool.QueryRowContext(ctx, `
		INSERT INTO batch_runs (batch_size, rng_seed)
		VALUES ($1, $2)
		RETURNING id`, size, seed).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("start batch run: %w", err)
	}
	return id, nil
}

// recordOutcome writes one row to the outcomes table — the first thing in this
// project to do so. The decision id is carried, so an outcome is attached to the
// specific decision it resulted from rather than only to the payment.
func recordOutcome(pool *sql.DB, paymentID string, decisionID int64, outcome string) error {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	_, err := pool.ExecContext(ctx, `
		INSERT INTO outcomes (payment_id, decision_id, outcome)
		VALUES ($1, $2, $3)`, paymentID, decisionID, outcome)
	return err
}

func finishRun(pool *sql.DB, runID int64, t totals, size int, seed int64) error {
	// Rates are computed against money at risk, not against payment count, and
	// only over the payments actually scored. A run where some payments were
	// skipped would otherwise report a rate diluted by payments it never ruled
	// on.
	var rate, baselineRate float64
	if t.atRisk > 0 {
		rate = float64(t.recovered) / float64(t.atRisk)
		baselineRate = float64(t.baselineRecovered) / float64(t.atRisk)
	}

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	if _, err := pool.ExecContext(ctx, `
		UPDATE batch_runs
		SET completed_at = now(),
		    total_at_risk_paise = $2,
		    total_recovered_paise = $3,
		    recovery_rate = $4,
		    baseline_recovered_paise = $5,
		    baseline_recovery_rate = $6
		WHERE id = $1`,
		runID, t.atRisk, t.recovered, rate, t.baselineRecovered, baselineRate); err != nil {
		return fmt.Errorf("finish batch run: %w", err)
	}

	fmt.Printf("\nBatch complete: %d payments, seed=%d\n", size, seed)
	fmt.Printf("At risk:    %s\n", rupees(t.atRisk))
	fmt.Printf("Recovered:  %s  (%.1f%% recovery rate)\n", rupees(t.recovered), rate*100)
	fmt.Printf("Baseline:   %s  (%.1f%% recovery rate)\n", rupees(t.baselineRecovered), baselineRate*100)
	fmt.Printf("Improvement: %+.1f percentage points over naive baseline\n", (rate-baselineRate)*100)

	// The breakdown is printed after the headline figures rather than instead of
	// them. escalated_pending is the number worth watching: those are payments
	// the agent deliberately did not attempt, and counting them as failures
	// would misread a policy choice as a loss.
	fmt.Printf("\nOutcomes: %d recovered, %d still_failed, %d escalated_pending",
		t.byOutcome[simulate.OutcomeRecovered],
		t.byOutcome[simulate.OutcomeStillFailed],
		t.byOutcome[simulate.OutcomeEscalatedPending])
	if t.skipped > 0 {
		fmt.Printf(", %d skipped (not scored)", t.skipped)
	}
	fmt.Printf("\nbatch_runs id=%d\n", runID)
	return nil
}

// rupees renders paise with Indian digit grouping, matching what the dashboard
// shows for the same number.
func rupees(paise int64) string {
	neg := paise < 0
	if neg {
		paise = -paise
	}
	whole := paise / 100
	frac := paise % 100

	digits := fmt.Sprintf("%d", whole)
	var grouped string
	if len(digits) <= 3 {
		grouped = digits
	} else {
		// Indian grouping: the last three digits, then pairs.
		head, tail := digits[:len(digits)-3], digits[len(digits)-3:]
		var parts []string
		for len(head) > 2 {
			parts = append([]string{head[len(head)-2:]}, parts...)
			head = head[:len(head)-2]
		}
		if head != "" {
			parts = append([]string{head}, parts...)
		}
		grouped = strings.Join(parts, ",") + "," + tail
	}

	sign := ""
	if neg {
		sign = "-"
	}
	return fmt.Sprintf("%s₹%s.%02d", sign, grouped, frac)
}
