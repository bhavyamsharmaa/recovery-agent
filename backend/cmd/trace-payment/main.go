// Command trace-payment reconstructs one payment's whole story from the
// database alone — what failed, every decision made about it in order, and any
// recorded outcome.
//
// Nothing here reads a log file. That is the point: if the story is not legible
// from the tables, the tables are not recording enough, and no amount of stdout
// will help six months from now.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/db"
)

const queryTimeout = 30 * time.Second

func main() {
	paymentID := flag.String("payment-id", "", "the payment to trace")
	flag.Parse()

	if *paymentID == "" {
		fmt.Fprintln(os.Stderr, "--payment-id is required")
		flag.Usage()
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	pool, err := db.Connect(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := trace(ctx, pool, *paymentID); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func trace(ctx context.Context, pool *sql.DB, paymentID string) error {
	if err := printPayment(ctx, pool, paymentID); err != nil {
		return err
	}
	fmt.Println()
	if err := printDecisions(ctx, pool, paymentID); err != nil {
		return err
	}
	fmt.Println()
	return printOutcomes(ctx, pool, paymentID)
}

func printPayment(ctx context.Context, pool *sql.DB, paymentID string) error {
	var (
		category, code, reason, source, method string
		amount                                 int64
		attempts                               int
		first, last                            time.Time
	)
	err := pool.QueryRowContext(ctx, `
		SELECT category, error_code, error_reason, error_source, payment_method,
		       amount_paise, attempt_count, first_failed_at, last_seen_at
		FROM failed_payments WHERE payment_id = $1`, paymentID).
		Scan(&category, &code, &reason, &source, &method, &amount, &attempts, &first, &last)

	// A payment id with no row is the ordinary "never seen" answer, not a
	// failure of this tool, so it says so plainly and exits clean.
	if err == sql.ErrNoRows {
		fmt.Printf("Payment: %s\n  no record — this payment has never been ingested\n", paymentID)
		os.Exit(0)
	}
	if err != nil {
		return fmt.Errorf("read payment: %w", err)
	}

	fmt.Printf("Payment: %s\n", paymentID)
	fmt.Printf("Category: %s | Amount: %s | Method: %s\n",
		orUnrecorded(category), rupees(amount), orUnrecorded(method))
	fmt.Printf("Failure:  %s / %s (source: %s)\n",
		orUnrecorded(code), orUnrecorded(reason), orUnrecorded(source))
	fmt.Printf("First failed: %s | Last seen: %s | Attempts: %d\n",
		utc(first), utc(last), attempts)
	return nil
}

func printDecisions(ctx context.Context, pool *sql.DB, paymentID string) error {
	rows, err := pool.QueryContext(ctx, `
		SELECT attempt_number, source, action, confidence, escalation_reason,
		       original_action, alternate_method, customer_message, created_at
		FROM decisions WHERE payment_id = $1 ORDER BY id`, paymentID)
	if err != nil {
		return fmt.Errorf("read decisions: %w", err)
	}
	defer rows.Close()

	type decision struct {
		attempt                                           int
		source, action                                    string
		confidence                                        sql.NullFloat64
		escalationReason, originalAction, alternateMethod sql.NullString
		customerMessage                                   sql.NullString
		createdAt                                         time.Time
	}

	var all []decision
	for rows.Next() {
		var d decision
		if err := rows.Scan(&d.attempt, &d.source, &d.action, &d.confidence,
			&d.escalationReason, &d.originalAction, &d.alternateMethod,
			&d.customerMessage, &d.createdAt); err != nil {
			return fmt.Errorf("scan decision: %w", err)
		}
		all = append(all, d)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate decisions: %w", err)
	}

	if len(all) == 0 {
		fmt.Println("Decision history: none recorded")
		return nil
	}

	// Column widths are computed from the rows actually present rather than
	// fixed, so one long source name does not leave every other line ragged.
	sourceWidth, actionWidth := 0, 0
	for _, d := range all {
		sourceWidth = max(sourceWidth, len(d.source))
		actionWidth = max(actionWidth, len(d.action))
	}

	fmt.Println("Decision history:")
	for _, d := range all {
		fmt.Printf("  #%d [%-*s] action=%-*s confidence=%-4s",
			d.attempt, sourceWidth, d.source, actionWidth, d.action, confidence(d.confidence))

		// Only shown when set. A NULL here is not missing data — it is the
		// decision saying this did not apply to it.
		if d.escalationReason.Valid {
			fmt.Printf(" reason=%s", d.escalationReason.String)
		}
		if d.originalAction.Valid {
			fmt.Printf(" overrode=%s", d.originalAction.String)
		}
		if d.alternateMethod.Valid {
			fmt.Printf(" alternate=%s", d.alternateMethod.String)
		}
		fmt.Printf("  (%s)\n", utc(d.createdAt))

		if d.customerMessage.Valid {
			fmt.Printf("      told the customer: %s\n", d.customerMessage.String)
		}
	}
	return nil
}

func printOutcomes(ctx context.Context, pool *sql.DB, paymentID string) error {
	rows, err := pool.QueryContext(ctx, `
		SELECT outcome, decision_id, recorded_at
		FROM outcomes WHERE payment_id = $1 ORDER BY id`, paymentID)
	if err != nil {
		return fmt.Errorf("read outcomes: %w", err)
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		var outcome string
		var decisionID sql.NullInt64
		var at time.Time
		if err := rows.Scan(&outcome, &decisionID, &at); err != nil {
			return fmt.Errorf("scan outcome: %w", err)
		}
		if n == 0 {
			fmt.Println("Outcomes:")
		}
		n++
		against := "unlinked"
		if decisionID.Valid {
			against = fmt.Sprintf("decision id %d", decisionID.Int64)
		}
		fmt.Printf("  %s (%s) — %s\n", outcome, against, utc(at))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate outcomes: %w", err)
	}

	if n == 0 {
		// Nothing feeds this table yet: no part of the system learns whether a
		// recovery actually worked. Saying so is more useful than an empty
		// heading that looks like a query returned nothing.
		fmt.Println("Outcomes: none recorded yet")
	}
	return nil
}

// rupees renders paise as currency. The database stores paise because that is
// what Razorpay sends and integers do not drift; a human reading a trace wants
// rupees.
func rupees(paise int64) string {
	return fmt.Sprintf("₹%d.%02d", paise/100, paise%100)
}

// orUnrecorded marks a column that was never written, so a blank in the output
// is never mistaken for a value that happens to be empty.
func orUnrecorded(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(unrecorded)"
	}
	return s
}

// confidence prints NULL rather than 0, because the two mean different things:
// a stopping-rule or fallback decision had no model behind it at all.
func confidence(c sql.NullFloat64) string {
	if !c.Valid {
		return "NULL"
	}
	return fmt.Sprintf("%.2f", c.Float64)
}

func utc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
