package ingest

import (
	"context"
	"errors"

	"github.com/bhavyamsharmaa/recovery-agent/internal/decide"
)

// Demo-only forced decision failure, scoped to a single request.
//
// WHY THIS SHAPE, AND NOT AN ENV VAR. Day 4 had a FORCE_DECIDE_FAILURE
// environment variable for exactly this purpose and it was removed before merge,
// because a global switch that makes the decision layer fail is one deployment
// mistake away from making it fail in production, and nothing in a request would
// reveal that it had been left on. The replacement is deliberately incapable of
// that: the instruction rides on one request's context, it is put there by one
// clearly-labelled simulate-only endpoint, and no configuration anywhere can
// turn it on for traffic that did not ask for it.
//
// A webhook arriving on /webhook/payment-failed carries no such context value,
// so ForcedFailureDecider is indistinguishable from the real client for it. The
// only way to reach the failure path is to call the endpoint that exists to
// demonstrate it.

// forceFailureKey is an unexported context key type, so no package outside this
// one can set the value even by accident — a plain string key could be
// overwritten by any code that happened to use the same string.
type forceFailureKey struct{}

// WithForcedDecideFailure marks a context as belonging to a request that has
// asked for the decision layer to fail.
//
// It is exported because the API package needs to build such a request, and
// unexported keys mean that is the only way to do it.
func WithForcedDecideFailure(ctx context.Context) context.Context {
	return context.WithValue(ctx, forceFailureKey{}, true)
}

// ForcedDecideFailureRequested reports whether this context was marked.
func ForcedDecideFailureRequested(ctx context.Context) bool {
	v, _ := ctx.Value(forceFailureKey{}).(bool)
	return v
}

// ErrForcedFailure is what the decision layer returns for a marked request. It
// says plainly in its text that it was deliberate, so a reader finding it in a
// log is not sent hunting for an outage that never happened.
var ErrForcedFailure = errors.New("decide: forced failure requested by /api/simulate/llm-failure (demo only)")

// ForcedFailureDecider wraps the real decider and fails only for requests that
// asked to fail.
//
// Failing on both the original call and the retry is the point: Day 4's bounded
// retry exists to survive a single bad response, and the fallback only engages
// when the retry fails too. A wrapper that failed once would demonstrate the
// retry, not the fallback. Returning the error from every call is what drives
// the handler down the double-failure path this endpoint exists to show.
type ForcedFailureDecider struct {
	Real Decider
}

var _ Decider = (*ForcedFailureDecider)(nil)

func (d *ForcedFailureDecider) Decide(ctx context.Context, in decide.DecisionInput) (decide.Decision, decide.Outcome, error) {
	if ForcedDecideFailureRequested(ctx) {
		// Outcome.Retried is reported true so the handler's log line describes
		// what actually happened to a real double failure: the call was made,
		// the retry was made, and both failed.
		return decide.Decision{}, decide.Outcome{Retried: true}, ErrForcedFailure
	}
	return d.Real.Decide(ctx, in)
}
