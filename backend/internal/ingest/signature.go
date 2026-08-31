package ingest

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
)

// signatureHeader is where Razorpay carries the HMAC of the request body.
const signatureHeader = "X-Razorpay-Signature"

// webhookSecretEnv names the environment variable holding the webhook signing
// secret.
//
// This is NOT RAZORPAY_KEY_ID/RAZORPAY_KEY_SECRET. Those are API credentials
// for calling Razorpay; this one is configured separately in their dashboard
// when the webhook is registered, and is the only value that can verify a
// delivery came from them. Reusing the API secret here would verify nothing,
// because Razorpay does not sign with it.
const webhookSecretEnv = "RAZORPAY_WEBHOOK_SECRET"

// ErrMissingWebhookSecret is returned by NewVerifier when the secret is unset.
//
// Fatal at startup, matching API_ACCESS_KEY and ANTHROPIC_API_KEY. A receiver
// that came up unable to verify signatures would accept anything the internet
// posted at it — manufactured payment records, spent model budget, fabricated
// decisions in the audit trail — and would look completely healthy doing it,
// because an unverified delivery is processed exactly like a real one.
var ErrMissingWebhookSecret = fmt.Errorf("%s is not set: refusing to serve the webhook without a signing secret", webhookSecretEnv)

// Sign returns the hex-encoded HMAC-SHA256 of body under secret.
//
// Exported because the simulator has to produce deliveries this package will
// accept, and the alternative — a second implementation in the simulator — is
// two definitions of "correctly signed" that can disagree. If this function is
// wrong, the tests and the simulator are wrong in exactly the same way, which
// is the failure mode to accept here: the real check is against Razorpay's own
// deliveries, and no local reimplementation makes that check for us.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body) // hash.Hash.Write never returns an error
	return hex.EncodeToString(mac.Sum(nil))
}

// verifier holds the secret the handler checks deliveries against.
type verifier struct {
	secret string
}

// unusableVerifier returns a verifier holding a fresh random secret, so it
// rejects every delivery.
//
// This is the default in NewHandler, and it is what makes an unconfigured
// handler fail closed. The alternative — a nil verifier meaning "skip the
// check" — would make "verification is off" both the default and invisible,
// on the one endpoint exposed to the public internet.
//
// The secret is random rather than a constant so that nothing can sign for it,
// including code in this repository that can read the constant. A rand.Read
// failure panics: the process cannot safely continue without knowing this
// value is unguessable, and the only alternative is a predictable secret.
func unusableVerifier() *verifier {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("ingest: could not generate placeholder webhook secret: " + err.Error())
	}
	return &verifier{secret: string(b)}
}

// NewVerifier reads the signing secret, returning an error when it is unset so
// the process can refuse to start.
func NewVerifier() (*verifier, error) {
	secret := os.Getenv(webhookSecretEnv)
	if secret == "" {
		return nil, ErrMissingWebhookSecret
	}
	return &verifier{secret: secret}, nil
}

// verify reports whether body carries a valid signature for this request.
//
// body must be the exact bytes received, before any parsing. See the comment
// at the read site in ServeHTTP for why re-serialising would break this.
func (v *verifier) verify(r *http.Request, body []byte) bool {
	presented := r.Header.Get(signatureHeader)
	if presented == "" {
		return false
	}

	expected := Sign(v.secret, body)

	// Constant-time, for the same reason the API key gate is: == returns at the
	// first differing byte, so the time a rejection takes leaks how long a
	// correct prefix was, and a caller who can measure that can recover a valid
	// signature one byte at a time.
	//
	// Comparing the hex strings rather than the raw digests is deliberate: a
	// malformed header that does not decode as hex should be rejected as a
	// wrong signature, not crash or be special-cased. hmac.Equal on []byte(s)
	// of the two hex strings compares them without either.
	return hmac.Equal([]byte(presented), []byte(expected))
}

// signatureRejectedLog records a delivery that failed verification.
//
// It carries no signature value and no secret. The presented signature is an
// attacker-supplied string and logging it in full would put a value that is one
// correct guess away from valid into a log that is copied into issues and
// pasted into chats; the length and whether the header was present at all are
// what actually distinguish "misconfigured sender" from "someone probing".
//
// It carries no payment id either, because there is no trustworthy one: the
// body has not been parsed, and parsing it to enrich a rejection log would be
// doing work on unverified input for the sake of a log line.
type signatureRejectedLog struct {
	Event string `json:"event"`
	Path  string `json:"path"`

	// HeaderPresent separates "sender is not signing at all" — a
	// misconfiguration, or a caller that does not know it must — from "signed
	// with the wrong secret", which is a key mismatch or a forgery attempt.
	HeaderPresent bool `json:"header_present"`

	// SignatureLength is a shape check without disclosure. A correct signature
	// is 64 hex characters; 0 or 71 says the sender is confused about the
	// format, while exactly 64 says the format is right and the secret is not.
	SignatureLength int `json:"signature_length"`

	// BodyBytes is what was hashed. A mismatch when everything else looks right
	// points at something rewriting the body in transit — a proxy
	// re-encoding JSON is the usual culprit, and it is invisible without this.
	BodyBytes int `json:"body_bytes"`

	TS string `json:"ts"`
}
