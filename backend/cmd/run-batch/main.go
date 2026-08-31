// Command run-batch fires N simulated failures through the real pipeline and
// scores what the agent's routing recovered against a blind retry baseline.
//
// The run itself lives in internal/batch, because the dashboard's "Run new
// batch" button needs the same code. A batch triggered from a browser and one
// triggered from this command must be the same batch, or the figures on screen
// describe something this command cannot reproduce. What is left here is the
// command-line surface and the printing.
//
// See internal/batch for the simulation boundary and the reproducibility rules.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/batch"
	"github.com/bhavyamsharmaa/recovery-agent/internal/db"
)

const connectTimeout = 30 * time.Second

func main() {
	size := flag.Int("size", 100, "number of simulated failures to run through the pipeline")
	seed := flag.Int64("seed", 0, "RNG seed; 0 (the default) uses the current unix timestamp")
	url := flag.String("url", batch.DefaultWebhookURL, "webhook endpoint to POST to")
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

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	pool, err := db.Connect(ctx)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "database unavailable:", err)
		os.Exit(1)
	}
	defer pool.Close()

	res, err := batch.Run(context.Background(), pool, batch.Options{
		Size: *size,
		Seed: *seed,
		URL:  *url,
		Progress: func(i int, paymentID, scenario, action, outcome, baseline string, amount int64) {
			fmt.Printf("  [%d/%d] %-19s %-19s %-22s -> %-18s (baseline %s) %s\n",
				i+1, *size, paymentID, scenario, action, outcome, baseline, Rupees(amount))
		},
		Skipped: func(i int, paymentID, reason string) {
			fmt.Fprintf(os.Stderr, "  [%d/%d] %s: %s\n", i+1, *size, paymentID, reason)
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Printf("\nBatch complete: %d payments, seed=%d\n", res.Size, res.Seed)
	fmt.Printf("At risk:    %s\n", Rupees(res.TotalAtRiskPaise))
	fmt.Printf("Recovered:  %s  (%.1f%% recovery rate)\n", Rupees(res.TotalRecoveredPaise), res.RecoveryRate*100)
	fmt.Printf("Baseline:   %s  (%.1f%% recovery rate)\n", Rupees(res.BaselineRecoveredPaise), res.BaselineRecoveryRate*100)
	fmt.Printf("Improvement: %+.1f percentage points over naive baseline\n",
		(res.RecoveryRate-res.BaselineRecoveryRate)*100)

	// The breakdown is printed after the headline figures rather than instead of
	// them. escalated_pending is the number worth watching: those are payments
	// the agent deliberately did not attempt, and counting them as failures
	// would misread a policy choice as a loss.
	fmt.Printf("\nOutcomes: %d recovered, %d still_failed, %d escalated_pending",
		res.Recovered, res.StillFailed, res.EscalatedPending)
	if res.Skipped > 0 {
		fmt.Printf(", %d skipped (not scored)", res.Skipped)
	}
	fmt.Printf("\nbatch_runs id=%d\n", res.ID)
}

// Rupees renders paise with Indian digit grouping, matching what the dashboard
// shows for the same number.
func Rupees(paise int64) string {
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
