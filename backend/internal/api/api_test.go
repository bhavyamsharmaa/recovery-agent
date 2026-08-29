package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/db"
)

// These tests run against the real database, like every other Postgres test in
// this project. The behaviour under test is what a query returns — ordering,
// filtering, the latest-decision join — and none of that is demonstrable
// against a fake: a stub would only prove that the Go code passes through
// whatever it is handed.
//
// Rows are seeded directly with INSERT rather than through the webhook handler.
// The API is a reader; what matters is that it reads a known table state
// correctly, and going through the handler would drag the model into a test
// about SQL.

// seed is one payment plus the decisions recorded against it.
type seed struct {
	paymentID string
	category  string
	method    string
	amount    int64
	reason    string
	lastSeen  time.Time
	attempts  int
	decisions []seedDecision
}

type seedDecision struct {
	attemptNumber int
	action        string
	source        string
	confidence    *float64
}

func newTestAPI(t *testing.T) (*Handler, *sql.DB, string) {
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

	// The prefix is unique per test run so a case can find its own rows in a
	// table that holds every other run's, and so cleanup deletes only its own.
	prefix := fmt.Sprintf("pay_api_%d_", time.Now().UnixNano())
	return NewHandler(pool), pool, prefix
}

func insertSeeds(t *testing.T, pool *sql.DB, prefix string, seeds []seed) {
	t.Helper()

	t.Cleanup(func() {
		// Children first: decisions references failed_payments, and the schema
		// deliberately has no ON DELETE CASCADE.
		if _, err := pool.Exec(`DELETE FROM decisions WHERE payment_id LIKE $1`, prefix+"%"); err != nil {
			t.Errorf("cleanup decisions: %v", err)
		}
		if _, err := pool.Exec(`DELETE FROM failed_payments WHERE payment_id LIKE $1`, prefix+"%"); err != nil {
			t.Errorf("cleanup failed_payments: %v", err)
		}
	})

	for _, s := range seeds {
		if _, err := pool.Exec(`
			INSERT INTO failed_payments (payment_id, category, error_code, error_reason,
			    error_source, payment_method, amount_paise, first_failed_at,
			    last_seen_at, attempt_count)
			VALUES ($1, $2, $3, $4, 'bank', $5, $6, $7, $8, $9)`,
			prefix+s.paymentID, s.category, "TEST_CODE", s.reason, s.method,
			s.amount, s.lastSeen.Add(-time.Minute), s.lastSeen, s.attempts); err != nil {
			t.Fatalf("seed payment %s: %v", s.paymentID, err)
		}
		for _, d := range s.decisions {
			if _, err := pool.Exec(`
				INSERT INTO decisions (payment_id, attempt_number, action, confidence,
				    reasoning, customer_message, source)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				prefix+s.paymentID, d.attemptNumber, d.action, d.confidence,
				"seeded for test", "we could not process your payment", d.source); err != nil {
				t.Fatalf("seed decision for %s: %v", s.paymentID, err)
			}
		}
	}
}

func get(t *testing.T, h *Handler, target string) (*httptest.ResponseRecorder, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, rec.Body.Bytes()
}

// onlySeeded drops rows belonging to other runs, so a shared development
// database with real traffic in it does not make the assertions below depend on
// what else happens to be stored.
func onlySeeded(all []paymentSummary, prefix string) []paymentSummary {
	var mine []paymentSummary
	for _, p := range all {
		if len(p.PaymentID) >= len(prefix) && p.PaymentID[:len(prefix)] == prefix {
			mine = append(mine, p)
		}
	}
	return mine
}

func decodeList(t *testing.T, body []byte) []paymentSummary {
	t.Helper()
	var out []paymentSummary
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode list: %v\nbody: %s", err, body)
	}
	return out
}

func float(f float64) *float64 { return &f }

// threeSeeds is the fixture the list tests share: three payments seen at
// distinct times, in two categories, with two different latest actions.
func threeSeeds(now time.Time) []seed {
	return []seed{
		{
			// Oldest. Two decisions, so the list has to pick the later one.
			paymentID: "old", category: "insufficient_funds", method: "card",
			amount: 499900, reason: "insufficient_funds",
			lastSeen: now.Add(-3 * time.Hour), attempts: 2,
			decisions: []seedDecision{
				{attemptNumber: 1, action: "retry", source: "llm", confidence: float(0.82)},
				{attemptNumber: 2, action: "escalate", source: "confidence_gate", confidence: float(0.68)},
			},
		},
		{
			paymentID: "mid", category: "bank_downtime", method: "upi",
			amount: 120000, reason: "gateway_error",
			lastSeen: now.Add(-2 * time.Hour), attempts: 1,
			decisions: []seedDecision{
				{attemptNumber: 1, action: "retry", source: "llm", confidence: float(0.85)},
			},
		},
		{
			// Newest, and never decided on: its latest_* fields must be null.
			paymentID: "new", category: "insufficient_funds", method: "card",
			amount: 25000, reason: "insufficient_funds",
			lastSeen: now.Add(-1 * time.Hour), attempts: 1,
		},
	}
}

func TestListPaymentsOrdersMostRecentFirst(t *testing.T) {
	h, pool, prefix := newTestAPI(t)
	now := time.Now().UTC()
	insertSeeds(t, pool, prefix, threeSeeds(now))

	rec, body := get(t, h, "/api/payments")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, body)
	}

	mine := onlySeeded(decodeList(t, body), prefix)
	if len(mine) != 3 {
		t.Fatalf("got %d seeded rows, want 3", len(mine))
	}

	want := []string{prefix + "new", prefix + "mid", prefix + "old"}
	for i, id := range want {
		if mine[i].PaymentID != id {
			t.Errorf("row %d = %s, want %s", i, mine[i].PaymentID, id)
		}
	}
}

func TestListPaymentsCarriesLatestDecision(t *testing.T) {
	h, pool, prefix := newTestAPI(t)
	now := time.Now().UTC()
	insertSeeds(t, pool, prefix, threeSeeds(now))

	_, body := get(t, h, "/api/payments")
	byID := map[string]paymentSummary{}
	for _, p := range onlySeeded(decodeList(t, body), prefix) {
		byID[p.PaymentID] = p
	}

	// The payment with two decisions must report the second, not the first.
	old := byID[prefix+"old"]
	if old.LatestAction == nil || *old.LatestAction != "escalate" {
		t.Errorf("latest_action = %v, want escalate (attempt 2, not attempt 1)", deref(old.LatestAction))
	}
	if old.LatestSource == nil || *old.LatestSource != "confidence_gate" {
		t.Errorf("latest_source = %v, want confidence_gate", deref(old.LatestSource))
	}
	if old.LatestConfidence == nil || *old.LatestConfidence != 0.68 {
		t.Errorf("latest_confidence = %v, want 0.68", old.LatestConfidence)
	}
	// The three latest_* fields must come from one row, not three aggregates.
	if old.AttemptCount != 2 || old.AmountPaise != 499900 || old.Category != "insufficient_funds" {
		t.Errorf("payment columns wrong: %+v", old)
	}

	// A payment with no decisions is still listed, with nulls rather than zeros.
	fresh := byID[prefix+"new"]
	if fresh.PaymentID == "" {
		t.Fatalf("undecided payment missing from the list entirely")
	}
	if fresh.LatestAction != nil || fresh.LatestConfidence != nil || fresh.LatestSource != nil {
		t.Errorf("undecided payment has decision fields set: %+v", fresh)
	}
}

func TestListPaymentsFilters(t *testing.T) {
	h, pool, prefix := newTestAPI(t)
	now := time.Now().UTC()
	insertSeeds(t, pool, prefix, threeSeeds(now))

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{"by category", "?category=insufficient_funds", []string{prefix + "new", prefix + "old"}},
		{"by action", "?action=escalate", []string{prefix + "old"}},
		{"combined", "?category=insufficient_funds&action=escalate", []string{prefix + "old"}},
		// An action filter excludes the undecided payment on its own, because
		// its latest action is NULL and NULL never equals a value.
		{"combined excludes undecided", "?category=insufficient_funds&action=retry", nil},
		{"no match", "?category=hard_decline", nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, body := get(t, h, "/api/payments"+c.query)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", rec.Code, body)
			}
			mine := onlySeeded(decodeList(t, body), prefix)
			if len(mine) != len(c.want) {
				t.Fatalf("got %d rows, want %d: %s", len(mine), len(c.want), body)
			}
			for i, id := range c.want {
				if mine[i].PaymentID != id {
					t.Errorf("row %d = %s, want %s", i, mine[i].PaymentID, id)
				}
			}
		})
	}
}

// An empty result must encode as [], not null: a frontend iterating the
// response should not have to special-case "no payments".
func TestListPaymentsEmptyResultIsArray(t *testing.T) {
	h, _, _ := newTestAPI(t)

	_, body := get(t, h, "/api/payments?category=no_such_category_exists")
	if string(body) != "[]\n" {
		t.Errorf("body = %q, want %q", body, "[]\n")
	}
}

func TestGetPaymentReturnsDecisionsInOrder(t *testing.T) {
	h, pool, prefix := newTestAPI(t)
	now := time.Now().UTC()
	insertSeeds(t, pool, prefix, threeSeeds(now))

	rec, body := get(t, h, "/api/payments/"+prefix+"old")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, body)
	}

	var got paymentDetail
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}

	if got.Payment.PaymentID != prefix+"old" {
		t.Errorf("payment_id = %s, want %s", got.Payment.PaymentID, prefix+"old")
	}
	if got.Payment.ErrorCode != "TEST_CODE" || got.Payment.ErrorSource != "bank" {
		t.Errorf("failure columns wrong: %+v", got.Payment)
	}

	if len(got.Decisions) != 2 {
		t.Fatalf("got %d decisions, want 2", len(got.Decisions))
	}
	if got.Decisions[0].AttemptNumber != 1 || got.Decisions[1].AttemptNumber != 2 {
		t.Errorf("decisions out of order: %d then %d",
			got.Decisions[0].AttemptNumber, got.Decisions[1].AttemptNumber)
	}
	if got.Decisions[0].Action != "retry" || got.Decisions[1].Action != "escalate" {
		t.Errorf("actions = %s, %s; want retry, escalate",
			got.Decisions[0].Action, got.Decisions[1].Action)
	}

	// Nothing writes outcomes yet, so the array must be present and empty
	// rather than absent — the shape should not change the day it fills up.
	if got.Outcomes == nil {
		t.Error("outcomes = null, want []")
	}
}

// A NULL confidence is not zero confidence: it says no model stood behind the
// decision. The two must stay distinguishable through the JSON.
func TestGetPaymentKeepsNullConfidenceNull(t *testing.T) {
	h, pool, prefix := newTestAPI(t)
	insertSeeds(t, pool, prefix, []seed{{
		paymentID: "ruled", category: "hard_decline", method: "card",
		amount: 10000, reason: "card_blocked", lastSeen: time.Now().UTC(), attempts: 1,
		decisions: []seedDecision{
			{attemptNumber: 1, action: "escalate", source: "stopping_rule", confidence: nil},
		},
	}})

	_, body := get(t, h, "/api/payments/"+prefix+"ruled")
	var got paymentDetail
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	if len(got.Decisions) != 1 {
		t.Fatalf("got %d decisions, want 1", len(got.Decisions))
	}
	if got.Decisions[0].Confidence != nil {
		t.Errorf("confidence = %v, want null", *got.Decisions[0].Confidence)
	}
}

func TestGetPaymentUnknownIDIs404(t *testing.T) {
	h, _, prefix := newTestAPI(t)

	rec, body := get(t, h, "/api/payments/"+prefix+"never_ingested")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rec.Code, body)
	}

	// The body has to be JSON too — a caller parsing every response should not
	// hit a decode error on the error path.
	var errBody map[string]string
	if err := json.Unmarshal(body, &errBody); err != nil {
		t.Fatalf("404 body is not JSON: %v\nbody: %s", err, body)
	}
	if errBody["error"] == "" {
		t.Errorf("404 body has no error field: %s", body)
	}
}

func TestCORSHeaderOnAPIRoutes(t *testing.T) {
	h, _, _ := newTestAPI(t)

	rec, _ := get(t, h, "/api/payments")
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != defaultFrontendOrigin {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, defaultFrontendOrigin)
	}

	// Preflight has to be answered by the handler itself; the mux only knows
	// GET and would answer OPTIONS with a 405 the browser reports as an opaque
	// CORS failure.
	req := httptest.NewRequest(http.MethodOptions, "/api/payments", nil)
	pre := httptest.NewRecorder()
	h.ServeHTTP(pre, req)
	if pre.Code != http.StatusNoContent {
		t.Errorf("OPTIONS status = %d, want 204", pre.Code)
	}
	if got := pre.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("preflight has no Access-Control-Allow-Methods")
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
