package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/db"
)

// The batch-run and escalation routes, in the same live-database style as
// api_test.go: seeded rows, assertions against what the endpoint returns, and
// cleanup that deletes children before parents because the schema deliberately
// has no ON DELETE CASCADE.

// liveDB opens the real database, skipping unless RECOVERY_LIVE_TESTS=1. Shared
// by every test in this package that needs one.
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

func getJSON(t *testing.T, h *Handler, target string) (*httptest.ResponseRecorder, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, rec.Body.Bytes()
}

// ---------------------------------------------------------------- batch runs

// seedBatchRun inserts one batch_runs row. completed decides whether it carries
// figures, so the incomplete-run path can be exercised.
func seedBatchRun(t *testing.T, pool *sql.DB, seed int64, completed bool) int64 {
	t.Helper()

	var id int64
	var err error
	if completed {
		err = pool.QueryRow(`
			INSERT INTO batch_runs (batch_size, rng_seed, completed_at,
			    total_at_risk_paise, total_recovered_paise, recovery_rate,
			    baseline_recovered_paise, baseline_recovery_rate)
			VALUES (10, $1, now(), 1000000, 400000, 0.4, 200000, 0.2)
			RETURNING id`, seed).Scan(&id)
	} else {
		err = pool.QueryRow(`
			INSERT INTO batch_runs (batch_size, rng_seed) VALUES (10, $1) RETURNING id`, seed).Scan(&id)
	}
	if err != nil {
		t.Fatalf("seed batch run: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(`DELETE FROM batch_runs WHERE id = $1`, id); err != nil {
			t.Errorf("cleanup batch_runs: %v", err)
		}
	})
	return id
}

func TestGetLatestBatchRunReturnsTheMostRecentCompleted(t *testing.T) {
	pool := liveDB(t)
	h := NewHandler(pool)

	seed := time.Now().UnixNano()
	id := seedBatchRun(t, pool, seed, true)

	rec, body := getJSON(t, h, "/api/batch-runs/latest")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, body)
	}

	var got batchRunJSON
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	if got.ID != id {
		t.Errorf("id = %d, want %d (the run just seeded)", got.ID, id)
	}
	if got.CompletedAt == nil {
		t.Error("completed_at is null on a run selected for being complete")
	}
	// improvement_points is derived server-side: 0.4 - 0.2 = 0.2 -> 20 points.
	if got.ImprovementPoints == nil {
		t.Fatal("improvement_points is null")
	}
	if diff := *got.ImprovementPoints - 20; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("improvement_points = %v, want 20", *got.ImprovementPoints)
	}
}

// TestGetLatestBatchRunIgnoresIncompleteRuns is why Latest filters on
// completed_at: a run in progress has no figures, and returning it would blank
// the dashboard every time somebody pressed the button.
func TestGetLatestBatchRunIgnoresIncompleteRuns(t *testing.T) {
	pool := liveDB(t)
	h := NewHandler(pool)

	completed := seedBatchRun(t, pool, time.Now().UnixNano(), true)
	// Inserted after, so it is the most recent row but not the most recent
	// completed one.
	incomplete := seedBatchRun(t, pool, time.Now().UnixNano()+1, false)

	_, body := getJSON(t, h, "/api/batch-runs/latest")
	var got batchRunJSON
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID == incomplete {
		t.Error("latest returned the incomplete run; it must return the last completed one")
	}
	if got.ID != completed {
		t.Errorf("id = %d, want %d", got.ID, completed)
	}
}

// TestListBatchRunsIncludesIncompleteOnes is the opposite rule for the history:
// an abandoned run is part of the record, and hiding it would make the history
// look tidier than it is. Its figures must be null rather than zero.
func TestListBatchRunsIncludesIncompleteOnes(t *testing.T) {
	pool := liveDB(t)
	h := NewHandler(pool)

	incomplete := seedBatchRun(t, pool, time.Now().UnixNano(), false)

	rec, body := getJSON(t, h, "/api/batch-runs")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, body)
	}

	var runs []batchRunJSON
	if err := json.Unmarshal(body, &runs); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	if len(runs) == 0 {
		t.Fatal("history is empty")
	}

	// Most recent first.
	for i := 1; i < len(runs); i++ {
		if runs[i-1].StartedAt.before(runs[i].StartedAt) {
			t.Errorf("history is not ordered most recent first at index %d", i)
			break
		}
	}

	var found *batchRunJSON
	for i := range runs {
		if runs[i].ID == incomplete {
			found = &runs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("the incomplete run %d is missing from the history", incomplete)
	}
	if found.CompletedAt != nil {
		t.Error("completed_at is set on the incomplete run")
	}
	// Null, not zero: "never finished" and "finished having recovered nothing"
	// are different statements.
	if found.TotalAtRiskPaise != nil || found.RecoveryRate != nil || found.ImprovementPoints != nil {
		t.Errorf("incomplete run carries figures instead of nulls: %+v", found)
	}
}

// TestTriggerBatchRunRejectsBadSizes covers the cap that stands between an
// unauthenticated endpoint and an unbounded model bill.
func TestTriggerBatchRunRejectsBadSizes(t *testing.T) {
	pool := liveDB(t)
	h := NewHandler(pool)

	for _, size := range []int{-1, 201, 100000} {
		req := httptest.NewRequest(http.MethodPost, "/api/batch-runs",
			strings.NewReader(fmt.Sprintf(`{"size":%d}`, size)))
		req.Header.Set("content-type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("size %d: status = %d, want 400", size, rec.Code)
		}
	}
}

