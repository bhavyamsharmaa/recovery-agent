// Command simulate fires fake Razorpay payment.failed webhooks at a URL.
//
// The payload shape mirrors Razorpay's documented payment.failed event exactly,
// so that on Day 8 a real test-mode webhook can be pointed at the same endpoint
// with no receiver changes.
//
// The scenarios and payload construction moved to internal/simulate when
// cmd/run-batch needed the same payloads and could not import a `package main`.
// What is left here is the command-line surface.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"time"

	"github.com/bhavyamsharmaa/recovery-agent/internal/simulate"
)

func main() {
	url := flag.String("url", "http://localhost:8080/webhook/payment-failed", "webhook endpoint to POST to")
	scenario := flag.String("scenario", "insufficient_funds", "insufficient_funds | bank_downtime | hard_decline | soft_decline | network_error | duplicate | random")
	count := flag.Int("count", 1, "number of webhooks to send")
	eventID := flag.String("event-id", "", "reuse this exact event id for every call; empty generates a fresh one per call")
	paymentID := flag.String("payment-id", "", "reuse this exact payment id for every call while event ids stay fresh; empty generates a fresh one per call")
	flag.Parse()

	if *scenario != "random" && !simulate.KnownScenario(*scenario) {
		fmt.Fprintf(os.Stderr, "unknown scenario %q\n", *scenario)
		flag.Usage()
		os.Exit(2)
	}
	if *count < 1 {
		fmt.Fprintln(os.Stderr, "--count must be at least 1")
		os.Exit(2)
	}

	// This command is a manual tool, so its randomness is seeded from the clock
	// and its output is not meant to be reproducible. cmd/run-batch is the one
	// that needs a stored seed, and it supplies its own.
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	// A duplicate is a redelivery: Razorpay resends the identical event, so the
	// body and the event id must both stay fixed across calls. Building it once
	// outside the loop is what makes this a real idempotency test.
	var fixedID string
	var fixedBody []byte
	if *scenario == "duplicate" {
		fixedID = *eventID
		if fixedID == "" {
			fixedID = simulate.RazorpayID(rng, "evt")
		}
		fixedBody = simulate.MustMarshal(simulate.Build(rng, "duplicate", *paymentID))
	}

	failures := 0
	for i := 0; i < *count; i++ {
		name := *scenario
		id := *eventID
		body := fixedBody

		switch {
		case name == "duplicate":
			id = fixedID
		default:
			if name == "random" {
				name = simulate.PickScenario(rng)
			}
			if id == "" {
				id = simulate.RazorpayID(rng, "evt")
			}
			body = simulate.MustMarshal(simulate.Build(rng, name, *paymentID))
		}

		status, err := simulate.Send(*url, id, body)
		if err != nil {
			fmt.Printf("event_id=%s scenario=%s status=ERROR (%v)\n", id, name, err)
			failures++
			continue
		}
		fmt.Printf("event_id=%s scenario=%s status=%d\n", id, name, status)
		if status < 200 || status > 299 {
			failures++
		}
	}

	if failures > 0 {
		fmt.Fprintf(os.Stderr, "%d of %d webhooks were not accepted\n", failures, *count)
		os.Exit(1)
	}
}
