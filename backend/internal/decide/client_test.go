package decide

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient points a Client at a stub server. No test in this file reaches
// the real API.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{apiKey: "test-key", http: srv.Client(), endpoint: srv.URL}
}

// respondWith wraps text in the Anthropic response envelope.
func respondWith(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(apiResponse{
			Content: []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{{Type: "text", Text: text}},
			StopReason: "end_turn",
		})
	}
}

var sampleInput = DecisionInput{
	Category:                "insufficient_funds",
	ErrorReason:             "insufficient_funds",
	AmountPaise:             499900,
	AttemptCount:            0,
	TimeSinceFailureSeconds: 120,
	RemainingRetryBudget:    1,
}

func TestDecideParsesValidResponse(t *testing.T) {
	const text = `{"action":"retry_delayed","confidence":0.72,"reasoning":"Funds may not have cleared.","customer_message":"We'll automatically retry your payment shortly.","alternate_method":""}`

	c := newTestClient(t, respondWith(text))

	d, out, err := c.Decide(context.Background(), sampleInput)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Raw != text {
		t.Errorf("raw text\n got: %s\nwant: %s", out.Raw, text)
	}
	if d.Action != ActionRetryDelayed {
		t.Errorf("Action = %q, want %q", d.Action, ActionRetryDelayed)
	}
	if d.Confidence != 0.72 {
		t.Errorf("Confidence = %v, want 0.72", d.Confidence)
	}
	if d.CustomerMessage != "We'll automatically retry your payment shortly." {
		t.Errorf("CustomerMessage = %q", d.CustomerMessage)
	}
	if d.AlternateMethod != "" {
		t.Errorf("AlternateMethod = %q, want empty", d.AlternateMethod)
	}
}

func TestDecideMalformedJSONReturnsErrorWithRawText(t *testing.T) {
	// The exact failure seen from this model on Day 1: correct JSON, wrapped in
	// a markdown fence.
	const text = "```json\n{\"action\":\"retry_now\",\"confidence\":0.8,\"reasoning\":\"r\",\"customer_message\":\"m\"}\n```"

	c := newTestClient(t, respondWith(text))

	_, out, err := c.Decide(context.Background(), sampleInput)
	if err == nil {
		t.Fatal("expected an error for fenced JSON, got nil")
	}
	if out.Raw != text {
		t.Errorf("raw text was not preserved on failure\n got: %s\nwant: %s", out.Raw, text)
	}
	if !strings.Contains(err.Error(), "unmarshal decision") {
		t.Errorf("error = %v, want an unmarshal failure", err)
	}
}

func TestDecideRejectsOutOfRangeConfidence(t *testing.T) {
	// Structurally valid JSON, in-schema field names, impossible value.
	const text = `{"action":"retry_now","confidence":1.5,"reasoning":"r","customer_message":"m","alternate_method":""}`

	c := newTestClient(t, respondWith(text))

	_, out, err := c.Decide(context.Background(), sampleInput)
	if err == nil {
		t.Fatal("expected a validation error for confidence 1.5, got nil")
	}
	if out.Raw != text {
		t.Errorf("raw text was not preserved on validation failure: %s", out.Raw)
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Errorf("error = %v, want an out-of-range failure", err)
	}
}

func TestDecideRejectsNegativeConfidence(t *testing.T) {
	const text = `{"action":"escalate","confidence":-0.1,"reasoning":"r","customer_message":"m"}`

	c := newTestClient(t, respondWith(text))

	if _, _, err := c.Decide(context.Background(), sampleInput); err == nil {
		t.Fatal("expected a validation error for confidence -0.1, got nil")
	}
}

func TestDecideRejectsInventedAction(t *testing.T) {
	// Plausible-looking but not one of the five. Acting on this would mean
	// executing an action the rest of the system has no handler for.
	const text = `{"action":"retry_tomorrow","confidence":0.9,"reasoning":"r","customer_message":"m"}`

	c := newTestClient(t, respondWith(text))

	_, out, err := c.Decide(context.Background(), sampleInput)
	if err == nil {
		t.Fatal("expected a validation error for an unknown action, got nil")
	}
	if !strings.Contains(err.Error(), "invalid action") {
		t.Errorf("error = %v, want an invalid-action failure", err)
	}
	if out.Raw != text {
		t.Errorf("raw text was not preserved: %s", out.Raw)
	}
}

// The exact inconsistency seen in the second harness run: escalate carrying an
// alternate_method. A consumer reading that field without also checking action
// would act on a suggestion the model never meant as one.
func TestDecideRejectsAlternateMethodOnNonSuggestAction(t *testing.T) {
	const text = `{"action":"escalate","confidence":0.88,"reasoning":"r","customer_message":"m","alternate_method":"upi"}`

	c := newTestClient(t, respondWith(text))

	_, out, err := c.Decide(context.Background(), sampleInput)
	if err == nil {
		t.Fatal("expected an error for alternate_method on escalate, got nil")
	}
	if !strings.Contains(err.Error(), "must be empty") {
		t.Errorf("error = %v, want a must-be-empty failure", err)
	}
	if out.Raw != text {
		t.Errorf("raw text was not preserved: %s", out.Raw)
	}
}

