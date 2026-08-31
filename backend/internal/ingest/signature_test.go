package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testWebhookSecret is the signing secret used across this package's tests.
const testWebhookSecret = "test-webhook-signing-secret"

// testVerifier returns a verifier for testWebhookSecret.
//
// Tests install this explicitly rather than getting verification for free,
// because the handler's default is a verifier that rejects everything. That
// default is what makes a misconfigured production handler fail closed, and a
// test-only escape hatch that disabled the check would be the one path where
// "no verification" is possible — on the endpoint where it must not be.
func testVerifier() *verifier { return &verifier{secret: testWebhookSecret} }

// signedRequest builds a correctly signed delivery, the way the simulator and
// Razorpay both do: the signature is computed over the exact bytes the request
// carries, never over a re-serialised copy.
func signedRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/webhook/payment-failed", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	req.Header.Set(signatureHeader, Sign(testWebhookSecret, []byte(body)))
	return req
}

// validBody is a minimal well-formed payment.failed payload. The tests below
// are about signatures, so the payload only has to be something the handler
// would otherwise process.
func validBody(paymentID string) string {
	return `{"event":"payment.failed","payload":{"payment":{"entity":{` +
		`"id":"` + paymentID + `","amount":50000,"currency":"INR","method":"card",` +
		`"error_code":"BAD_REQUEST_ERROR","error_reason":"insufficient_funds",` +
		`"error_source":"customer","error_step":"payment_authorization"}}}}`
}

// countingDecider comes from forcefail_test.go, which already defines it with
// exactly the semantics these tests need: it records how many times the
// decision layer was actually consulted, which is how a rejected delivery is
// shown to have got nowhere near classification.

// recordingDecisionStore captures decisions instead of writing them, so a test
// can assert that a rejected delivery produced none.
type recordingDecisionStore struct {
	records []DecisionRecord
}

func (s *recordingDecisionStore) RecordDecision(_ context.Context, d DecisionRecord) error {
	s.records = append(s.records, d)
	return nil
}

func TestValidSignatureIsAccepted(t *testing.T) {
	decider := &countingDecider{}
	store := NewInMemoryAttemptStore()
	h := NewHandler(decider, store).WithVerifier(testVerifier())

	var buf bytes.Buffer
	saved := logOut
	logOut = &buf
	defer func() { logOut = saved }()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signedRequest(validBody("pay_sig_valid")))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if decider.calls != 1 {
		t.Errorf("decider called %d times, want 1 — a valid delivery must be processed", decider.calls)
	}
	if got := store.Get("pay_sig_valid"); got != 1 {
		t.Errorf("attempt count = %d, want 1 — a valid delivery must be counted", got)
	}
	if !strings.Contains(buf.String(), "payment_received") {
		t.Errorf("no payment_received line for a validly signed delivery: %s", buf.String())
	}
}

