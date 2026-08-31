package batch

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/db"
)

// The money arithmetic behind the figures on the dashboard.
//
// These run against the real database, like every other Postgres test here,
// because what is under test includes the batch_runs row lifecycle and the
// read-back through trace.Load — neither of which a fake proves anything about.
//
// What they do NOT use is the real webhook handler. Options.URL points at a stub
// in each test, so the decisions are chosen by the test rather than by a live
// model. That is deliberate: the arithmetic must be verifiable against
// hand-computed totals, and a model that occasionally routes a payment
// differently would make the expected numbers unknowable. The real pipeline is
// exercised by cmd/run-batch and the api package's own tests.

func liveDB(t *testing.T) *sql.DB {
	t.Helper()

	if os.Getenv("RECOVERY_LIVE_TESTS") != "1" {
		t.Skip("set RECOVERY_LIVE_TESTS=1 to run against the real database")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

// cleanupRun deletes everything one test created. Children first: outcomes and
// decisions reference failed_payments, and the schema deliberately has no
// ON DELETE CASCADE.
func cleanupRun(t *testing.T, pool *sql.DB, runID int64, paymentIDs *[]string) {
	t.Helper()
	t.Cleanup(func() {
		for _, id := range *paymentIDs {
			if _, err := pool.Exec(`DELETE FROM outcomes WHERE payment_id = $1`, id); err != nil {
				t.Errorf("cleanup outcomes: %v", err)
			}
			if _, err := pool.Exec(`DELETE FROM decisions WHERE payment_id = $1`, id); err != nil {
				t.Errorf("cleanup decisions: %v", err)
			}
			if _, err := pool.Exec(`DELETE FROM failed_payments WHERE payment_id = $1`, id); err != nil {
				t.Errorf("cleanup failed_payments: %v", err)
			}
		}
		if runID != 0 {
			if _, err := pool.Exec(`DELETE FROM batch_runs WHERE id = $1`, runID); err != nil {
				t.Errorf("cleanup batch_runs: %v", err)
			}
		}
	})
}

// stubWebhook stands in for the ingest handler.
//
// It writes the payment and one decision straight to the database — the same
// rows the real handler would leave behind — so trace.Load finds what Run
// expects, with an action the test chose. amountFor and actionFor let a case
// control exactly what the arithmetic will be fed.
type stubWebhook struct {
	t          *testing.T
	pool       *sql.DB
	seen       *[]string
	amountFor  func(i int) int64
	actionFor  func(i int) string
	statusFor  func(i int) int
	skipRecord func(i int) bool
	calls      int
}

func (s *stubWebhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	i := s.calls
	s.calls++

	if s.statusFor != nil {
		if code := s.statusFor(i); code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
	}

	// A payment the webhook accepted but recorded nothing for: the "no decision
	// recorded" skip path.
	if s.skipRecord != nil && s.skipRecord(i) {
		w.WriteHeader(http.StatusOK)
		return
	}

	var payload struct {
		Payload struct {
			Payment struct {
				Entity struct {
					ID string `json:"id"`
				} `json:"entity"`
			} `json:"payment"`
		} `json:"payload"`
	}
	if err := decodeJSON(r, &payload); err != nil {
		s.t.Errorf("stub could not decode body: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	paymentID := payload.Payload.Payment.Entity.ID
	*s.seen = append(*s.seen, paymentID)

	amount := s.amountFor(i)
	now := time.Now().UTC()
	if _, err := s.pool.Exec(`
		INSERT INTO failed_payments (payment_id, category, error_code, error_reason,
		    error_source, payment_method, amount_paise, first_failed_at, last_seen_at,
		    attempt_count)
		VALUES ($1, 'insufficient_funds', 'TEST', 'insufficient_funds', 'customer',
		        'card', $2, $3, $3, 1)`, paymentID, amount, now); err != nil {
		s.t.Errorf("stub insert payment: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if _, err := s.pool.Exec(`
		INSERT INTO decisions (payment_id, attempt_number, action, source, confidence)
		VALUES ($1, 1, $2, 'llm', 0.9)`, paymentID, s.actionFor(i)); err != nil {
		s.t.Errorf("stub insert decision: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// readRun reads a batch_runs row back as stored, so a test asserts on what the
// database holds rather than on the in-memory Result alone.
func readRun(t *testing.T, pool *sql.DB, id int64) StoredRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r, err := scanRun(pool.QueryRowContext(ctx, `SELECT`+runColumns+` FROM batch_runs WHERE id = $1`, id))
	if err != nil {
		t.Fatalf("read back run %d: %v", id, err)
	}
	return r
}

// TestRunAggregatesMoneyAndRates is the core arithmetic case.
//
// Four payments with chosen amounts and chosen actions, so every total below is
// hand-computable. escalate contributes to at-risk and never to recovered,
// which is the distinction the whole comparison rests on.
func TestRunAggregatesMoneyAndRates(t *testing.T) {
	pool := liveDB(t)
	var seen []string

	// Amounts chosen to be distinct and to sum to a round number:
	// 100000 + 200000 + 300000 + 400000 = 1000000 paise = ₹10,000.00
	amounts := []int64{100000, 200000, 300000, 400000}
	// retry_now recovers at p=0.55, escalate never resolves. Whether a given
	// retry_now draw recovers is decided by the seeded per-payment RNG, so the
	// test reads the outcomes table rather than assuming which ones won.
	actions := []string{"retry_now", "retry_now", "escalate", "retry_now"}

	stub := &stubWebhook{
		t: t, pool: pool, seen: &seen,
		amountFor: func(i int) int64 { return amounts[i] },
		actionFor: func(i int) string { return actions[i] },
	}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	res, err := Run(context.Background(), pool, Options{Size: 4, Seed: 12345, URL: srv.URL})
	cleanupRun(t, pool, res.ID, &seen)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// At-risk is every scored payment, regardless of outcome.
	if want := int64(1000000); res.TotalAtRiskPaise != want {
		t.Errorf("TotalAtRiskPaise = %d, want %d", res.TotalAtRiskPaise, want)
	}
	if res.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0", res.Skipped)
	}
	if got := res.Recovered + res.StillFailed + res.EscalatedPending; got != 4 {
		t.Errorf("outcome counts sum to %d, want 4", got)
	}
	// The one escalate must land in escalated_pending and nowhere else.
	if res.EscalatedPending != 1 {
		t.Errorf("EscalatedPending = %d, want 1 (the single escalate)", res.EscalatedPending)
	}

	// Recovered money must equal the sum of the amounts whose outcome row says
	// recovered — computed from the database, not from the Result being tested.
	var recoveredFromDB int64
	if err := pool.QueryRow(`
		SELECT COALESCE(SUM(p.amount_paise), 0)
		FROM outcomes o JOIN failed_payments p ON p.payment_id = o.payment_id
		WHERE o.payment_id = ANY($1) AND o.outcome = 'recovered'`, seen).Scan(&recoveredFromDB); err != nil {
		t.Fatalf("sum recovered: %v", err)
	}
	if res.TotalRecoveredPaise != recoveredFromDB {
		t.Errorf("TotalRecoveredPaise = %d, but the outcomes table sums to %d",
			res.TotalRecoveredPaise, recoveredFromDB)
	}

	// Rates are recovered over at-risk, to the last bit.
	wantRate := float64(res.TotalRecoveredPaise) / float64(res.TotalAtRiskPaise)
	if res.RecoveryRate != wantRate {
		t.Errorf("RecoveryRate = %v, want %v", res.RecoveryRate, wantRate)
	}
	wantBaseline := float64(res.BaselineRecoveredPaise) / float64(res.TotalAtRiskPaise)
	if res.BaselineRecoveryRate != wantBaseline {
		t.Errorf("BaselineRecoveryRate = %v, want %v", res.BaselineRecoveryRate, wantBaseline)
	}

	// The stored row must agree with the returned Result. A summary printed from
	// memory that disagrees with the row is a number nobody can reproduce.
	stored := readRun(t, pool, res.ID)
	if !stored.CompletedAt.Valid {
		t.Error("completed_at is NULL on a finished run")
	}
	if stored.TotalAtRiskPaise.Int64 != res.TotalAtRiskPaise ||
		stored.TotalRecoveredPaise.Int64 != res.TotalRecoveredPaise ||
		stored.RecoveryRate.Float64 != res.RecoveryRate ||
		stored.BaselineRecoveredPaise.Int64 != res.BaselineRecoveredPaise ||
		stored.BaselineRecoveryRate.Float64 != res.BaselineRecoveryRate {
		t.Errorf("stored row disagrees with the returned Result:\n  stored: %+v\n  result: %+v", stored, res)
	}
	if stored.RNGSeed != 12345 || stored.BatchSize != 4 {
		t.Errorf("stored seed/size = %d/%d, want 12345/4", stored.RNGSeed, stored.BatchSize)
	}
}

// TestRunBaselineIsScoredButNotPersisted guards the boundary between the two
// strategies: the baseline is accumulated for the comparison and must never
// appear in the outcomes table, which records what happened in THIS system.
func TestRunBaselineIsScoredButNotPersisted(t *testing.T) {
	pool := liveDB(t)
	var seen []string

	stub := &stubWebhook{
		t: t, pool: pool, seen: &seen,
		amountFor: func(int) int64 { return 500000 },
		actionFor: func(int) string { return "retry_now" },
	}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	res, err := Run(context.Background(), pool, Options{Size: 6, Seed: 777, URL: srv.URL})
	cleanupRun(t, pool, res.ID, &seen)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Exactly one outcome row per scored payment, and never two.
	var rows int
	if err := pool.QueryRow(`SELECT count(*) FROM outcomes WHERE payment_id = ANY($1)`, seen).Scan(&rows); err != nil {
		t.Fatalf("count outcomes: %v", err)
	}
	if rows != 6 {
		t.Errorf("outcomes rows = %d, want 6 — one per payment, with nothing written for the baseline", rows)
	}

	// The baseline still produced a figure, so it was scored.
	if res.BaselineRecoveredPaise < 0 || res.BaselineRecoveredPaise > res.TotalAtRiskPaise {
		t.Errorf("BaselineRecoveredPaise = %d, outside 0..%d", res.BaselineRecoveredPaise, res.TotalAtRiskPaise)
	}

	// Every stored outcome must be one this system can produce. "recovered" from
	// the baseline leaking in would be indistinguishable here, which is why the
	// count check above matters more than this one.
	orows, err := pool.Query(`SELECT DISTINCT outcome FROM outcomes WHERE payment_id = ANY($1)`, seen)
	if err != nil {
		t.Fatalf("read outcomes: %v", err)
	}
	defer orows.Close()
	for orows.Next() {
		var o string
		if err := orows.Scan(&o); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if o != "recovered" && o != "still_failed" && o != "escalated_pending" {
			t.Errorf("unexpected outcome value %q", o)
		}
	}
}

// TestRunSkipsUnscorablePayments covers the three skip paths and, more
// importantly, that a skipped payment is excluded from the denominator.
//
// Counting an unscorable payment as at-risk would blame the routing for a
// payment it never ruled on, and would drag the recovery rate down for a reason
// that has nothing to do with the routing.
func TestRunSkipsUnscorablePayments(t *testing.T) {
	pool := liveDB(t)
	var seen []string

	// Index 1 answers 500, index 3 answers 200 but records nothing. Indexes 0
	// and 2 are scored normally, at 250000 paise each.
	stub := &stubWebhook{
		t: t, pool: pool, seen: &seen,
		amountFor: func(int) int64 { return 250000 },
		actionFor: func(int) string { return "retry_now" },
		statusFor: func(i int) int {
			if i == 1 {
				return http.StatusInternalServerError
			}
			return http.StatusOK
		},
		skipRecord: func(i int) bool { return i == 3 },
	}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	var skippedReasons []string
	res, err := Run(context.Background(), pool, Options{
		Size: 4, Seed: 4242, URL: srv.URL,
		Skipped: func(_ int, _, reason string) { skippedReasons = append(skippedReasons, reason) },
	})
	cleanupRun(t, pool, res.ID, &seen)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2 (one non-2xx, one with no decision)", res.Skipped)
	}
	if len(skippedReasons) != 2 {
		t.Errorf("Skipped callback fired %d times, want 2: %v", len(skippedReasons), skippedReasons)
	}

	// Only the two scored payments are in the denominator.
	if want := int64(500000); res.TotalAtRiskPaise != want {
		t.Errorf("TotalAtRiskPaise = %d, want %d — a skipped payment must not enter the denominator",
			res.TotalAtRiskPaise, want)
	}
	if got := res.Recovered + res.StillFailed + res.EscalatedPending; got != 2 {
		t.Errorf("outcome counts sum to %d, want 2", got)
	}

	// And the run still completes, with figures, despite the skips.
	stored := readRun(t, pool, res.ID)
	if !stored.CompletedAt.Valid {
		t.Error("a run with skipped payments must still complete")
	}
	if stored.BatchSize != 4 {
		t.Errorf("batch_size = %d, want 4 — it records what was asked for, not what was scored", stored.BatchSize)
	}
}

// TestRunWithNothingScorableReportsZeroRatesNotNaN is the division-by-zero
// case. Every payment failing to score leaves at-risk at 0, and 0/0 in Go is
// NaN — which would be stored as a rate and rendered on the dashboard.
func TestRunWithNothingScorableReportsZeroRatesNotNaN(t *testing.T) {
	pool := liveDB(t)
	var seen []string

	stub := &stubWebhook{
		t: t, pool: pool, seen: &seen,
		amountFor: func(int) int64 { return 1 },
		actionFor: func(int) string { return "retry_now" },
		statusFor: func(int) int { return http.StatusInternalServerError },
	}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	res, err := Run(context.Background(), pool, Options{Size: 3, Seed: 9, URL: srv.URL})
	cleanupRun(t, pool, res.ID, &seen)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Skipped != 3 || res.TotalAtRiskPaise != 0 {
		t.Fatalf("expected all 3 skipped and 0 at risk, got Skipped=%d atRisk=%d",
			res.Skipped, res.TotalAtRiskPaise)
	}
	if res.RecoveryRate != 0 || res.BaselineRecoveryRate != 0 {
		t.Errorf("rates = %v / %v, want 0 / 0 — 0/0 must not become NaN",
			res.RecoveryRate, res.BaselineRecoveryRate)
	}
	if isNaN(res.RecoveryRate) || isNaN(res.BaselineRecoveryRate) {
		t.Error("a rate is NaN; it would be stored and rendered as one")
	}

	stored := readRun(t, pool, res.ID)
	if stored.RecoveryRate.Float64 != 0 {
		t.Errorf("stored recovery_rate = %v, want 0", stored.RecoveryRate.Float64)
	}
}

func isNaN(f float64) bool { return f != f }

// TestRunLeavesIncompleteRowWhenCancelled covers the state batch_runs' nullable
// columns exist for.
//
// A run cancelled partway must leave its row with completed_at NULL — evidence
// that it was attempted — rather than vanishing or being stamped complete with
// partial figures. The read layer then reports it as incomplete, and the
// dashboard says so instead of rendering zeros.
func TestRunLeavesIncompleteRowWhenCancelled(t *testing.T) {
	pool := liveDB(t)
	var seen []string

	ctx, cancel := context.WithCancel(context.Background())

	stub := &stubWebhook{
		t: t, pool: pool, seen: &seen,
		amountFor: func(int) int64 { return 100000 },
		actionFor: func(int) string { return "retry_now" },
	}
	// Cancel after the second payment, mid-run.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.ServeHTTP(w, r)
		if stub.calls == 2 {
			cancel()
		}
	}))
	defer srv.Close()

	res, err := Run(ctx, pool, Options{Size: 10, Seed: 31337, URL: srv.URL})
	cleanupRun(t, pool, res.ID, &seen)

	if err == nil {
		t.Fatal("Run returned nil error after its context was cancelled")
	}
	if res.ID == 0 {
		t.Fatal("no batch_runs row was created, so a cancelled run left no evidence it was attempted")
	}

	stored := readRun(t, pool, res.ID)
	if stored.CompletedAt.Valid {
		t.Error("completed_at is set on a cancelled run; it must stay NULL")
	}
	if stored.TotalAtRiskPaise.Valid || stored.RecoveryRate.Valid {
		t.Error("a cancelled run has figures stored; they must stay NULL rather than being partial")
	}
	// The seed and size are still there, because they were known at insert.
	if stored.RNGSeed != 31337 || stored.BatchSize != 10 {
		t.Errorf("stored seed/size = %d/%d, want 31337/10", stored.RNGSeed, stored.BatchSize)
	}

	// And Latest must not return it: an incomplete run has nothing to show, and
	// surfacing it would blank the dashboard.
	latest, err := Latest(context.Background(), pool)
	if err == nil && latest.ID == res.ID {
		t.Error("Latest returned the incomplete run; it must return the last completed one")
	}
}

// TestRunRejectsBadSize is the guard on the one input a caller controls.
func TestRunRejectsBadSize(t *testing.T) {
	pool := liveDB(t)
	for _, size := range []int{0, -1} {
		if _, err := Run(context.Background(), pool, Options{Size: size}); err == nil {
			t.Errorf("Run accepted size %d", size)
		}
	}
}

// TestRunChoosesASeedWhenGivenNone: a seed of 0 means "pick one", and the
// chosen value must be returned and stored, or the run is not reproducible
// afterwards even though it looked like it succeeded.
func TestRunChoosesASeedWhenGivenNone(t *testing.T) {
	pool := liveDB(t)
	var seen []string

	stub := &stubWebhook{
		t: t, pool: pool, seen: &seen,
		amountFor: func(int) int64 { return 1000 },
		actionFor: func(int) string { return "escalate" },
	}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	res, err := Run(context.Background(), pool, Options{Size: 1, Seed: 0, URL: srv.URL})
	cleanupRun(t, pool, res.ID, &seen)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Seed == 0 {
		t.Fatal("Result.Seed is still 0; the chosen seed was not reported")
	}
	stored := readRun(t, pool, res.ID)
	if stored.RNGSeed != res.Seed {
		t.Errorf("stored seed %d does not match the reported seed %d", stored.RNGSeed, res.Seed)
	}
}

// TestDeriveIsStableAndSeparatesStreams pins the per-payment seed derivation.
//
// This is what stops one payment being routed differently from shifting every
// subsequent draw. It must be deterministic, and the two streams must not
// collide for the same payment — if they did, the agent's outcome and the
// baseline's would be perfectly correlated and the comparison would be
// meaningless.
func TestDeriveIsStableAndSeparatesStreams(t *testing.T) {
	const seed = 20260831

	for i := 0; i < 50; i++ {
		if a, b := derive(seed, "outcome", i), derive(seed, "outcome", i); a != b {
			t.Fatalf("derive is not deterministic at index %d: %d vs %d", i, a, b)
		}
		if a, b := derive(seed, "outcome", i), derive(seed, "baseline", i); a == b {
			t.Errorf("outcome and baseline streams collide at index %d", i)
		}
	}

	// Neighbouring indices must not produce neighbouring values, or a batch's
	// draws would be visibly patterned.
	seen := map[int64]int{}
	for i := 0; i < 200; i++ {
		v := derive(seed, "outcome", i)
		if prev, dup := seen[v]; dup {
			t.Errorf("derive collided for indices %d and %d", prev, i)
		}
		seen[v] = i
	}

	// A different batch seed must move every stream.
	if derive(seed, "outcome", 7) == derive(seed+1, "outcome", 7) {
		t.Error("changing the batch seed did not change the derived value")
	}
}