func TestDecideRejectsSuggestWithoutAlternateMethod(t *testing.T) {
	const text = `{"action":"suggest_alternate_method","confidence":0.8,"reasoning":"r","customer_message":"m","alternate_method":""}`

	c := newTestClient(t, respondWith(text))

	if _, _, err := c.Decide(context.Background(), sampleInput); err == nil {
		t.Fatal("expected an error for suggest_alternate_method with no method, got nil")
	}
}

func TestDecideRejectsUnknownAlternateMethod(t *testing.T) {
	const text = `{"action":"suggest_alternate_method","confidence":0.8,"reasoning":"r","customer_message":"m","alternate_method":"cash"}`

	c := newTestClient(t, respondWith(text))

	_, _, err := c.Decide(context.Background(), sampleInput)
	if err == nil {
		t.Fatal("expected an error for an unknown alternate_method, got nil")
	}
	if !strings.Contains(err.Error(), "invalid alternate_method") {
		t.Errorf("error = %v, want an invalid-alternate-method failure", err)
	}
}

// sampleInput has payment_method "card", so suggesting card is suggesting the
// method that just failed.
func TestDecideRejectsAlternateMethodMatchingPaymentMethod(t *testing.T) {
	const text = `{"action":"suggest_alternate_method","confidence":0.8,"reasoning":"r","customer_message":"m","alternate_method":"card"}`

	c := newTestClient(t, respondWith(text))

	if _, _, err := c.Decide(context.Background(), sampleInput); err == nil {
		t.Fatal("expected an error for alternate_method equal to payment_method, got nil")
	}
}

func TestDecideAcceptsValidSuggestAlternateMethod(t *testing.T) {
	const text = `{"action":"suggest_alternate_method","confidence":0.8,"reasoning":"r","customer_message":"m","alternate_method":"upi"}`

	c := newTestClient(t, respondWith(text))

	d, _, err := c.Decide(context.Background(), sampleInput)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.AlternateMethod != "upi" {
		t.Errorf("AlternateMethod = %q, want upi", d.AlternateMethod)
	}
}

// respondInSequence returns each text in turn, and reports how many requests
// were made so a test can prove no third attempt happened.
func respondInSequence(texts ...string) (http.HandlerFunc, *int) {
	calls := 0
	return func(w http.ResponseWriter, r *http.Request) {
		i := calls
		calls++
		if i >= len(texts) {
			// A call past the end means the retry bound leaked.
			w.WriteHeader(http.StatusTeapot)
			return
		}
		respondWith(texts[i])(w, r)
	}, &calls
}

// The live failure this retry exists for: Haiku fenced its output once, then
// answered correctly when asked again.
func TestDecideRetriesOnceOnParseFailure(t *testing.T) {
	const fenced = "```json\n{\"action\":\"retry_now\",\"confidence\":0.8,\"reasoning\":\"r\",\"customer_message\":\"m\"}\n```"
	const valid = `{"action":"retry_delayed","confidence":0.72,"reasoning":"r","customer_message":"m","alternate_method":""}`

	handler, calls := respondInSequence(fenced, valid)
	c := newTestClient(t, handler)

	d, out, err := c.Decide(context.Background(), sampleInput)
	if err != nil {
		t.Fatalf("expected the retry to succeed, got: %v", err)
	}
	if d.Action != ActionRetryDelayed {
		t.Errorf("Action = %q, want the retry's %q", d.Action, ActionRetryDelayed)
	}
	if !out.Retried {
		t.Error("Outcome.Retried = false, want true")
	}
	if out.Raw != valid {
		t.Errorf("Outcome.Raw is not the retry's text\n got: %s\nwant: %s", out.Raw, valid)
	}
	if *calls != 2 {
		t.Errorf("made %d requests, want exactly 2", *calls)
	}
}

func TestDecideDoesNotRetryMoreThanOnce(t *testing.T) {
	// Two different malformed bodies, so the error and raw text identify which
	// attempt they came from.
	const first = "```json\n{\"action\":\"retry_now\",\"confidence\":0.8}\n```"
	const second = `Here is the decision: {"action":"retry_now","confidence":0.8}`

	handler, calls := respondInSequence(first, second)
	c := newTestClient(t, handler)

	_, out, err := c.Decide(context.Background(), sampleInput)
	if err == nil {
		t.Fatal("expected an error when both attempts fail, got nil")
	}
	if *calls != 2 {
		t.Errorf("made %d requests, want exactly 2 — no third attempt", *calls)
	}
	if out.Raw != second {
		t.Errorf("Outcome.Raw is not the second attempt's text\n got: %s\nwant: %s", out.Raw, second)
	}
	if out.Raw == first {
		t.Error("Outcome.Raw is the first attempt's text; it must be the second's")
	}
	if !out.Retried {
		t.Error("Outcome.Retried = false, want true")
	}
}