// The central guarantee: a rejected delivery must not reach classification, the
// decision layer, the attempt counter, or any write. Asserting only on the 401
// would pass even if the handler rejected *after* doing all of that.
func TestInvalidSignatureIsRejectedAndNothingIsWritten(t *testing.T) {
	cases := []struct {
		name      string
		signature string
	}{
		{"wrong secret", Sign("some-other-secret", []byte(validBody("pay_sig_wrong")))},
		{"not hex", "!!!not-a-signature!!!"},
		{"empty value", ""},
		{"truncated", Sign(testWebhookSecret, []byte(validBody("pay_sig_wrong")))[:63]},
		{"one char changed", flipLast(Sign(testWebhookSecret, []byte(validBody("pay_sig_wrong"))))},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			decider := &countingDecider{}
			store := NewInMemoryAttemptStore()
			recorder := &recordingDecisionStore{}
			events := NewInMemoryEventStore()
			h := NewHandler(decider, store).
				WithVerifier(testVerifier()).
				WithDecisionRecorder(recorder).
				WithEventStore(events)

			var buf bytes.Buffer
			saved := logOut
			logOut = &buf
			defer func() { logOut = saved }()

			req := httptest.NewRequest(http.MethodPost, "/webhook/payment-failed",
				strings.NewReader(validBody("pay_sig_wrong")))
			req.Header.Set(signatureHeader, c.signature)
			req.Header.Set(eventIDHeader, "evt_sig_wrong")

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}

			// Nothing downstream ran.
			if decider.calls != 0 {
				t.Errorf("decider called %d times, want 0 — rejection must precede the decision layer", decider.calls)
			}
			if got := store.Get("pay_sig_wrong"); got != 0 {
				t.Errorf("attempt count = %d, want 0 — rejection must precede counting", got)
			}
			if len(recorder.records) != 0 {
				t.Errorf("%d decisions written, want 0 — rejection must precede any write", len(recorder.records))
			}

			// The event id must not be consumed either: recording it would make
			// a later genuine delivery of the same event look like a duplicate
			// and be silently dropped.
			if !events.RecordEvent("evt_sig_wrong", "pay_sig_wrong") {
				t.Error("event id was consumed by a rejected delivery; a genuine redelivery would now be dropped")
			}

			out := buf.String()
			if !strings.Contains(out, "webhook_signature_invalid") {
				t.Errorf("no webhook_signature_invalid line: %s", out)
			}
			if strings.Contains(out, "payment_received") {
				t.Errorf("payment_received logged for a rejected delivery: %s", out)
			}
		})
	}
}

func TestMissingSignatureHeaderIsRejected(t *testing.T) {
	decider := &countingDecider{}
	store := NewInMemoryAttemptStore()
	h := NewHandler(decider, store).WithVerifier(testVerifier())

	var buf bytes.Buffer
	saved := logOut
	logOut = &buf
	defer func() { logOut = saved }()

	// No signature header at all.
	req := httptest.NewRequest(http.MethodPost, "/webhook/payment-failed",
		strings.NewReader(validBody("pay_sig_missing")))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if decider.calls != 0 {
		t.Errorf("decider called %d times, want 0", decider.calls)
	}
	if got := store.Get("pay_sig_missing"); got != 0 {
		t.Errorf("attempt count = %d, want 0", got)
	}

	// header_present distinguishes a sender that is not signing at all from one
	// signing with the wrong secret. An operator needs that to tell a
	// misconfiguration from a key mismatch.
	var line map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err == nil && m["event"] == "webhook_signature_invalid" {
			line = m
		}
	}
	if line == nil {
		t.Fatalf("no webhook_signature_invalid line: %s", buf.String())
	}
	if line["header_present"] != false {
		t.Errorf("header_present = %v, want false", line["header_present"])
	}
}

// Tampering in transit: sign a body, then change it. This is the case the whole
// mechanism exists for — the signature was genuinely valid for what the sender
// sent, and must not be valid for what arrived.
func TestBodyAlteredAfterSigningIsRejected(t *testing.T) {
	original := validBody("pay_sig_original")

	// Signed over the original, delivered with a different amount. A receiver
	// that verified a re-serialised parse, or that hashed anything other than
	// the received bytes, would accept this.
	tampered := strings.Replace(original, `"amount":50000`, `"amount":1`, 1)
	if tampered == original {
		t.Fatal("test bug: the tampered body is identical to the original")
	}

	decider := &countingDecider{}
	store := NewInMemoryAttemptStore()
	h := NewHandler(decider, store).WithVerifier(testVerifier())

	var buf bytes.Buffer
	saved := logOut
	logOut = &buf
	defer func() { logOut = saved }()

	req := httptest.NewRequest(http.MethodPost, "/webhook/payment-failed", strings.NewReader(tampered))
	req.Header.Set(signatureHeader, Sign(testWebhookSecret, []byte(original)))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — a body altered after signing must not verify", rec.Code)
	}
	if decider.calls != 0 {
		t.Errorf("decider called %d times, want 0", decider.calls)
	}
	if got := store.Get("pay_sig_original"); got != 0 {
		t.Errorf("attempt count = %d, want 0", got)
	}
	if strings.Contains(buf.String(), "payment_received") {
		t.Errorf("payment_received logged for a tampered delivery: %s", buf.String())
	}
}

