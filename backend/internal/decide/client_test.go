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

	d, raw, err := c.Decide(context.Background(), sampleInput)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if raw != text {
		t.Errorf("raw text\n got: %s\nwant: %s", raw, text)
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

	_, raw, err := c.Decide(context.Background(), sampleInput)
	if err == nil {
		t.Fatal("expected an error for fenced JSON, got nil")
	}
	if raw != text {
		t.Errorf("raw text was not preserved on failure\n got: %s\nwant: %s", raw, text)
	}
	if !strings.Contains(err.Error(), "unmarshal decision") {
		t.Errorf("error = %v, want an unmarshal failure", err)
	}
}

func TestDecideRejectsOutOfRangeConfidence(t *testing.T) {
	// Structurally valid JSON, in-schema field names, impossible value.
	const text = `{"action":"retry_now","confidence":1.5,"reasoning":"r","customer_message":"m","alternate_method":""}`

	c := newTestClient(t, respondWith(text))

	_, raw, err := c.Decide(context.Background(), sampleInput)
	if err == nil {
		t.Fatal("expected a validation error for confidence 1.5, got nil")
	}
	if raw != text {
		t.Errorf("raw text was not preserved on validation failure: %s", raw)
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

	_, raw, err := c.Decide(context.Background(), sampleInput)
	if err == nil {
		t.Fatal("expected a validation error for an unknown action, got nil")
	}
	if !strings.Contains(err.Error(), "invalid action") {
		t.Errorf("error = %v, want an invalid-action failure", err)
	}
	if raw != text {
		t.Errorf("raw text was not preserved: %s", raw)
	}
}

func TestDecideNonOKStatusPreservesBody(t *testing.T) {
	const body = `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, body)
	})

	_, raw, err := c.Decide(context.Background(), sampleInput)
	if err == nil {
		t.Fatal("expected an error for HTTP 401, got nil")
	}
	if raw != body {
		t.Errorf("error body was not preserved\n got: %s\nwant: %s", raw, body)
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
