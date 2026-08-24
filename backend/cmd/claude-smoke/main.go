// Command claude-smoke is a throwaway probe. It makes exactly one call to the
// Anthropic Messages API and proves the response parses into the Decision
// struct that the recovery agent will depend on. Not product code — no retries,
// no config, no abstraction. Delete once Day 2 has the real client.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

const (
	endpoint         = "https://api.anthropic.com/v1/messages"
	model            = "claude-haiku-4-5-20251001"
	anthropicVersion = "2023-06-01"
	maxTokens        = 1024
)

// Decision is the schema Claude must produce. Day 2 depends on this shape.
type Decision struct {
	Action          string  `json:"action"`     // retry_now | retry_delayed | suggest_alternate_method | escalate | no_retry
	Confidence      float64 `json:"confidence"` // 0.0 to 1.0
	Reasoning       string  `json:"reasoning"`
	CustomerMessage string  `json:"customer_message"`
	AlternateMethod string  `json:"alternate_method,omitempty"` // "upi" | "netbanking" | ""
}

const systemPrompt = `You are a payment-failure recovery agent for an Indian payments product. You receive one failed payment attempt as JSON and decide how to recover it.

Your entire response is fed directly to a JSON parser. The first character you emit must be { and the last must be }. Emitting anything else crashes the parser.

The JSON object has exactly these keys:
{
  "action": one of "retry_now" | "retry_delayed" | "suggest_alternate_method" | "escalate" | "no_retry",
  "confidence": a number between 0.0 and 1.0,
  "reasoning": a short string explaining the decision (internal, not shown to the customer),
  "customer_message": a short string written directly to the customer,
  "alternate_method": "upi" | "netbanking" | "" (empty string unless action is "suggest_alternate_method")
}

Example input:
{"category":"card_expired","decline_code":"GATEWAY_ERROR","amount_inr":1200,"attempt_count":2,"time_since_failure_seconds":300,"remaining_retry_budget":1}

Example output (correct — bare JSON, nothing around it):
{"action":"suggest_alternate_method","confidence":0.88,"reasoning":"Card is expired, so retrying the same instrument cannot succeed. One retry remains but is better spent on a different method.","customer_message":"Your card appears to have expired. Would you like to pay 1200 via UPI instead?","alternate_method":"upi"}

The same output written WRONG — never do any of these:
` + "```json" + `
{"action":"suggest_alternate_method", ...}
` + "```" + `
(wrong: wrapped in a markdown code fence)

Here is the decision:
{"action":"suggest_alternate_method", ...}
(wrong: prose before the JSON)

Emit the correct form. Do not open your response with a backtick.`

const userScenario = `{
    "category": "insufficient_funds",
    "decline_code": "BAD_REQUEST_ERROR",
    "amount_inr": 4999,
    "attempt_count": 1,
    "time_since_failure_seconds": 45,
    "remaining_retry_budget": 0
}`

type request struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system"`
	Messages  []message `json:"messages"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type response struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
}

func main() {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "ANTHROPIC_API_KEY is not set")
		os.Exit(1)
	}

	body, err := json.Marshal(request{
		Model:     model,
		MaxTokens: maxTokens,
		System:    systemPrompt,
		Messages:  []message{{Role: "user", Content: userScenario}},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal request:", err)
		os.Exit(1)
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(os.Stderr, "build request:", err)
		os.Exit(1)
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("content-type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "send request:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read body:", err)
		os.Exit(1)
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "HTTP %d\n%s\n", resp.StatusCode, raw)
		os.Exit(1)
	}

	var parsed response
	if err := json.Unmarshal(raw, &parsed); err != nil {
		fmt.Fprintf(os.Stderr, "unmarshal envelope: %v\n%s\n", err, raw)
		os.Exit(1)
	}
	if len(parsed.Content) == 0 {
		fmt.Fprintf(os.Stderr, "response had no content blocks\n%s\n", raw)
		os.Exit(1)
	}

	text := parsed.Content[0].Text
	fmt.Println("--- raw content[0].text ---")
	fmt.Println(text)
	fmt.Println("--- stop_reason:", parsed.StopReason, "---")

	var decision Decision
	if err := json.Unmarshal([]byte(text), &decision); err != nil {
		fmt.Fprintf(os.Stderr, "\nunmarshal Decision: %v\nraw text was:\n%s\n", err, text)
		os.Exit(1)
	}

	fmt.Println("--- parsed Decision ---")
	fmt.Printf("%+v\n", decision)
}
