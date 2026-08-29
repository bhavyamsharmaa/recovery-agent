package simulate

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"net/http"
	"time"
)

// Scenario generation, moved here from cmd/simulate when cmd/run-batch needed
// the same payloads. A command is `package main` and cannot be imported, so the
// choice was to copy this or to lift it; a second copy of the failure taxonomy
// is a second place for it to drift away from docs/taxonomy.md.
//
// The randomness is a parameter throughout, for the same reason it is in
// outcome.go: a batch run has to be reproducible from one stored seed, and a
// package reaching for the global source cannot be.

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

// ScenarioNames is the fixed pick-list for a random scenario. "duplicate" is
// excluded: it is a delivery behaviour, not a failure mode.
var ScenarioNames = []string{
	"insufficient_funds",
	"bank_downtime",
	"hard_decline",
	"soft_decline",
	"network_error",
}

// KnownScenario reports whether a name can be built.
func KnownScenario(name string) bool {
	_, ok := scenarios[name]
	return ok
}

// PickScenario chooses one of the five real failure categories.
func PickScenario(rng *rand.Rand) string {
	return ScenarioNames[rng.Intn(len(ScenarioNames))]
}

// Event is Razorpay's payment.failed webhook envelope, as this simulator emits
// it. The shape mirrors Razorpay's documented event exactly, so a real
// test-mode webhook can later be pointed at the same endpoint with no receiver
// changes.
type Event struct {
	AccountID string   `json:"account_id"`
	Contains  []string `json:"contains"`
	CreatedAt int64    `json:"created_at"`
	Entity    string   `json:"entity"`
	Event     string   `json:"event"`
	Payload   Payload  `json:"payload"`
}

type Payload struct {
	Payment struct {
		Entity PaymentEntity `json:"entity"`
	} `json:"payment"`
}

type PaymentEntity struct {
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

// Build renders one event. A non-empty paymentID is reused verbatim, which is
// what lets the same payment be delivered repeatedly under fresh event ids —
// the shape of a payment that fails again, as opposed to a redelivery of one
// failure, which is what the "duplicate" scenario produces.
//
// The amount is drawn from rng, so a seeded caller gets the same rupee figures
// every run. That is what makes a batch's "at risk" total reproducible.
func Build(rng *rand.Rand, name, paymentID string) Event {
	s := scenarios[name]
	now := time.Now().Unix()

	if paymentID == "" {
		paymentID = RazorpayID(rng, "pay")
	}

	var e Event
	e.AccountID = "acc_BFQ7uQEaa7j2z7"
	e.Contains = []string{"payment"}
	e.CreatedAt = now
	e.Entity = "event"
	e.Event = "payment.failed"
	e.Payload.Payment.Entity = PaymentEntity{
		ID:               paymentID,
		Amount:           (rng.Intn(50000-100+1) + 100) * 100, // 100-50000 INR, sent as paise
		Currency:         "INR",
		Status:           "failed",
		Method:           s.method,
		ErrorCode:        s.errorCode,
		ErrorDescription: s.errorDescription,
		ErrorReason:      s.errorReason,
		ErrorSource:      s.errorSource,
		ErrorStep:        s.errorStep,
		OrderID:          RazorpayID(rng, "order"),
		CreatedAt:        now,
	}
	return e
}

// Send POSTs one event to the webhook endpoint, carrying the event id in the
// header where Razorpay puts it.
func Send(url, eventID string, body []byte) (int, error) {
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

// RazorpayID mimics Razorpay's <prefix>_<14 alphanumeric> id format.
func RazorpayID(rng *rand.Rand, prefix string) string {
	b := make([]byte, 14)
	for i := range b {
		b[i] = idChars[rng.Intn(len(idChars))]
	}
	return prefix + "_" + string(b)
}

// MustMarshal serialises an event.
func MustMarshal(e Event) []byte {
	b, err := json.Marshal(e)
	if err != nil {
		panic(err) // the struct is fixed and fully serialisable; this cannot fail
	}
	return b
}
