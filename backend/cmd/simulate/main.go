// Command simulate fires fake Razorpay payment.failed webhooks at a URL.
//
// The payload shape mirrors Razorpay's documented payment.failed event exactly,
// so that on Day 8 a real test-mode webhook can be pointed at the same endpoint
// with no receiver changes.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"time"
)

// seed holds the per-scenario fields that vary. Everything else in the payload
// is either fixed or randomised per call.
//
// error_reason and error_source come from the "Category → decline code mapping"
// section of docs/taxonomy.md — each scenario is seeded to classify as its
// namesake category. Change them there first, not here.
//
// error_code and error_step are still modelled rather than captured. Day 8
// should reconcile all four against what real test-mode webhooks send.
type seed struct {
	method           string
	errorCode        string
	errorDescription string
	errorReason      string
	errorSource      string
	errorStep        string
}

var scenarios = map[string]seed{
	// Customer has no money. Retrying now is pointless; retrying after payday is not.
	"insufficient_funds": {
		method:           "card",
		errorCode:        "BAD_REQUEST_ERROR",
		errorDescription: "Your payment was declined due to insufficient funds",
		errorReason:      "insufficient_funds",
		errorSource:      "customer",
		errorStep:        "payment_authorization",
	},
	// Bank or gateway is down. The instrument is fine — wait and retry.
	"bank_downtime": {
		method:           "netbanking",
		errorCode:        "GATEWAY_ERROR",
		errorDescription: "Your payment could not be completed due to a temporary bank issue",
		errorReason:      "bank_technical_error",
		errorSource:      "bank",
		errorStep:        "payment_authentication",
	},
	// Issuer refused outright. No retry on this instrument will ever succeed.
	"hard_decline": {
		method:           "card",
		errorCode:        "BAD_REQUEST_ERROR",
		errorDescription: "Your card has been declined by the issuing bank",
		errorReason:      "card_declined",
		errorSource:      "bank",
		errorStep:        "payment_authorization",
	},
	// Authentication did not complete. Same instrument, retry is plausible.
	"soft_decline": {
		method:           "card",
		errorCode:        "BAD_REQUEST_ERROR",
		errorDescription: "Payment authentication failed. Please retry and complete the verification",
		errorReason:      "authentication_failed",
		errorSource:      "customer",
		errorStep:        "payment_authentication",
	},
	// Timed out in transit. We do not know whether the bank saw it.
	"network_error": {
		method:           "upi",
		errorCode:        "GATEWAY_ERROR",
		errorDescription: "Your payment timed out before it could reach the bank",
		errorReason:      "gateway_technical_error",
		errorSource:      "gateway",
		errorStep:        "payment_authorization",
	},
	// Redelivery of an event we have already seen. Same seed as insufficient_funds;
	// what makes it a duplicate is that the whole payload repeats byte for byte.
	"duplicate": {
		method:           "card",
		errorCode:        "BAD_REQUEST_ERROR",
		errorDescription: "Your payment was declined due to insufficient funds",
		errorReason:      "insufficient_funds",
		errorSource:      "customer",
		errorStep:        "payment_authorization",
	},
}

// scenarioNames is the fixed pick-list for --scenario random. "duplicate" is
// excluded: it is a delivery behaviour, not a failure mode.
var scenarioNames = []string{
	"insufficient_funds",
	"bank_downtime",
	"hard_decline",
	"soft_decline",
	"network_error",
}

type event struct {
	AccountID string   `json:"account_id"`
	Contains  []string `json:"contains"`
	CreatedAt int64    `json:"created_at"`
	Entity    string   `json:"entity"`
	Event     string   `json:"event"`
	Payload   payload  `json:"payload"`
}

type payload struct {
	Payment struct {
		Entity paymentEntity `json:"entity"`
	} `json:"payment"`
}

type paymentEntity struct {
	ID               string `json:"id"`
	Amount           int    `json:"amount"` // paise
	Currency         string `json:"currency"`
	Status           string `json:"status"`
	Method           string `json:"method"`
	ErrorCode        string `json:"error_code"`
	ErrorDescription string `json:"error_description"`
	ErrorReason      string `json:"error_reason"`
	ErrorSource      string `json:"error_source"`
	ErrorStep        string `json:"error_step"`
	OrderID          string `json:"order_id"`
	CreatedAt        int64  `json:"created_at"`
}

func main() {
	url := flag.String("url", "http://localhost:8080/webhook/payment-failed", "webhook endpoint to POST to")
	scenario := flag.String("scenario", "insufficient_funds", "insufficient_funds | bank_downtime | hard_decline | soft_decline | network_error | duplicate | random")
	count := flag.Int("count", 1, "number of webhooks to send")
	eventID := flag.String("event-id", "", "reuse this exact event id for every call; empty generates a fresh one per call")
	flag.Parse()

	if *scenario != "random" {
		if _, ok := scenarios[*scenario]; !ok {
			fmt.Fprintf(os.Stderr, "unknown scenario %q\n", *scenario)
			flag.Usage()
			os.Exit(2)
		}
	}
	if *count < 1 {
		fmt.Fprintln(os.Stderr, "--count must be at least 1")
		os.Exit(2)
	}

	// A duplicate is a redelivery: Razorpay resends the identical event, so the
	// body and the event id must both stay fixed across calls. Building it once
	// outside the loop is what makes this a real idempotency test.
	var fixedID string
	var fixedBody []byte
	if *scenario == "duplicate" {
		fixedID = *eventID
		if fixedID == "" {
			fixedID = razorpayID("evt")
		}
		fixedBody = mustMarshal(build("duplicate"))
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
				name = scenarioNames[rand.Intn(len(scenarioNames))]
			}
			if id == "" {
				id = razorpayID("evt")
			}
			body = mustMarshal(build(name))
		}

		status, err := send(*url, id, body)
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

func build(name string) event {
	s := scenarios[name]
	now := time.Now().Unix()

	var e event
	e.AccountID = "acc_BFQ7uQEaa7j2z7"
	e.Contains = []string{"payment"}
	e.CreatedAt = now
	e.Entity = "event"
	e.Event = "payment.failed"
	e.Payload.Payment.Entity = paymentEntity{
		ID:               razorpayID("pay"),
		Amount:           (rand.Intn(50000-100+1) + 100) * 100, // 100-50000 INR, sent as paise
		Currency:         "INR",
		Status:           "failed",
		Method:           s.method,
		ErrorCode:        s.errorCode,
		ErrorDescription: s.errorDescription,
		ErrorReason:      s.errorReason,
		ErrorSource:      s.errorSource,
		ErrorStep:        s.errorStep,
		OrderID:          razorpayID("order"),
		CreatedAt:        now,
	}
	return e
}

func send(url, eventID string, body []byte) (int, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("content-type", "application/json")
	// Razorpay carries the event id in a header, not the body. It is the only
	// stable handle a receiver has for deduplicating redeliveries.
	req.Header.Set("x-razorpay-event-id", eventID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

const idChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// razorpayID mimics Razorpay's <prefix>_<14 alphanumeric> id format.
func razorpayID(prefix string) string {
	b := make([]byte, 14)
	for i := range b {
		b[i] = idChars[rand.Intn(len(idChars))]
	}
	return prefix + "_" + string(b)
}

func mustMarshal(e event) []byte {
	b, err := json.Marshal(e)
	if err != nil {
		panic(err) // the struct is fixed and fully serialisable; this cannot fail
	}
	return b
}
