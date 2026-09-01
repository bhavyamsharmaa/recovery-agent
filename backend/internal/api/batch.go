package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/batch"
)

// batchRunTimeout bounds one triggered run. A batch is not a quick query: every
// payment in it makes a real model call through the real webhook path, so a
// hundred of them takes minutes. The browser's own request is held open for the
// duration, which is why the default size behind the button is small.
const batchRunTimeout = 10 * time.Minute

// defaultTriggeredBatchSize is what POST /api/batch-runs uses when the caller
// does not say. It is far below the CLI's default of 100 on purpose: this one
// runs while somebody watches a spinner, and a hundred sequential model calls is
// several minutes of that. The CLI, which nobody is watching, keeps 100.
const defaultTriggeredBatchSize = 20

// maxTriggeredBatchSize caps what the endpoint will accept. Without it, an
// unauthenticated caller — and these routes are unauthenticated — could ask for
// a million payments and spend real model budget doing it.
const maxTriggeredBatchSize = 200

// publicBaseURLEnv names the variable holding this instance's own externally
// reachable base URL, e.g. https://recovery-agent-mcyz.onrender.com.
//
// A batch posts its simulated failures to the webhook over HTTP rather than
// calling the handler in-process, deliberately: that is what makes it exercise
// the same path Razorpay hits. The consequence is that the run needs an address
// for this very server, and the server cannot work one out for itself — the
// port it binds is not necessarily the port it is reached on, and behind a
// proxy the scheme and host are not its own either.
//
// Issue #3: without this the API path fell back to batch.DefaultWebhookURL,
// which is localhost:8080. A deployed instance binds $PORT, so every payment
// failed to connect and the run stored zeros beside a real completed_at.
const publicBaseURLEnv = "PUBLIC_BASE_URL"

// webhookPath is appended to the public base URL. It matches the route
// registered in cmd/server, and the two must stay in step — a batch posting to
// a path nothing serves would 404 every payment and skip the lot.
const webhookPath = "/webhook/payment-failed"

// batchRunner serialises triggered runs.
//
// Two batches at once would interleave their webhooks through one handler and
// one attempt counter, and each would then read back decisions made under load
// the other created. The figures would still be arithmetically correct and would
// describe nothing reproducible. One at a time, and a second caller is told to
// wait rather than being queued: a queued run started minutes after the button
// was pressed is not what the person pressing it asked for.
type batchRunner struct {
	mu      sync.Mutex
	running bool
}

func (r *batchRunner) acquire() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return false
	}
	r.running = true
	return true
}

func (r *batchRunner) release() {
	r.mu.Lock()
	r.running = false
	r.mu.Unlock()
}

// batchSkippedLog records one payment a batch could not score.
//
// These are the lines issue #3 discarded. The HTTP path collected the reasons
// through Options.Skipped and passed no callback, so a hundred identical
// "connection refused" messages naming the exact cause were dropped while the
// run reported success. The URL is on every line because it is the field that
// was wrong.
//
// Shaped like ingest's log structs — an event name, the facts, a timestamp —
// so one JSON log stream covers the whole pipeline.
type batchSkippedLog struct {
	Event     string `json:"event"`
	Index     int    `json:"index"`
	Size      int    `json:"size"`
	PaymentID string `json:"payment_id"`
	URL       string `json:"url"`
	Reason    string `json:"reason"`
	TS        string `json:"ts"`
}

// batchPartialLog records a completed run that scored fewer payments than it
// set out to. The figures it stored are real but describe a smaller batch than
// batch_size claims, and skipped_count on the row is the durable version of
// this line.
type batchPartialLog struct {
	Event   string `json:"event"`
	RunID   int64  `json:"run_id"`
	Skipped int    `json:"skipped"`
	Scored  int    `json:"scored"`
	Size    int    `json:"size"`
	TS      string `json:"ts"`
}

// logOut is where this package's structured log lines go. A package var rather
// than os.Stdout inline so tests can capture them; production never reassigns
// it. Mirrors ingest.logOut.
var logOut io.Writer = os.Stdout

func logLine(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not marshal log line: %v\n", err)
		return
	}
	fmt.Fprintln(logOut, string(b))
}

