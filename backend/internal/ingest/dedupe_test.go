package ingest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bhavyamsharmaa/recovery-agent/internal/decide"
)

// confidentStub is a decider whose answers always clear the confidence gate, so
// a dedupe test never has an override line confusing its assertions.
func confidentStub() *stubDecider {
	return &stubDecider{decision: decide.Decision{
		Action:          decide.ActionRetryDelayed,
		Confidence:      0.90,
		Reasoning:       "First failure, one retry remains.",
		CustomerMessage: "We'll automatically retry your payment shortly.",
	}}
}

// TestDedupeSameEventIDIsProcessedOnce is the Day 1 guarantee, restated against
// the event id the key was moved to: a redelivery is the same event arriving
// again, and only the first one does any work.
func TestDedupeSameEventIDIsProcessedOnce(t *testing.T) {
	decider := confidentStub()
	h := NewHandler(decider, NewInMemoryAttemptStore())
	body := webhookBody("pay_same_event", "insufficient_funds")

	first := fireEvent(t, h, "evt_fixed", body)
	received := findLine(t, first, "payment_received")
	if got := received["payment_id"]; got != "pay_same_event" {
		t.Errorf("payment_id = %v, want pay_same_event", got)
	}
	for _, m := range first {
		if m["event"] == "duplicate" {
			t.Errorf("first delivery was treated as a duplicate")
		}
	}

	for _, delivery := range []int{2, 3} {
		lines := fireEvent(t, h, "evt_fixed", body)

		dup := findLine(t, lines, "duplicate")
		if got := dup["event_id"]; got != "evt_fixed" {
			t.Errorf("delivery %d duplicate event_id = %v, want evt_fixed", delivery, got)
		}
		// The payment id rides along for correlation even though it is not the key.
		if got := dup["payment_id"]; got != "pay_same_event" {
			t.Errorf("delivery %d duplicate payment_id = %v, want pay_same_event", delivery, got)
		}
		if len(lines) != 1 {
			t.Errorf("delivery %d emitted %d log lines, want exactly the duplicate line: %v", delivery, len(lines), lines)
		}
	}

	// The strongest statement of "did no work": the decision layer was consulted
	// once, and the payment was counted once, across three deliveries.
	if decider.calls != 1 {
		t.Errorf("decider called %d times across 3 identical deliveries, want 1", decider.calls)
	}
}

// TestDedupeDifferentEventIDsSamePaymentAreBothNew is the behaviour the re-key
// was built for. Under the old payment-id key the second delivery here was
// silently dropped, which made the stopping rule unreachable — so a regression
// here breaks Day 3's budget enforcement, not only Day 1's idempotency.
func TestDedupeDifferentEventIDsSamePaymentAreBothNew(t *testing.T) {
	decider := confidentStub()
	store := NewInMemoryAttemptStore()
	h := NewHandler(decider, store)

	// soft_decline budgets 2, so both deliveries stay within budget and reach the
	// decision layer. Using a category that stopped on delivery 2 would make a
	// dropped delivery and a stopped one look identical in the log.
	body := webhookBody("pay_two_events", "authentication_failed")

	for _, eventID := range []string{"evt_first", "evt_second"} {
		lines := fireEvent(t, h, eventID, body)
		for _, m := range lines {
			if m["event"] == "duplicate" {
				t.Fatalf("delivery %s was dropped as a duplicate; distinct event ids are distinct deliveries", eventID)
			}
		}
		findLine(t, lines, "payment_received")
	}

	if decider.calls != 2 {
		t.Errorf("decider called %d times, want 2 — both deliveries must reach the decision layer", decider.calls)
	}
	if got := store.Get("pay_two_events"); got != 2 {
		t.Errorf("attempt count = %d, want 2 — a dropped delivery would leave this at 1", got)
	}
}

// syncBuffer serialises writes from concurrent handlers. logOut is a plain
// package var, so the concurrent test sets it once around all goroutines rather
// than per-call the way fireEvent does.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// TestDedupeConcurrentSameEventID is the race the LoadOrStore is there to
// prevent. A Load-then-Store pair would let several goroutines all read "not
// seen" before any of them wrote, and every one of them would be processed as
// new — the same correctness bar the attempt store is held to.
func TestDedupeConcurrentSameEventID(t *testing.T) {
	const n = 20

	decider := confidentStub()
	h := NewHandler(decider, NewInMemoryAttemptStore())
	body := webhookBody("pay_concurrent", "insufficient_funds")

	out := &syncBuffer{}
	saved := logOut
	logOut = out
	defer func() { logOut = saved }()

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release together, to maximise the window the race needs
			req := httptest.NewRequest(http.MethodPost, "/webhook/payment-failed", strings.NewReader(body))
			req.Header.Set(eventIDHeader, "evt_concurrent")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 — a duplicate must still answer 200", rec.Code)
			}
		}()
	}
	close(start)
	wg.Wait()

	var received, duplicate int
	for _, l := range strings.Split(strings.TrimSpace(out.buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", l, err)
		}
		switch m["event"] {
		case "payment_received":
			received++
		case "duplicate":
			duplicate++
		}
	}

	if received != 1 {
		t.Errorf("payment_received count = %d, want exactly 1 — concurrent redeliveries were processed as new", received)
	}
	if duplicate != n-1 {
		t.Errorf("duplicate count = %d, want %d", duplicate, n-1)
	}
	if decider.calls != 1 {
		t.Errorf("decider called %d times under %d concurrent redeliveries, want 1", decider.calls, n)
	}
}
