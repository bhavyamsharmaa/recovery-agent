package batch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNoRuns reports that no batch has ever been run. It is an ordinary answer,
// not a failure — a fresh database has none — so callers distinguish it from a
// query error rather than treating an empty read as broken.
var ErrNoRuns = errors.New("batch: no runs recorded")

// StoredRun is one batch_runs row as read back from the database. It is named
// apart from Run, the function that executes a batch, because the two are
// different things: one is a record, the other is an action.
//
// Every result column is nullable, because a row is written when a run starts
// and filled in when it finishes. A run that crashed leaves them NULL, which is
// a real state meaning "started and never completed" — distinct from a run that
// completed having recovered nothing. Flattening both to zero here would lose
// that difference before any caller could act on it.
type StoredRun struct {
	ID          int64
	StartedAt   time.Time
	CompletedAt sql.NullTime
	BatchSize   int
	RNGSeed     int64

	TotalAtRiskPaise       sql.NullInt64
	TotalRecoveredPaise    sql.NullInt64
	RecoveryRate           sql.NullFloat64
	BaselineRecoveredPaise sql.NullInt64
	BaselineRecoveryRate   sql.NullFloat64

	// FallbackDecisions is NULL for a run that never completed, and 0 for a run
	// that completed with every payment reaching the model. Those are different
	// statements and stay distinguishable.
	FallbackDecisions sql.NullInt64
}

const runColumns = `
	id, started_at, completed_at, batch_size, rng_seed,
	total_at_risk_paise, total_recovered_paise, recovery_rate,
	baseline_recovered_paise, baseline_recovery_rate, fallback_decisions`

func scanRun(s interface{ Scan(...any) error }) (StoredRun, error) {
	var r StoredRun
	err := s.Scan(&r.ID, &r.StartedAt, &r.CompletedAt, &r.BatchSize, &r.RNGSeed,
		&r.TotalAtRiskPaise, &r.TotalRecoveredPaise, &r.RecoveryRate,
		&r.BaselineRecoveredPaise, &r.BaselineRecoveryRate, &r.FallbackDecisions)
	return r, err
}

// Latest returns the most recently completed run, or ErrNoRuns.
//
// Completed, not merely most recent: a run in progress has no figures yet, and
// returning it would blank the dashboard every time somebody pressed the button.
// The summary should keep showing the last real result until a new one exists.
func Latest(ctx context.Context, db *sql.DB) (StoredRun, error) {
	r, err := scanRun(db.QueryRowContext(ctx, `
		SELECT`+runColumns+`
		FROM batch_runs
		WHERE completed_at IS NOT NULL
		ORDER BY completed_at DESC, id DESC
		LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return StoredRun{}, ErrNoRuns
	}
	if err != nil {
		return StoredRun{}, fmt.Errorf("batch: read latest run: %w", err)
	}
	return r, nil
}

// List returns runs most recent first, including ones that never completed —
// an abandoned run is part of the history and hiding it would make the record
// look tidier than it is.
//
// started_at alone is not a unique ordering, so id breaks the tie and the page
// order stays stable between identical requests.
func List(ctx context.Context, db *sql.DB, limit int) ([]StoredRun, error) {
	if limit < 1 {
		limit = 50
	}
	rows, err := db.QueryContext(ctx, `
		SELECT`+runColumns+`
		FROM batch_runs
		ORDER BY started_at DESC, id DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("batch: list runs: %w", err)
	}
	defer rows.Close()

	var all []StoredRun
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("batch: scan run: %w", err)
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("batch: iterate runs: %w", err)
	}
	return all, nil
}