// Byte-for-byte equality is the property the handler depends on: it verifies
// the received bytes and then parses those same bytes. This pins that a
// semantically identical body with different bytes does NOT verify, which is
// why the handler must never re-serialise before checking.
func TestSemanticallyEqualBodyWithDifferentBytesDoesNotVerify(t *testing.T) {
	signed := `{"event":"payment.failed","amount":50000}`

	// Same JSON object, different bytes: reordered keys and added whitespace.
	// json.Unmarshal treats these as equal; HMAC does not, and must not.
	variants := []string{
		`{"amount":50000,"event":"payment.failed"}`,
		`{"event":"payment.failed", "amount":50000}`,
		`{"event":"payment.failed","amount":50000} `,
		`{"event":"payment.failed","amount":5.0e4}`,
	}

	want := Sign(testWebhookSecret, []byte(signed))
	for _, v := range variants {
		if got := Sign(testWebhookSecret, []byte(v)); got == want {
			t.Errorf("%q produced the same signature as %q; the hash is not over raw bytes", v, signed)
		}
	}
}

// A handler built without WithVerifier must reject everything, including a
// delivery signed with an empty secret — which is what an unconfigured sender
// and an unconfigured receiver would agree on if the default were a blank key.
func TestHandlerWithoutVerifierFailsClosed(t *testing.T) {
	decider := &countingDecider{}
	h := NewHandler(decider, NewInMemoryAttemptStore())

	var buf bytes.Buffer
	saved := logOut
	logOut = &buf
	defer func() { logOut = saved }()

	body := validBody("pay_sig_default")
	for _, secret := range []string{"", testWebhookSecret, "any-secret"} {
		req := httptest.NewRequest(http.MethodPost, "/webhook/payment-failed", strings.NewReader(body))
		req.Header.Set(signatureHeader, Sign(secret, []byte(body)))

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("secret %q: status = %d, want 401 — an unconfigured handler must reject everything", secret, rec.Code)
		}
	}
	if decider.calls != 0 {
		t.Errorf("decider called %d times, want 0", decider.calls)
	}
}

// NewVerifier must refuse to build without a secret, so main can refuse to
// start rather than serving an endpoint that accepts anything.
func TestNewVerifierRequiresTheSecret(t *testing.T) {
	t.Setenv(webhookSecretEnv, "")
	if _, err := NewVerifier(); err == nil {
		t.Fatal("NewVerifier succeeded with no secret set; it must refuse")
	}

	t.Setenv(webhookSecretEnv, "a-secret")
	v, err := NewVerifier()
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if v.secret != "a-secret" {
		t.Errorf("secret = %q, want %q", v.secret, "a-secret")
	}
}

// The rejection body must not leak the expected signature or the secret. It is
// returned to an unauthenticated caller, so anything in it is public.
func TestRejectionDisclosesNothing(t *testing.T) {
	h := NewHandler(&countingDecider{}, NewInMemoryAttemptStore()).WithVerifier(testVerifier())

	var buf bytes.Buffer
	saved := logOut
	logOut = &buf
	defer func() { logOut = saved }()

	body := validBody("pay_sig_leak")
	req := httptest.NewRequest(http.MethodPost, "/webhook/payment-failed", strings.NewReader(body))
	req.Header.Set(signatureHeader, "wrong")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	expected := Sign(testWebhookSecret, []byte(body))
	if strings.Contains(rec.Body.String(), expected) {
		t.Error("response body contains the expected signature")
	}
	if strings.Contains(rec.Body.String(), testWebhookSecret) {
		t.Error("response body contains the secret")
	}
	if strings.Contains(buf.String(), testWebhookSecret) {
		t.Error("log line contains the secret")
	}
	if strings.Contains(buf.String(), expected) {
		t.Error("log line contains the expected signature")
	}
}

// flipLast changes the final character of a signature, producing a value of the
// right length and shape that is still wrong.
func flipLast(s string) string {
	if s == "" {
		return s
	}
	last := s[len(s)-1]
	if last == '0' {
		return s[:len(s)-1] + "1"
	}
	return s[:len(s)-1] + "0"
}
