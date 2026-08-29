// Command trace-payment reconstructs one payment's whole story from the
// database alone — what failed, every decision made about it in order, and any
// recorded outcome.
//
// Nothing here reads a log file. That is the point: if the story is not legible
// from the tables, the tables are not recording enough, and no amount of stdout
// will help six months from now.
//
// The queries moved to internal/trace when the JSON API needed the same rows.
// What is left here is the formatting, which is all this command ever really
// was.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/db"
	"github.com/bhavyamsharmaa/recovery-agent/internal/trace"
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

	if err := run(ctx, pool, *paymentID); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, pool *sql.DB, paymentID string) error {
	full, err := trace.Load(ctx, pool, paymentID)

	// A payment id with no row is the ordinary "never seen" answer, not a
	// failure of this tool, so it says so plainly and exits clean.
	if errors.Is(err, trace.ErrNotFound) {
		fmt.Printf("Payment: %s\n  no record — this payment has never been ingested\n", paymentID)
		return nil
	}
	if err != nil {
		return err
	}

	printPayment(full.Payment)
	fmt.Println()
	printDecisions(full.Decisions)
	fmt.Println()
	printOutcomes(full.Outcomes)
	return nil
}

func printPayment(p trace.Payment) {
	fmt.Printf("Payment: %s\n", p.PaymentID)
	fmt.Printf("Category: %s | Amount: %s | Method: %s\n",
		orUnrecorded(p.Category), rupees(p.AmountPaise), orUnrecorded(p.PaymentMethod))
	fmt.Printf("Failure:  %s / %s (source: %s)\n",
		orUnrecorded(p.ErrorCode), orUnrecorded(p.ErrorReason), orUnrecorded(p.ErrorSource))
	fmt.Printf("First failed: %s | Last seen: %s | Attempts: %d\n",
		utc(p.FirstFailedAt), utc(p.LastSeenAt), p.AttemptCount)
}

func printDecisions(all []trace.Decision) {
	if len(all) == 0 {
		fmt.Println("Decision history: none recorded")
		return
	}

	// Column widths are computed from the rows actually present rather than
	// fixed, so one long source name does not leave every other line ragged.
	sourceWidth, actionWidth := 0, 0
	for _, d := range all {
		sourceWidth = max(sourceWidth, len(d.Source))
		actionWidth = max(actionWidth, len(d.Action))
	}

	fmt.Println("Decision history:")
	for _, d := range all {
		fmt.Printf("  #%d [%-*s] action=%-*s confidence=%-4s",
			d.AttemptNumber, sourceWidth, d.Source, actionWidth, d.Action, confidence(d.Confidence))

		// Only shown when set. A NULL here is not missing data — it is the
		// decision saying this did not apply to it.
		if d.EscalationReason.Valid {
			fmt.Printf(" reason=%s", d.EscalationReason.String)
		}
		if d.OriginalAction.Valid {
			fmt.Printf(" overrode=%s", d.OriginalAction.String)
		}
		if d.AlternateMethod.Valid {
			fmt.Printf(" alternate=%s", d.AlternateMethod.String)
		}
		fmt.Printf("  (%s)\n", utc(d.CreatedAt))

		if d.CustomerMessage.Valid {
			fmt.Printf("      told the customer: %s\n", d.CustomerMessage.String)
		}
	}
}

func printOutcomes(all []trace.Outcome) {
	if len(all) == 0 {
		// Nothing feeds this table yet: no part of the system learns whether a
		// recovery actually worked. Saying so is more useful than an empty
		// heading that looks like a query returned nothing.
		fmt.Println("Outcomes: none recorded yet")
		return
	}

	fmt.Println("Outcomes:")
	for _, o := range all {
		against := "unlinked"
		if o.DecisionID.Valid {
			against = fmt.Sprintf("decision id %d", o.DecisionID.Int64)
		}
		fmt.Printf("  %s (%s) — %s\n", o.Outcome, against, utc(o.RecordedAt))
	}
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
