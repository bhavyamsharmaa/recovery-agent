package ingest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bhavyamsharmaa/recovery-agent/internal/decide"
)

// The isolation guarantee behind the demo's forced-failure endpoint.
//
// Day 4 had a FORCE_DECIDE_FAILURE environment variable for this and it was
// removed before merge, because a global switch that breaks the decision layer
// is one deployment mistake away from breaking it in production with nothing in
// a request to reveal it. These tests exist so a future change cannot quietly
// reintroduce that hazard in a new shape: the property under test is not "the
// forced failure works" but "nothing except the marked context can cause it".
//
// The test that would have caught the original incident is
// TestWebhookCannotBeForcedToFailByAnyInput, which drives the real handler with
// every input a caller controls and asserts the model was reached every time.

// countingDecider records how many times it was actually consulted, so a test
// can prove the real decider was reached rather than short-circuited.
type countingDecider struct {
	calls    int
	decision decide.Decision
}

func (d *countingDecider) Decide(context.Context, decide.DecisionInput) (decide.Decision, decide.Outcome, error) {
	d.calls++
	return d.decision, decide.Outcome{}, nil
}

func confidentDecision() decide.Decision {
	return decide.Decision{
		Action:          decide.ActionRetryDelayed,
		Confidence:      0.90,
		Reasoning:       "First failure, one retry remains.",
		CustomerMessage: "We'll automatically retry your payment shortly.",
	}
}

// TestForcedFailureDeciderPassesThroughUnmarkedContexts is the default path:
// with no marker, the wrapper must be indistinguishable from the real client.
func TestForcedFailureDeciderPassesThroughUnmarkedContexts(t *testing.T) {
	real := &countingDecider{decision: confidentDecision()}
	wrapped := &ForcedFailureDecider{Real: real}

	// Every context shape a real request could arrive with.
	contexts := map[string]context.Context{
		"background":              context.Background(),
		"todo":                    context.TODO(),
		"with an unrelated value": context.WithValue(context.Background(), struct{ k string }{"other"}, true),
		"with a string key named like the marker": context.WithValue(
			context.Background(), "forceFailureKey", true), //nolint:staticcheck // deliberately the wrong key type
	}

	for name, ctx := range contexts {
		t.Run(name, func(t *testing.T) {
			before := real.calls
			d, _, err := wrapped.Decide(ctx, decide.DecisionInput{Category: "insufficient_funds"})
			if err != nil {
				t.Fatalf("unmarked context produced an error: %v", err)
			}
			if real.calls != before+1 {
				t.Error("the real decider was not consulted")
			}
			if d.Action != decide.ActionRetryDelayed {
				t.Errorf("action = %q, want the real decider's answer", d.Action)
			}
		})
	}
}

// TestForcedFailureDeciderFailsOnlyWhenMarked covers the positive case, and
// that the failure is reported as a double failure — the fallback only engages
// when the retry fails too, so a wrapper that failed once would demonstrate the
// retry rather than the fallback.
func TestForcedFailureDeciderFailsOnlyWhenMarked(t *testing.T) {
	real := &countingDecider{decision: confidentDecision()}
	wrapped := &ForcedFailureDecider{Real: real}

	ctx := WithForcedDecideFailure(context.Background())
	_, outcome, err := wrapped.Decide(ctx, decide.DecisionInput{Category: "insufficient_funds"})

	if !errors.Is(err, ErrForcedFailure) {
		t.Fatalf("err = %v, want ErrForcedFailure", err)
	}
	if !outcome.Retried {
		t.Error("Outcome.Retried is false; the handler's log would not describe a real double failure")
	}
	if real.calls != 0 {
		t.Error("the real decider was consulted for a forced failure; no model call should be made")
	}
	// The error text has to say it was deliberate, or a reader finding it in a
	// log goes hunting for an outage that never happened.
	if !strings.Contains(err.Error(), "demo only") {
		t.Errorf("error text %q does not identify itself as deliberate", err.Error())
	}
}

// TestForcedFailureMarkerDoesNotLeakAcrossContexts guards the scope of the
// marker: a derived context inherits it, which is intended, but a sibling
// context built from the same parent must not.
func TestForcedFailureMarkerDoesNotLeakAcrossContexts(t *testing.T) {
	parent := context.Background()
	marked := WithForcedDecideFailure(parent)

	if !ForcedDecideFailureRequested(marked) {
		t.Error("the marked context does not report as marked")
	}
	if ForcedDecideFailureRequested(parent) {
		t.Error("marking a derived context mutated its parent")
	}

	sibling := context.WithValue(parent, struct{ k string }{"unrelated"}, 1)
	if ForcedDecideFailureRequested(sibling) {
		t.Error("a sibling of the marked context reports as marked")
	}

	// A child of the marked context does inherit it. That is correct — the
	// handler derives a timeout context from the request's before calling the
	// decider, and the marker has to survive that.
	child, cancel := context.WithTimeout(marked, 0)
	defer cancel()
	if !ForcedDecideFailureRequested(child) {
		t.Error("a child of the marked context lost the marker; the handler derives one before deciding")
	}
}

