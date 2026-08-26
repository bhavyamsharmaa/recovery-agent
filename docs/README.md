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

  `AttemptStore.Get` is deliberately unused in the request path. The stopping
  rule calls `Increment` and checks its return value, because that is one atomic
  step: a `Get`-then-`Increment` pair would let two concurrent deliveries of the
  same payment read the same count and both pass a budget with one attempt left.
  `Get` stays on the interface for tests and for Day 5's queries.

- **LLM decision layer, double-failure gap (found and fixed):** the bounded
  retry added to survive occasional Haiku output-formatting failures had no
  defined behavior if both the original call and the retry failed — a payment
  would receive no decision and no customer message. Found during a routine
  audit, not a failing test. Fixed with a conservative static fallback
  (action: `no_retry`, generic customer message, logged with
  `source: fallback_rule`) so no payment is ever left silently unresolved. See
  [issue #1](https://github.com/bhavyamsharmaa/recovery-agent/issues/1) for the
  full discovery-to-fix trail.

- **Webhook deduplication is likewise in-memory.** After a restart, a redelivery
  of an event seen before the restart is processed as new.

- **The confidence threshold is 0.75, calibrated rather than defaulted.** It was
  set against the observed score distribution (0.68–0.95) across the Day 2 test
  scenarios, so that the escalation path is genuinely exercised instead of being
  a branch that never executes. Two live-measured scenarios that score below it
  are pinned in `calibration_test.go`, with a live re-measurement test behind
  `RECOVERY_LIVE_TESTS=1`.

  Observed scores are quantised to a handful of values (0.68, 0.75, 0.78, 0.82,
  0.85) rather than continuous. 0.75 sits exactly on the boundary, and the
  comparison is `>=`, so an exact 0.75 is acted on rather than escalated. Moving
  the threshold would move a whole cluster across the line at once.

- **The confidence gate concentrates on `insufficient_funds`, by design.** Across
  every category probed, no other one has been observed producing a
  sub-threshold confidence: `soft_decline`, `bank_downtime` and `network_error`
  cluster at 0.78–0.85 however ambiguous the input is made. Those categories
  have clear-cut signal — a bank outage or a mistyped CVV has an obvious right
  response — while `insufficient_funds` genuinely trades off a retry that may be
  premature against an escalation that may be unnecessary. The gate is not
  broken for the other categories; there is simply nothing for it to catch.

- **Decision calls pin `temperature` to 0** for reproducibility: the same failed
  payment should get the same decision twice. This measurably tightened
  variance — three inputs that previously returned two or three different
  confidences across five identical calls each returned a single value
  afterwards. It is a reduction, not a guarantee: residual variance may remain,
  which is why the live calibration test asks for a majority of runs below the
  threshold rather than all of them.

## Known test-fidelity limitations

- **`TestDedupeConcurrentSameEventID` is probabilistic.** It was validated by
  reverting the deduplication to a non-atomic `Load`-then-`Store` pair: it did
  catch that regression, but not on every run — three consecutive runs passed
  with the bug present, and it failed within fifty. This is a limitation of the
  test's sensitivity, not a gap in the deduplication logic, which uses a single
  atomic `LoadOrStore`. Race detection (`go test -race`) would catch it
  deterministically but is unavailable in the current toolchain, which has no
  64-bit gcc for cgo.
