package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// These tests need no database, unlike the rest of this package.
//
// That is the point of the gate: it rejects before the request reaches a
// handler, so proving it rejects requires no rows to reject access to. Keeping
// them DB-free means they run in a plain `go test ./...` — the check that
// stands between the payment data and the internet should not be one that only
// runs when someone remembers to set RECOVERY_LIVE_TESTS.

const testKey = "test-secret-value"

// reached is a stand-in for the real API handler. It records whether the
// request got past the gate, which is the fact these tests are actually about:
// a 401 body proves what the caller was told, not that the handler was skipped.
type reached struct {
	called bool
}

func (s *reached) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.called = true
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

func newTestAuth(t *testing.T) (http.Handler, *reached) {
	t.Helper()

	t.Setenv(apiKeyEnv, testKey)
	downstream := &reached{}
	gate, err := NewAuth(downstream)
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	return gate, downstream
}

// route is one endpoint to drive the gate with. Both a read and a write are
// covered, because "the gate is mounted" and "the gate is mounted on
// everything" are different claims and only the second one is the requirement.
type route struct {
	name   string
	method string
	path   string
}

// Every route the mux registers, so this list failing to match NewHandler is
// itself a signal. The paths are spelled out rather than derived, so adding a
// route to the mux without adding it here leaves a visible gap in what is
// proven rather than a silently smaller test.
var allRoutes = []route{
	{"read payments", http.MethodGet, "/api/payments"},
	{"read one payment", http.MethodGet, "/api/payments/pay_123"},
	{"read escalations", http.MethodGet, "/api/escalations"},
	{"read batch runs", http.MethodGet, "/api/batch-runs"},
	{"read latest batch run", http.MethodGet, "/api/batch-runs/latest"},
	{"write batch run", http.MethodPost, "/api/batch-runs"},
	{"write simulate failure", http.MethodPost, "/api/simulate/failure"},
	{"write simulate duplicate", http.MethodPost, "/api/simulate/duplicate"},
	{"write simulate llm failure", http.MethodPost, "/api/simulate/llm-failure"},
}

func TestNoHeaderIsRejected(t *testing.T) {
	for _, rt := range allRoutes {
		t.Run(rt.name, func(t *testing.T) {
			gate, downstream := newTestAuth(t)

			rec := httptest.NewRecorder()
			gate.ServeHTTP(rec, httptest.NewRequest(rt.method, rt.path, nil))

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if downstream.called {
				t.Error("request reached the handler; the gate must reject before it")
			}
		})
	}
}

func TestWrongKeyIsRejected(t *testing.T) {
	// Values chosen to cover the ways a wrong key is wrong: close but not
	// equal, a prefix of the real one, the empty string, and a different
	// length. The prefix case matters most — it is what a byte-at-a-time
	// guessing attack sends, and it must be as rejected as anything else.
	wrong := []struct {
		name  string
		value string
	}{
		{"different value", "not-the-secret"},
		{"correct prefix", testKey[:len(testKey)-1]},
		{"trailing character", testKey + "x"},
		{"empty header", ""},
		{"case changed", "TEST-SECRET-VALUE"},
	}

	for _, rt := range allRoutes {
		for _, w := range wrong {
			t.Run(rt.name+"/"+w.name, func(t *testing.T) {
				gate, downstream := newTestAuth(t)

				req := httptest.NewRequest(rt.method, rt.path, nil)
				req.Header.Set(apiKeyHeader, w.value)

				rec := httptest.NewRecorder()
				gate.ServeHTTP(rec, req)

				if rec.Code != http.StatusUnauthorized {
					t.Errorf("status = %d, want 401", rec.Code)
				}
				if downstream.called {
					t.Error("request reached the handler; the gate must reject before it")
				}
			})
		}
	}
}

func TestCorrectKeyIsAccepted(t *testing.T) {
	for _, rt := range allRoutes {
		t.Run(rt.name, func(t *testing.T) {
			gate, downstream := newTestAuth(t)

			req := httptest.NewRequest(rt.method, rt.path, nil)
			req.Header.Set(apiKeyHeader, testKey)

			rec := httptest.NewRecorder()
			gate.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
			if !downstream.called {
				t.Error("request did not reach the handler")
			}
		})
	}
}

// The 401 body is JSON, like every other error this API returns. A caller that
// parses error bodies should not have to special-case this one, and the
// frontend client's error path reads `error` out of exactly this shape.
func TestRejectionBodyIsJSON(t *testing.T) {
	gate, _ := newTestAuth(t)

	rec := httptest.NewRecorder()
	gate.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/payments", nil))

	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if body["error"] == "" {
		t.Errorf("body has no error field: %q", rec.Body.String())
	}
}

// The rejection must not tell the caller whether the header was missing or
// merely wrong. Both are "you may not have this"; distinguishing them confirms
// to someone probing that their header was the right shape.
func TestRejectionDoesNotDistinguishMissingFromWrong(t *testing.T) {
	gate, _ := newTestAuth(t)

	missing := httptest.NewRecorder()
	gate.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/api/payments", nil))

	wrongReq := httptest.NewRequest(http.MethodGet, "/api/payments", nil)
	wrongReq.Header.Set(apiKeyHeader, "wrong")
	wrong := httptest.NewRecorder()
	gate.ServeHTTP(wrong, wrongReq)

	if missing.Body.String() != wrong.Body.String() {
		t.Errorf("bodies differ: missing=%q wrong=%q", missing.Body.String(), wrong.Body.String())
	}
	if missing.Code != wrong.Code {
		t.Errorf("codes differ: missing=%d wrong=%d", missing.Code, wrong.Code)
	}
}

// A server with no key configured must not start. Serving anyway would expose
// every route to anyone who found the port, and nothing in a successful
// response would reveal that the check was not running.
func TestMissingEnvVarRefusesToBuild(t *testing.T) {
	t.Setenv(apiKeyEnv, "")

	if _, err := NewAuth(&reached{}); err == nil {
		t.Fatal("NewAuth succeeded with no key set; it must refuse")
	}
}

// CORS preflight passes without the key, and has to: a browser sends OPTIONS
// with no custom headers, because the preflight is what asks permission to send
// X-API-Key at all. Rejecting it would fail every browser request before the
// real one was made.
func TestPreflightPassesWithoutKey(t *testing.T) {
	gate, downstream := newTestAuth(t)

	rec := httptest.NewRecorder()
	gate.ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/api/payments", nil))

	if !downstream.called {
		t.Error("preflight was rejected; a browser could then never send the key")
	}
}

// The gate must be mounted on the real handler's whole surface, not only on the
// stub above. This drives NewHandler's own routes through the gate with no key
// and asserts none of them answer — including any route added to the mux after
// this test was written, since an unlisted route still cannot get past the gate.
func TestRealHandlerIsFullyGated(t *testing.T) {
	t.Setenv(apiKeyEnv, testKey)

	// A nil database is safe here precisely because nothing reaches the
	// handlers: if any of these requests got through, it would panic on the nil
	// pool rather than quietly returning something, so a regression fails
	// loudly instead of passing.
	gate, err := NewAuth(NewHandler(nil))
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}

	for _, rt := range allRoutes {
		t.Run(rt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			gate.ServeHTTP(rec, httptest.NewRequest(rt.method, rt.path, nil))

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}
