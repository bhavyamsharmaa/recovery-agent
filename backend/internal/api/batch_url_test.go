package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Issue #3, at the layer where it actually happened.
//
// The batch package now fails loudly when nothing can be delivered, but that is
// the backstop. The bug itself was here: triggerBatchRun called batch.Run
// without Options.URL, so it silently used batch.DefaultWebhookURL —
// localhost:8080 — which no deployed instance listens on. These tests are about
// the URL being configured and passed, not about what a batch does once it has
// one.
//
// They need no database: the configuration check runs before any query, which
// is itself part of the contract — a misconfigured instance should say so
// rather than start work it cannot finish.

func TestBatchRunURLIsBuiltFromPublicBaseURL(t *testing.T) {
	cases := []struct {
		name string
		base string
		want string
	}{
		{
			name: "plain host",
			base: "https://recovery-agent-mcyz.onrender.com",
			want: "https://recovery-agent-mcyz.onrender.com/webhook/payment-failed",
		},
		{
			// A trailing slash is the obvious way to get this wrong when pasting
			// a URL into a dashboard, and it would produce a double slash in the
			// path. Trimmed rather than rejected: the value is unambiguous.
			name: "trailing slash is trimmed",
			base: "https://recovery-agent-mcyz.onrender.com/",
			want: "https://recovery-agent-mcyz.onrender.com/webhook/payment-failed",
		},
		{
			name: "local development",
			base: "http://localhost:8080",
			want: "http://localhost:8080/webhook/payment-failed",
		},
		{
			// The deployed case that issue #3 broke on: a port that is not 8080.
			name: "non-default port",
			base: "http://localhost:10000",
			want: "http://localhost:10000/webhook/payment-failed",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(publicBaseURLEnv, c.base)

			h, err := NewHandlerWithBatchURL(nil)
			if err != nil {
				t.Fatalf("NewHandlerWithBatchURL: %v", err)
			}
			if h.webhookURL != c.want {
				t.Errorf("webhookURL = %q, want %q", h.webhookURL, c.want)
			}
		})
	}
}

// The whole point of the fix: whatever the handler ends up with, it must not be
// the package default that caused the bug.
func TestBatchRunURLIsNeverTheLocalhostDefault(t *testing.T) {
	t.Setenv(publicBaseURLEnv, "https://recovery-agent-mcyz.onrender.com")

	h, err := NewHandlerWithBatchURL(nil)
	if err != nil {
		t.Fatalf("NewHandlerWithBatchURL: %v", err)
	}
	if strings.Contains(h.webhookURL, "localhost:8080") {
		t.Errorf("webhookURL = %q; a configured instance must not fall back to the local default", h.webhookURL)
	}
}

// An unset PUBLIC_BASE_URL must stop the process, not fall back. The fallback
// is what made issue #3 invisible: a deployed server came up looking healthy
// and produced runs that recorded zeros.
func TestMissingPublicBaseURLRefusesToBuild(t *testing.T) {
	t.Setenv(publicBaseURLEnv, "")

	if _, err := NewHandlerWithBatchURL(nil); err == nil {
		t.Fatal("NewHandlerWithBatchURL succeeded with no base URL set; it must refuse so the server can fail to start")
	} else if !strings.Contains(err.Error(), publicBaseURLEnv) {
		t.Errorf("error does not name the missing variable: %v", err)
	}
}

// A handler built without the URL — NewHandler, which every other test and the
// read-only callers use — must refuse to run a batch rather than posting to the
// package default.
//
// 503 with the variable named, not a 500: this is a configuration problem with
// a specific fix, and the response should say which one. The read routes beside
// it keep working, which is why this is checked per request rather than at
// construction.
func TestTriggerBatchRunRefusesWithoutAConfiguredURL(t *testing.T) {
	h := NewHandler(nil) // no webhookURL

	req := httptest.NewRequest(http.MethodPost, "/api/batch-runs", strings.NewReader(`{"size":2}`))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()

	// Straight to the handler: a nil database is safe precisely because the
	// configuration check must return before anything queries it. If this ever
	// regresses, the nil pool panics rather than quietly proceeding.
	h.triggerBatchRun(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), publicBaseURLEnv) {
		t.Errorf("response does not name the missing variable: %s", rec.Body.String())
	}
}

// The size validation must still run, and must run before the configuration
// check has any say — a bad request is a bad request whether or not the
// instance is configured.
func TestTriggerBatchRunStillValidatesSizeWithoutAURL(t *testing.T) {
	h := NewHandler(nil)

	req := httptest.NewRequest(http.MethodPost, "/api/batch-runs", strings.NewReader(`{"size":9999}`))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	h.triggerBatchRun(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an out-of-range size", rec.Code)
	}
}
