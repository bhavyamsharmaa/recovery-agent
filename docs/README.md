An agent that classifies failed payments and recovers them with policy-driven retries.

## Known limitations

- **Attempt counters are in-memory and reset on server restart.** They live
  behind the `ingest.AttemptStore` interface
  ([backend/internal/ingest/attempts.go](../backend/internal/ingest/attempts.go)),
  whose only implementation today is `InMemoryAttemptStore`. Accepted through
  Day 4; a Postgres implementation of the same interface replaces it from Day 5,
  which is a one-line change at the constructor in `cmd/server/main.go`. A
  restart before then silently zeroes every counter, so a payment that had
  exhausted its budget becomes eligible again.

- **The budget check reads and writes the counter in two steps.** The stopping
  rule calls `Get` to read the count and `Increment` only once the payment is
  going to be acted on, so a stopped delivery does not consume an attempt it
  never got. The two calls are individually atomic but not atomic together: two
  concurrent deliveries of the same payment can both read the same count and
  both proceed, allowing one more attempt than the budget permits. Concurrent
  redeliveries of the *same event* are still caught by deduplication; this
  affects genuine simultaneous failures of one payment only.

- **Webhook deduplication is likewise in-memory.** After a restart, a redelivery
  of an event seen before the restart is processed as new.

- **The confidence threshold is 0.75, calibrated rather than defaulted.** It was
  set against the observed score distribution (0.68–0.95) across the Day 2 test
  scenarios, so that the escalation path is genuinely exercised instead of being
  a branch that never executes. Two live-measured scenarios that score below it
  are pinned in `calibration_test.go`, with a live re-measurement test behind
  `RECOVERY_LIVE_TESTS=1`.

  Observed scores are quantised to a handful of values (0.68, 0.75, 0.78, 0.82,
  0.85) rather than continuous, and are not stable run to run: the same input
  can return 0.68 once and 0.78 the next time. 0.75 sits exactly on the
  boundary, and the comparison is `>=`, so an exact 0.75 is acted on rather than
  escalated. Moving the threshold would move a whole cluster across the line at
  once.

## Known test-fidelity limitations

- **`TestDedupeConcurrentSameEventID` is probabilistic.** It was validated by
  reverting the deduplication to a non-atomic `Load`-then-`Store` pair: it did
  catch that regression, but not on every run — three consecutive runs passed
  with the bug present, and it failed within fifty. This is a limitation of the
  test's sensitivity, not a gap in the deduplication logic, which uses a single
  atomic `LoadOrStore`. Race detection (`go test -race`) would catch it
  deterministically but is unavailable in the current toolchain, which has no
  64-bit gcc for cgo.
