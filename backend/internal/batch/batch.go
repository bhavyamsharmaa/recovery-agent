// Package batch fires N simulated failures through the real pipeline and scores
// what the agent's routing recovered against a blind retry baseline.
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
//
// This lives in internal/ rather than in cmd/run-batch because the dashboard's
// "Run new batch" button needs the same code the CLI runs. A batch triggered
// from a browser and one triggered from a terminal must be the same batch, or
// the figures on screen describe something the CLI cannot reproduce.
package batch

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/simulate"
	"github.com/bhavyamsharmaa/recovery-agent/internal/trace"
)

const dbTimeout = 30 * time.Second

// DefaultWebhookURL is where a batch posts its simulated failures. It is the
// server's own webhook endpoint: a batch triggered from the API loops back
// through the full HTTP path rather than calling the handler in-process, so the
// batch exercises exactly what Razorpay would hit.
const DefaultWebhookURL = "http://localhost:8080/webhook/payment-failed"

// Options configures one run.
type Options struct {
	Size int
	Seed int64
	URL  string

	// Progress, if set, is called after each payment is scored. The API uses it
	// for logging; the CLI uses it to print the per-payment line.
	Progress func(index int, paymentID, scenario, action, outcome, baseline string, amountPaise int64)

	// Skipped, if set, is called when a payment could not be scored at all.
	Skipped func(index int, paymentID, reason string)
}

// Result is the summary of one run, matching the batch_runs row it wrote.
type Result struct {
	ID                     int64
	Size                   int
	Seed                   int64
	TotalAtRiskPaise       int64
	TotalRecoveredPaise    int64
	RecoveryRate           float64
	BaselineRecoveredPaise int64
	BaselineRecoveryRate   float64

	// Counts by outcome. Not stored on batch_runs — the outcomes table holds the
	// authoritative per-payment record — but useful to a caller reporting the
	// run, because escalated_pending is a policy choice rather than a loss and
	// should not be read as one.
	Recovered        int
	StillFailed      int
	EscalatedPending int
	Skipped          int
}

