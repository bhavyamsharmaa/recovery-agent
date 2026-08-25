package decide

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
)

const (
	defaultEndpoint  = "https://api.anthropic.com/v1/messages"
	model            = "claude-haiku-4-5-20251001"
	anthropicVersion = "2023-06-01"
	maxTokens        = 1024
)

type Client struct {
	apiKey string
	http   *http.Client

	// endpoint is overridable so tests can point at an httptest.Server.
	// Production callers get defaultEndpoint via NewClient.
	endpoint string
}

// NewClient reads ANTHROPIC_API_KEY from the environment.
func NewClient() (*Client, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, errors.New("decide: ANTHROPIC_API_KEY is not set")
	}
	return &Client{
		apiKey:   key,
		http:     &http.Client{},
		endpoint: defaultEndpoint,
	}, nil
}

type apiRequest struct {
	Model     string       `json:"model"`
	MaxTokens int          `json:"max_tokens"`
	System    string       `json:"system"`
	Messages  []apiMessage `json:"messages"`
}

type apiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type apiResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
}

// Decide asks the model what to do about one failed payment.
//
// The second return value is the raw text we tried to parse, and it is returned
// on every path where we have it — including failures. A rejected decision is
// only debuggable if you can see what the model actually said.
//
// No timeout is set here; cancellation is the caller's job via ctx.
func (c *Client) Decide(ctx context.Context, in DecisionInput) (Decision, string, error) {
	body, err := json.Marshal(apiRequest{
		Model:     model,
		MaxTokens: maxTokens,
		System:    BuildSystemPrompt(),
		Messages:  []apiMessage{{Role: "user", Content: BuildUserMessage(in)}},
	})
	if err != nil {
		return Decision{}, "", fmt.Errorf("decide: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return Decision{}, "", fmt.Errorf("decide: build request: %w", err)
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("content-type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return Decision{}, "", fmt.Errorf("decide: send request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Decision{}, "", fmt.Errorf("decide: read response: %w", err)
	}

	// Before the model's text is extracted, the response body itself is the most
	// useful raw text we have — an auth failure or rate limit lands here.
	if resp.StatusCode != http.StatusOK {
		return Decision{}, string(raw), fmt.Errorf("decide: http %d", resp.StatusCode)
	}

	var envelope apiResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return Decision{}, string(raw), fmt.Errorf("decide: unmarshal envelope: %w", err)
	}
	if len(envelope.Content) == 0 {
		return Decision{}, string(raw), errors.New("decide: response had no content blocks")
	}

	text := envelope.Content[0].Text

	var d Decision
	if err := json.Unmarshal([]byte(text), &d); err != nil {
		return Decision{}, text, fmt.Errorf("decide: unmarshal decision: %w", err)
	}
	if err := validate(d); err != nil {
		return d, text, err
	}
	return d, text, nil
}

// validate rejects a well-formed JSON object that is not a usable decision.
// Structural validity is not the same as being in-schema, and acting on an
// out-of-range confidence or an invented action is worse than failing loudly.
func validate(d Decision) error {
	switch d.Action {
	case ActionRetryNow, ActionRetryDelayed, ActionSuggestAlternateMethod, ActionEscalate, ActionNoRetry:
	default:
		return fmt.Errorf("decide: invalid action %q", d.Action)
	}
	if d.Confidence < 0.0 || d.Confidence > 1.0 {
		return fmt.Errorf("decide: confidence %v out of range [0.0, 1.0]", d.Confidence)
	}
	return nil
}