// A validation failure is a format failure too — the response parsed but was
// not a usable Decision.
func TestDecideRetriesOnValidationFailure(t *testing.T) {
	const badConfidence = `{"action":"retry_now","confidence":1.5,"reasoning":"r","customer_message":"m"}`
	const valid = `{"action":"escalate","confidence":0.9,"reasoning":"r","customer_message":"m","alternate_method":""}`

	handler, calls := respondInSequence(badConfidence, valid)
	c := newTestClient(t, handler)

	d, out, err := c.Decide(context.Background(), sampleInput)
	if err != nil {
		t.Fatalf("expected the retry to succeed, got: %v", err)
	}
	if d.Action != ActionEscalate {
		t.Errorf("Action = %q, want %q", d.Action, ActionEscalate)
	}
	if !out.Retried {
		t.Error("Outcome.Retried = false, want true")
	}
	if *calls != 2 {
		t.Errorf("made %d requests, want exactly 2", *calls)
	}
}

// Retrying a real outage would bill the API twice for nothing, so non-200s are
// deliberately not retried.
func TestDecideDoesNotRetryOnNonOKStatus(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"type":"error"}`)
	})

	_, out, err := c.Decide(context.Background(), sampleInput)
	if err == nil {
		t.Fatal("expected an error for HTTP 401, got nil")
	}
	if calls != 1 {
		t.Errorf("made %d requests, want exactly 1 — a non-200 must not be retried", calls)
	}
	if out.Retried {
		t.Error("Outcome.Retried = true, want false for a non-format failure")
	}
}

func TestDecideDoesNotRetryOnCancelledContext(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		respondWith(`{"action":"no_retry","confidence":0.5,"reasoning":"r","customer_message":"m"}`)(w, r)
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := c.Decide(ctx, sampleInput); err == nil {
		t.Fatal("expected an error for a cancelled context, got nil")
	}
	if calls != 0 {
		t.Errorf("made %d requests, want 0 — a cancelled context must not be retried", calls)
	}
}

// A response that parses on the first attempt must not trigger a second call.
func TestDecideDoesNotRetryOnSuccess(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		respondWith(`{"action":"retry_now","confidence":0.8,"reasoning":"r","customer_message":"m"}`)(w, r)
	})

	_, out, err := c.Decide(context.Background(), sampleInput)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("made %d requests, want exactly 1", calls)
	}
	if out.Retried {
		t.Error("Outcome.Retried = true on a first-attempt success")
	}
}

func TestDecideNonOKStatusPreservesBody(t *testing.T) {
	const body = `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, body)
	})

	_, out, err := c.Decide(context.Background(), sampleInput)
	if err == nil {
		t.Fatal("expected an error for HTTP 401, got nil")
	}
	if out.Raw != body {
		t.Errorf("error body was not preserved\n got: %s\nwant: %s", out.Raw, body)
	}
}

func TestDecideSendsCorrectRequest(t *testing.T) {
	var got apiRequest
	var headers http.Header

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		json.NewDecoder(r.Body).Decode(&got)
		respondWith(`{"action":"no_retry","confidence":0.5,"reasoning":"r","customer_message":"m"}`)(w, r)
	})

	if _, _, err := c.Decide(context.Background(), sampleInput); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("model = %q, want claude-haiku-4-5-20251001", got.Model)
	}
	if got.MaxTokens != 1024 {
		t.Errorf("max_tokens = %d, want 1024", got.MaxTokens)
	}
	if got.System != BuildSystemPrompt() {
		t.Error("system prompt sent does not match BuildSystemPrompt()")
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v, want one user message", got.Messages)
	}
	if got.Messages[0].Content != BuildUserMessage(sampleInput) {
		t.Errorf("user message = %q, want %q", got.Messages[0].Content, BuildUserMessage(sampleInput))
	}
	if headers.Get("x-api-key") != "test-key" {
		t.Errorf("x-api-key = %q", headers.Get("x-api-key"))
	}
	if headers.Get("anthropic-version") != anthropicVersion {
		t.Errorf("anthropic-version = %q, want %q", headers.Get("anthropic-version"), anthropicVersion)
	}
}

func TestDecideRespectsContextCancellation(t *testing.T) {
	c := newTestClient(t, respondWith(`{"action":"no_retry","confidence":0.5,"reasoning":"r","customer_message":"m"}`))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := c.Decide(ctx, sampleInput); err == nil {
		t.Fatal("expected an error for a cancelled context, got nil")
	}
}

func TestNewClientRequiresAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")

	if _, err := NewClient(); err == nil {
		t.Fatal("expected an error when ANTHROPIC_API_KEY is empty, got nil")
	}
}

func TestNewClientReadsAPIKeyFromEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-value")

	c, err := NewClient()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.apiKey != "sk-test-value" {
		t.Errorf("apiKey = %q, want sk-test-value", c.apiKey)
	}
	if c.endpoint != defaultEndpoint {
		t.Errorf("endpoint = %q, want %q", c.endpoint, defaultEndpoint)
	}
}