// batchRunJSON is one batch_runs row on the wire.
//
// The result fields are pointers because the columns are nullable: a run that
// started and never finished has no figures, and that is a different statement
// from a run that completed having recovered nothing. Sending 0 for both would
// make them indistinguishable in the UI.
type batchRunJSON struct {
	ID          int64     `json:"id"`
	StartedAt   jsonTime  `json:"started_at"`
	CompletedAt *jsonTime `json:"completed_at"`
	BatchSize   int       `json:"batch_size"`
	RNGSeed     int64     `json:"rng_seed"`

	TotalAtRiskPaise       *int64   `json:"total_at_risk_paise"`
	TotalRecoveredPaise    *int64   `json:"total_recovered_paise"`
	RecoveryRate           *float64 `json:"recovery_rate"`
	BaselineRecoveredPaise *int64   `json:"baseline_recovered_paise"`
	BaselineRecoveryRate   *float64 `json:"baseline_recovery_rate"`

	// FallbackDecisions counts payments in this run whose decision came from the
	// fallback — the model call and its retry both failed, so no decision was
	// formed at all. Those payments never auto-resolve, so they depress the
	// recovery rate for a reason that is an infrastructure outage rather than a
	// policy choice. Null when the run never completed; 0 is a real answer
	// meaning every payment reached the model.
	FallbackDecisions *int `json:"fallback_decisions"`

	// SkippedCount is how many payments could not be scored at all. A non-zero
	// value means the figures below describe fewer payments than batch_size,
	// and a consumer showing those figures should say so — the dashboard warning
	// is not built yet, and this field is what it will read.
	//
	// Null for a run that never completed and for any run predating migration
	// 006, whose skip count was never recorded; 0 means every payment scored.
	SkippedCount *int `json:"skipped_count"`

	// ImprovementPoints is recovery_rate - baseline_recovery_rate, in percentage
	// points, computed here rather than in the browser. It is the headline
	// number of the whole feature, and two clients deriving it independently is
	// two chances to derive it differently.
	ImprovementPoints *float64 `json:"improvement_points"`
}

func toBatchRunJSON(r batch.StoredRun) batchRunJSON {
	out := batchRunJSON{
		ID:                     r.ID,
		StartedAt:              jsonTime(r.StartedAt),
		BatchSize:              r.BatchSize,
		RNGSeed:                r.RNGSeed,
		TotalAtRiskPaise:       nullInt(r.TotalAtRiskPaise),
		TotalRecoveredPaise:    nullInt(r.TotalRecoveredPaise),
		RecoveryRate:           nullFloat(r.RecoveryRate),
		BaselineRecoveredPaise: nullInt(r.BaselineRecoveredPaise),
		BaselineRecoveryRate:   nullFloat(r.BaselineRecoveryRate),
		FallbackDecisions:      nullIntAsInt(r.FallbackDecisions),
		SkippedCount:           nullIntAsInt(r.SkippedCount),
	}
	if r.CompletedAt.Valid {
		t := jsonTime(r.CompletedAt.Time)
		out.CompletedAt = &t
	}
	// Only when both halves are present. An improvement derived from one known
	// rate and one missing one would be a number with no meaning.
	if r.RecoveryRate.Valid && r.BaselineRecoveryRate.Valid {
		p := (r.RecoveryRate.Float64 - r.BaselineRecoveryRate.Float64) * 100
		out.ImprovementPoints = &p
	}
	return out
}

// latestBatchRun serves GET /api/batch-runs/latest.
func (h *Handler) latestBatchRun(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r)
	defer cancel()

	run, err := batch.Latest(ctx, h.db)

	// No runs yet is a 404 with a body, not an empty 200. The dashboard has to
	// tell "nobody has run a batch" apart from "a batch ran and recovered
	// nothing", and an empty success answer cannot carry that difference.
	if errors.Is(err, batch.ErrNoRuns) {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "no batch runs recorded yet",
		})
		return
	}
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, "could not read the latest batch run", err)
		return
	}
	writeJSON(w, http.StatusOK, toBatchRunJSON(run))
}

// listBatchRuns serves GET /api/batch-runs, most recent first.
func (h *Handler) listBatchRuns(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r)
	defer cancel()

	runs, err := batch.List(ctx, h.db, 50)
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, "could not read batch runs", err)
		return
	}

	// Non-nil slice so an empty history encodes as [] rather than null.
	out := make([]batchRunJSON, 0, len(runs))
	for _, run := range runs {
		out = append(out, toBatchRunJSON(run))
	}
	writeJSON(w, http.StatusOK, out)
}