// TestTriggerBatchRunRunsAndStoresARun drives the POST end to end, through the
// real webhook handler, and checks the row it left behind.
//
// Size 2 deliberately: this makes real model calls, and the endpoint's own
// default of 20 would make the suite slow for no extra coverage.
func TestTriggerBatchRunRunsAndStoresARun(t *testing.T) {
	h, _ := newSimulateHandler(t)
	cleanupSimulated(t, h)

	req := httptest.NewRequest(http.MethodPost, "/api/batch-runs", strings.NewReader(`{"size":2,"seed":987654}`))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	var got batchRunJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}
	t.Cleanup(func() {
		if _, err := h.db.Exec(`DELETE FROM batch_runs WHERE id = $1`, got.ID); err != nil {
			t.Errorf("cleanup batch_runs: %v", err)
		}
	})

	if got.BatchSize != 2 {
		t.Errorf("batch_size = %d, want 2", got.BatchSize)
	}
	if got.RNGSeed != 987654 {
		t.Errorf("rng_seed = %d, want 987654 — the seed must be stored or the run is not reproducible",
			got.RNGSeed)
	}
	if got.CompletedAt == nil {
		t.Error("completed_at is null on a run that returned successfully")
	}
	if got.TotalAtRiskPaise == nil || *got.TotalAtRiskPaise <= 0 {
		t.Errorf("total_at_risk_paise = %v, want a positive figure", got.TotalAtRiskPaise)
	}
	if got.ImprovementPoints == nil {
		t.Error("improvement_points is null on a completed run")
	}
}

// --------------------------------------------------------------- escalations

func TestListEscalationsReturnsOnlyStoppingActions(t *testing.T) {
	pool := liveDB(t)
	h := NewHandler(pool)

	rec, body := getJSON(t, h, "/api/escalations")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, body)
	}

	var rows []escalationJSON
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, e := range rows {
		if e.Action != "escalate" && e.Action != "no_retry" {
			t.Errorf("payment %s has action %q; the queue must contain only stopping actions",
				e.PaymentID, e.Action)
		}
	}

	// Most recently decided first.
	for i := 1; i < len(rows); i++ {
		if rows[i-1].DecidedAt.before(rows[i].DecidedAt) {
			t.Errorf("queue is not ordered most recent first at index %d", i)
			break
		}
	}
}

// TestListEscalationsMatchesTheVerificationQuery cross-checks the endpoint
// against the MAX(id) formulation, which is the query a reviewer would write by
// hand. The two pick the latest decision differently — DISTINCT ON by
// (attempt_number, id) versus MAX(id) — so agreement is worth asserting rather
// than assuming.
func TestListEscalationsMatchesTheVerificationQuery(t *testing.T) {
	pool := liveDB(t)
	h := NewHandler(pool)

	var want int
	if err := pool.QueryRow(`
		SELECT COUNT(*) FROM decisions d
		WHERE d.action IN ('escalate', 'no_retry')
		  AND d.id = (SELECT MAX(id) FROM decisions WHERE payment_id = d.payment_id)`).Scan(&want); err != nil {
		t.Fatalf("verification query: %v", err)
	}

	_, body := getJSON(t, h, "/api/escalations")
	var rows []escalationJSON
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(rows) != want {
		t.Errorf("endpoint returned %d rows, MAX(id) verification query says %d — "+
			"the two ways of picking the latest decision disagree", len(rows), want)
	}
}

// TestListEscalationsKeepsNullsDistinct is the null-versus-zero rule at this
// endpoint. A fallback case has no escalation_reason — the system could not
// reason at all, so there is no policy reason to name — and a stopping-rule case
// has no confidence.
func TestListEscalationsKeepsNullsDistinct(t *testing.T) {
	pool := liveDB(t)
	h := NewHandler(pool)

	_, body := getJSON(t, h, "/api/escalations")
	var rows []escalationJSON
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var sawFallback, sawStoppingRule bool
	for _, e := range rows {
		switch e.Source {
		case "fallback_rule":
			sawFallback = true
			if e.EscalationReason != nil {
				t.Errorf("fallback case %s has escalation_reason %q; it must be null",
					e.PaymentID, *e.EscalationReason)
			}
			if e.Confidence != nil {
				t.Errorf("fallback case %s has a confidence; it must be null", e.PaymentID)
			}
		case "stopping_rule":
			sawStoppingRule = true
			if e.Confidence != nil {
				t.Errorf("stopping-rule case %s has confidence %v; it must be null — "+
					"the rule fired before the model was asked", e.PaymentID, *e.Confidence)
			}
		}
	}

	// Not fatal: a database with no such cases is legitimate. But saying so
	// beats a silent pass that proves nothing.
	if !sawFallback {
		t.Log("no fallback_rule cases present, so the null-escalation_reason branch was not exercised")
	}
	if !sawStoppingRule {
		t.Log("no stopping_rule cases present, so the null-confidence branch was not exercised")
	}
}

// before reports whether one rendered timestamp sorts earlier than another.
// jsonTime marshals to RFC 3339 in UTC, which sorts lexicographically.
func (t jsonTime) before(other jsonTime) bool {
	return time.Time(t).Before(time.Time(other))
}