// Run executes a batch and returns its summary.
//
// A seed of 0 means "choose one from the clock"; the chosen value is returned on
// the Result and stored on the row, so a run is always reproducible afterwards
// even when the caller did not pick a seed.
func Run(ctx context.Context, pool *sql.DB, opts Options) (Result, error) {
	if opts.Size < 1 {
		return Result{}, fmt.Errorf("batch: size must be at least 1, got %d", opts.Size)
	}
	if opts.Seed == 0 {
		opts.Seed = time.Now().Unix()
	}
	if opts.URL == "" {
		opts.URL = DefaultWebhookURL
	}

	// Ids come from the clock, deliberately, and never from the seed. See the
	// package comment: a seeded id would make a rerun collide with the previous
	// run's payments and measure a different system.
	ids := rand.New(rand.NewSource(time.Now().UnixNano()))

	// Generation is seeded, so the scenario mix and the amounts — and therefore
	// the "at risk" total — are identical for a given seed.
	gen := rand.New(rand.NewSource(opts.Seed))

	runID, err := startRun(ctx, pool, opts.Size, opts.Seed)
	if err != nil {
		return Result{}, err
	}

	res := Result{ID: runID, Size: opts.Size, Seed: opts.Seed}

	for i := 0; i < opts.Size; i++ {
		// A cancelled request should not leave a batch running against the
		// database forever. The row keeps its NULL completed_at, which is
		// exactly what that column is for.
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("batch: cancelled after %d of %d payments: %w", i, opts.Size, err)
		}

		scenario := simulate.PickScenario(gen)
		paymentID := simulate.RazorpayID(ids, "pay")
		eventID := simulate.RazorpayID(ids, "evt")
		body := simulate.MustMarshal(simulate.Build(gen, scenario, paymentID))

		status, err := simulate.Send(opts.URL, eventID, body)
		if err != nil {
			res.Skipped++
			report(opts.Skipped, i, paymentID, fmt.Sprintf("send failed: %v", err))
			continue
		}
		if status < 200 || status > 299 {
			res.Skipped++
			report(opts.Skipped, i, paymentID, fmt.Sprintf("webhook returned %d", status))
			continue
		}

		// Read back what the real pipeline decided, through the same queries
		// cmd/trace-payment and the read API use.
		readCtx, cancel := context.WithTimeout(ctx, dbTimeout)
		full, err := trace.Load(readCtx, pool, paymentID)
		cancel()
		if err != nil {
			res.Skipped++
			report(opts.Skipped, i, paymentID, fmt.Sprintf("could not read back: %v", err))
			continue
		}
		if len(full.Decisions) == 0 {
			// Nothing decided means nothing to score. Counting it as a failed
			// recovery would blame the routing for a payment it never ruled on.
			res.Skipped++
			report(opts.Skipped, i, paymentID, "no decision recorded")
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
		outcomeRNG := rand.New(rand.NewSource(derive(opts.Seed, "outcome", i)))
		baselineRNG := rand.New(rand.NewSource(derive(opts.Seed, "baseline", i)))

		outcome := simulate.ResolveOutcome(outcomeRNG, decision.Action)
		baseline := simulate.NaiveBaselineOutcome(baselineRNG)

		if err := recordOutcome(ctx, pool, paymentID, decision.ID, outcome); err != nil {
			return res, fmt.Errorf("batch: record outcome for %s: %w", paymentID, err)
		}

		res.TotalAtRiskPaise += amount
		switch outcome {
		case simulate.OutcomeRecovered:
			res.Recovered++
			res.TotalRecoveredPaise += amount
		case simulate.OutcomeStillFailed:
			res.StillFailed++
		case simulate.OutcomeEscalatedPending:
			res.EscalatedPending++
		}
		// The baseline is accumulated only. It is not written to outcomes,
		// because that table is a record of what happened to real payments in
		// this system, and the baseline is a strategy this system did not run.
		if baseline == simulate.OutcomeRecovered {
			res.BaselineRecoveredPaise += amount
		}

		if opts.Progress != nil {
			opts.Progress(i, paymentID, scenario, decision.Action, outcome, baseline, amount)
		}
	}

	// Rates are computed against money at risk, not against payment count, and
	// only over the payments actually scored. A run where some payments were
	// skipped would otherwise report a rate diluted by payments it never ruled
	// on.
	if res.TotalAtRiskPaise > 0 {
		res.RecoveryRate = float64(res.TotalRecoveredPaise) / float64(res.TotalAtRiskPaise)
		res.BaselineRecoveryRate = float64(res.BaselineRecoveredPaise) / float64(res.TotalAtRiskPaise)
	}

	if err := finishRun(ctx, pool, res); err != nil {
		return res, err
	}
	return res, nil
}

func report(f func(int, string, string), i int, paymentID, reason string) {
	if f != nil {
		f(i, paymentID, reason)
	}
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
func startRun(ctx context.Context, pool *sql.DB, size int, seed int64) (int64, error) {
	insertCtx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	var id int64
	err := pool.QueryRowContext(insertCtx, `
		INSERT INTO batch_runs (batch_size, rng_seed)
		VALUES ($1, $2)
		RETURNING id`, size, seed).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("batch: start run: %w", err)
	}
	return id, nil
}

// recordOutcome writes one row to the outcomes table. The decision id is
// carried, so an outcome is attached to the specific decision it resulted from
// rather than only to the payment.
func recordOutcome(ctx context.Context, pool *sql.DB, paymentID string, decisionID int64, outcome string) error {
	writeCtx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	_, err := pool.ExecContext(writeCtx, `
		INSERT INTO outcomes (payment_id, decision_id, outcome)
		VALUES ($1, $2, $3)`, paymentID, decisionID, outcome)
	return err
}

func finishRun(ctx context.Context, pool *sql.DB, res Result) error {
	updateCtx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	if _, err := pool.ExecContext(updateCtx, `
		UPDATE batch_runs
		SET completed_at = now(),
		    total_at_risk_paise = $2,
		    total_recovered_paise = $3,
		    recovery_rate = $4,
		    baseline_recovered_paise = $5,
		    baseline_recovery_rate = $6
		WHERE id = $1`,
		res.ID, res.TotalAtRiskPaise, res.TotalRecoveredPaise, res.RecoveryRate,
		res.BaselineRecoveredPaise, res.BaselineRecoveryRate); err != nil {
		return fmt.Errorf("batch: finish run: %w", err)
	}
	return nil
}
