package api

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
)

// apiKeyHeader carries the shared secret on every /api/ request.
const apiKeyHeader = "X-API-Key"

// apiKeyEnv names the environment variable holding the expected secret.
const apiKeyEnv = "API_ACCESS_KEY"

// errMissingAPIKey is returned by NewAuth when API_ACCESS_KEY is unset or
// empty.
//
// This is a startup error rather than a per-request one on purpose. A server
// that booted with no key configured and answered anyway would be serving the
// payment data and the write routes to anyone who found the port, which is the
// exact state this gate exists to prevent — and it would do so silently, since
// nothing in a successful response reveals that the check is not running. The
// alternative of treating "no key set" as "reject everything" was considered
// and rejected too: it fails in a way an operator reads as a bad key rather
// than as a missing one, and sends them looking in the wrong place.
var errMissingAPIKey = fmt.Errorf("%s is not set: refusing to serve /api/ without a shared secret", apiKeyEnv)

// auth is the shared-secret gate in front of every /api/ route.
type auth struct {
	key  string
	next http.Handler
}

// NewAuth wraps h with the shared-secret check, reading the expected value from
// API_ACCESS_KEY. It returns an error rather than a handler when that variable
// is unset, so the process can refuse to start.
//
// This wraps the whole /api/ subtree rather than being called inside each
// handler. A per-handler check is one route away from being forgotten — and the
// route most likely to be forgotten is the newest one, which nobody has thought
// about yet. Here, a route added to the mux inside NewHandler is covered by
// construction, because the request cannot reach the mux without passing this
// first.
//
// The webhook endpoint is deliberately NOT wrapped. It is called by Razorpay,
// which has no way to send our header, and it has its own separate
// authenticity problem — signature verification — that this secret does not
// solve and must not be mistaken for.
func NewAuth(h http.Handler) (http.Handler, error) {
	key := os.Getenv(apiKeyEnv)
	if key == "" {
		return nil, errMissingAPIKey
	}
	return &auth{key: key, next: h}, nil
}

func (a *auth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS preflight is answered before the key is checked, and must be: a
	// browser sends OPTIONS without custom headers by definition — the
	// preflight is what asks permission to send X-API-Key in the first place.
	// Answering 401 here would fail every browser request before the real one
	// was ever made, and the console would report it as a CORS error rather
	// than as an authentication one.
	//
	// This discloses nothing. The response carries no payment data and is
	// identical whether or not the caller holds the key.
	if r.Method == http.MethodOptions {
		a.next.ServeHTTP(w, r)
		return
	}

	presented := r.Header.Get(apiKeyHeader)

	// Constant-time comparison: == returns as soon as two bytes differ, so the
	// time it takes to fail leaks how long a correct prefix was, and a caller
	// who can measure that can recover the key one byte at a time. The length
	// check first is not a shortcut around that — subtle.ConstantTimeCompare
	// returns 0 for unequal lengths without comparing, so lengths are already
	// distinguishable; only the same-length case needs to be constant time,
	// and that is the case this protects.
	if subtle.ConstantTimeCompare([]byte(presented), []byte(a.key)) != 1 {
		// The reason is not narrowed to "missing" versus "wrong". Both are the
		// same answer to the caller: they may not have this. Telling an
		// attacker that their header was the right shape and only the value
		// was wrong confirms the mechanism for them.
		//
		// It is logged, though, with the distinction the response withholds —
		// an operator debugging a frontend that suddenly 401s needs to know
		// whether the header arrived at all.
		fmt.Fprintf(os.Stderr, "{\"event\":\"api_auth_rejected\",\"path\":%q,\"header_present\":%t}\n",
			r.URL.Path, presented != "")

		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "unauthorized",
		})
		return
	}

	a.next.ServeHTTP(w, r)
}