// triggerBatchRun serves POST /api/batch-runs.
//
// This endpoint writes outcomes and batch_runs rows and spends real model
// budget. It is behind the shared-secret gate in auth.go along with every other
// route here, and the size cap and single-run lock below bound what one
// authorised caller can spend in a single request.
//
// It also needs PUBLIC_BASE_URL, because a batch posts to this instance's own
// webhook over HTTP. See publicBaseURLEnv.
func (h *Handler) triggerBatchRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Size int   `json:"size"`
		Seed int64 `json:"seed"`
	}
	// An empty body is fine and means "use the defaults", which is what the
	// dashboard button sends.
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if req.Size == 0 {
		req.Size = defaultTriggeredBatchSize
	}
	if req.Size < 1 || req.Size > maxTriggeredBatchSize {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "size must be between 1 and 200",
		})
		return
	}

	if !h.batch.acquire() {
		// 409 rather than 429: this is not rate limiting, it is a conflict with
		// a run already in progress, and the caller should wait for that one
		// rather than back off and retry.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "a batch run is already in progress",
		})
		return
	}
	defer h.batch.release()

	// Deliberately NOT derived from the request's context. A batch writes
	// outcomes rows as it goes, and a browser tab closed halfway would otherwise
	// abandon the run with its batch_runs row left incomplete forever. The run
	// finishes and records its result even if nobody is listening.
	ctx, cancel := context.WithTimeout(context.Background(), batchRunTimeout)
	defer cancel()

	// Configuration is checked here rather than at the top of the function so a
	// misconfigured instance still serves every read route. A 503 names the
	// variable: the alternative is a run that posts nowhere, which is exactly
	// what issue #3 was.
	if h.webhookURL == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "batch runs are not configured: " + publicBaseURLEnv + " is unset",
		})
		return
	}

	res, err := batch.Run(ctx, h.db, batch.Options{
		Size: req.Size,
		Seed: req.Seed,

		// The instance's own address, not the package default. This one line is
		// the fix for issue #3.
		URL: h.webhookURL,

		// Every skip reason, as a structured line on stdout with the rest of the
		// pipeline's logs. The CLI wires this to stderr and that is how the bug
		// stayed invisible here: the HTTP path collected the reasons and dropped
		// them, so 100 identical "connection refused" messages — which named the
		// cause outright — were never written anywhere.
		// No run id on this line: the callback fires during the call that
		// produces it, so it is not available yet. The payment id and the
		// timestamp locate the line, and the partial/failure line below carries
		// the id once there is one.
		Skipped: func(i int, paymentID, reason string) {
			logLine(batchSkippedLog{
				Event:     "batch_payment_skipped",
				Index:     i,
				Size:      req.Size,
				PaymentID: paymentID,
				URL:       h.webhookURL,
				Reason:    reason,
				TS:        time.Now().UTC().Format(time.RFC3339),
			})
		},
	})

	// A run where nothing could be delivered is a failure, not a result. The
	// batch_runs row keeps its NULL completed_at and the caller gets an error
	// naming the URL — rather than a 200 carrying zeros, which is how this
	// looked for three production runs before issue #3 was found.
	if errors.Is(err, batch.ErrAllSkipped) {
		h.fail(w, r, http.StatusBadGateway, "batch run reached no payments: "+err.Error(), err)
		return
	}
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, "batch run failed", err)
		return
	}

	// A partially skipped run still has real figures over what it did score, so
	// it is returned rather than failed — but it is logged, because a run that
	// silently scored fewer payments than it claims is the shape of the next
	// bug like #3. skipped_count on the row is the durable record; this is the
	// line that gets noticed.
	if res.Skipped > 0 {
		logLine(batchPartialLog{
			Event:   "batch_run_partially_skipped",
			RunID:   res.ID,
			Skipped: res.Skipped,
			Size:    res.Size,
			Scored:  res.Size - res.Skipped,
			TS:      time.Now().UTC().Format(time.RFC3339),
		})
	}

	// Read the row back rather than reshaping the in-memory result, so what the
	// caller receives is what was actually stored — including the timestamps the
	// database generated.
	run, err := batch.Latest(ctx, h.db)
	if err != nil || run.ID != res.ID {
		// The run itself succeeded; only the read-back did not. Answering with
		// the in-memory figures is honest here — they are the same numbers that
		// were just written.
		writeJSON(w, http.StatusOK, batchRunJSON{
			ID:                     res.ID,
			StartedAt:              jsonTime(time.Now().UTC()),
			BatchSize:              res.Size,
			RNGSeed:                res.Seed,
			TotalAtRiskPaise:       &res.TotalAtRiskPaise,
			TotalRecoveredPaise:    &res.TotalRecoveredPaise,
			RecoveryRate:           &res.RecoveryRate,
			BaselineRecoveredPaise: &res.BaselineRecoveredPaise,
			BaselineRecoveryRate:   &res.BaselineRecoveryRate,
			FallbackDecisions:      &res.FallbackDecisions,
		})
		return
	}
	writeJSON(w, http.StatusOK, toBatchRunJSON(run))
}

// nullIntAsInt narrows a nullable BIGINT to *int for counts, which are small
// and read more naturally as int in JSON consumers.
func nullIntAsInt(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

// nullInt mirrors nullString and nullFloat in api.go: a NULL column becomes a
// JSON null rather than a zero, so "this run never finished" stays
// distinguishable from "this run recovered nothing".
func nullInt(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}