// TestWebhookCannotBeForcedToFailByAnyInput is the test that would have caught
// the Day 4 incident.
//
// It drives the REAL handler with every input a caller controls — headers,
// query parameters, and body fields, including ones named exactly after the
// mechanism — and asserts the model was reached every single time. If a future
// change ever translates one of these into the context marker, this fails.
func TestWebhookCannotBeForcedToFailByAnyInput(t *testing.T) {
	real := &countingDecider{decision: confidentDecision()}
	h := NewHandler(&ForcedFailureDecider{Real: real}, NewInMemoryAttemptStore())

	body := func(id string, extra string) string {
		return `{"event":"payment.failed",` + extra + `"payload":{"payment":{"entity":{` +
			`"id":"` + id + `","amount":50000,"method":"card",` +
			`"error_code":"BAD_REQUEST_ERROR","error_reason":"insufficient_funds",` +
			`"error_source":"customer","error_step":"payment_authorization"}}}}`
	}

	cases := []struct {
		name    string
		path    string
		headers map[string]string
		extra   string
	}{
		{name: "plain", path: "/webhook/payment-failed"},
		{
			name:    "header named after the removed env var",
			path:    "/webhook/payment-failed",
			headers: map[string]string{"X-Force-Decide-Failure": "true", "FORCE_DECIDE_FAILURE": "1"},
		},
		{
			name:    "header named after the endpoint",
			path:    "/webhook/payment-failed",
			headers: map[string]string{"X-Simulate-Llm-Failure": "1", "X-Forced": "true"},
		},
		{
			name: "query parameters",
			path: "/webhook/payment-failed?force_decide_failure=true&forced=true&simulate=llm-failure",
		},
		{
			name:  "body fields",
			path:  "/webhook/payment-failed",
			extra: `"forced":true,"force_decide_failure":true,"simulate_llm_failure":true,`,
		},
		{
			name:    "everything at once",
			path:    "/webhook/payment-failed?forced=true&force_decide_failure=true",
			headers: map[string]string{"X-Force-Decide-Failure": "true", "X-Forced": "1"},
			extra:   `"forced":true,"force_decide_failure":true,`,
		},
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := real.calls

			paymentID := "pay_isolation_probe_" + c.name
			req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(body(paymentID, c.extra)))
			req.Header.Set("content-type", "application/json")
			req.Header.Set(eventIDHeader, "evt_isolation_probe_"+string(rune('a'+i)))
			for k, v := range c.headers {
				req.Header.Set(k, v)
			}

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("webhook answered %d, want 200", rec.Code)
			}
			if real.calls != before+1 {
				t.Fatalf("the real decider was NOT consulted — this input reached the forced-failure "+
					"path, which means /webhook/payment-failed can be made to fail from outside. "+
					"That is the Day 4 hazard, reintroduced. Input: %+v", c)
			}
			// And no fallback line was emitted, so the double-failure path was
			// never taken.
			if out := rec.Body.String(); strings.Contains(out, "fallback") {
				t.Errorf("response mentions a fallback: %s", out)
			}
		})
	}
}

// TestWebhookStillReachesFallbackWhenGenuinelyMarked is the counterweight.
//
// Without it, the isolation test above would pass against a wrapper that never
// fails at all — which would be "safe" and completely broken. This drives the
// same handler with a marked context and asserts the fallback really does
// engage, so both halves of the property are pinned.
func TestWebhookStillReachesFallbackWhenGenuinelyMarked(t *testing.T) {
	real := &countingDecider{decision: confidentDecision()}
	h := NewHandler(&ForcedFailureDecider{Real: real}, NewInMemoryAttemptStore())

	body := `{"event":"payment.failed","payload":{"payment":{"entity":{` +
		`"id":"pay_marked_probe","amount":50000,"method":"card",` +
		`"error_code":"BAD_REQUEST_ERROR","error_reason":"insufficient_funds",` +
		`"error_source":"customer","error_step":"payment_authorization"}}}}`

	req := httptest.NewRequest(http.MethodPost, "/webhook/payment-failed", strings.NewReader(body)).
		WithContext(WithForcedDecideFailure(context.Background()))
	req.Header.Set("content-type", "application/json")
	req.Header.Set(eventIDHeader, "evt_marked_probe")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("webhook answered %d, want 200 — a model outage must not become Razorpay's problem", rec.Code)
	}
	if real.calls != 0 {
		t.Error("the real decider was consulted despite the marker")
	}
}
