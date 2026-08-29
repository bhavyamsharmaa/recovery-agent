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

- **`failed_payments.amount_paise` reflects the most recent delivery's amount,
  not the first.** The row is refreshed on every delivery, because a payment
  that fails again may fail differently and the decision being made is about
  the latest failure. This is correct for real Razorpay traffic, where the
  amount is stable across deliveries of one payment. It can look like drift
  when testing against `cmd/simulate`, which randomises the amount per call —
  so a payment traced after two simulated deliveries shows the second call's
  amount, not the first. The same applies to the `error_*` and `category`
  columns.

- **The tables have no `ON DELETE CASCADE`.** `decisions` and `outcomes`
  reference `failed_payments`, so a plain
  `DELETE FROM failed_payments WHERE ...` fails on the foreign key whenever the
  payment has decisions recorded against it. Delete children first, in a
  transaction. This is deliberate: a decision that silently disappears with its
  payment is an audit trail that cannot be trusted.

- **`AttemptStore` cannot report an error, so `Increment` fails closed via a
  sentinel.** The interface returns a plain `int` — it was defined on Day 3
  against an in-memory map, where nothing could fail. The Postgres
  implementation can, and answering `0` would read as "no attempts yet" and wave
  a `hard_decline` (budget 0) straight past the check that exists to stop it. So
  a database failure returns `math.MaxInt`, which exceeds every budget and stops
  the payment. **A database outage therefore appears as a spike in
  `retry_budget_exhausted` escalations, not as errors** — the raw cause is on
  stderr, and `attempt_count: 9223372036854775807` in a
  `stopping_rule_triggered` line is the tell. This is design debt: the
  interface should carry `error` and `context`, and is left alone only to keep
  the Day 3 stopping-rule and confidence-gate code untouched.

- **Counting an attempt requires the payment to have been recorded first.**
  `failed_payments.category` carries a `category_not_empty` CHECK
  (migration 002), because `TEXT NOT NULL` accepts `''` and an empty category is
  never useful — the retry budget is looked up by it. `Increment` is a plain
  `UPDATE`, not an upsert, because PostgreSQL evaluates a CHECK against the
  proposed row *before* resolving `ON CONFLICT`, so an upsert carrying
  placeholder values is rejected even when the row already exists and only the
  UPDATE would have run. If `RecordPayment` fails, a `record_payment_failed`
  log line is emitted, `Increment` then matches no row, and the payment fails
  closed and escalates. Nothing is written at all — which is the point: the
  earlier behaviour was a blank-but-valid row that self-healed only if that
  payment happened to fail again.

- **Webhook deduplication was in-memory and is now in Postgres (fixed).** It
  lived in a `sync.Map` on the handler, which was consistent while attempt
  counts were in memory beside it — a restart forgot both together. Day 5 moved
  the counts to Postgres and left the dedupe behind, and *that asymmetry was the
  bug*: a restart kept the attempt count for an event while forgetting the event
  had been handled, so a redelivery was processed as new and incremented a count
  for a delivery that was not new. Fixed behind an `ingest.EventStore` interface
  with a `webhook_events` table (migration 003) and a single
  `INSERT ... ON CONFLICT (event_id) DO NOTHING RETURNING event_id` — one
  statement, so the check and the record cannot be interleaved.
  `InMemoryEventStore` is retained for tests and for running without a database.

  The concurrency test was itself wrong at first and passed against a
  deliberately naive `SELECT`-then-`INSERT`: `database/sql` opens connections
  lazily, so twenty goroutines released at once against a cold pool queue behind
  connection setup and never overlap. With the pool warmed to twenty live
  connections first, the naive version fails as it should — 17, 12 and 17 of 20
  deliveries treated as new across three runs — and the atomic version passes.
  A concurrency test that has not been watched to fail is not evidence of
  anything.

  `PostgresEventStore.RecordEvent` fails *open* on a database error — the
  delivery is processed — which is the opposite of `AttemptStore.Increment`
  fails-closed above, deliberately. An unreadable attempt count must stop the
  payment, because letting it through spends a budget that protects the
  customer. An unmakeable dedupe check must let the payment through, because
  dropping it discards a real failure permanently. The worst case here is one
  double-counted attempt, which the retry budget still bounds. Such failures are
  logged as `dedupe_check_failed` with `processed_anyway: true`.

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

- **The read-only JSON API has no authentication.** `internal/api` serves
  `GET /api/payments` and `GET /api/payments/{id}` to anyone who can reach port
  8080. Those responses carry every failed payment's amount, error detail, the
  model's reasoning, and the message sent to the customer. This is acceptable
  for local development against a development database and for nothing else:
  before deployment it needs an authenticated operator session, and the
  permitted CORS origin — `http://localhost:5173` by default, overridable with
  `FRONTEND_ORIGIN` — tightened to the real frontend. The endpoints are
  read-only, which limits the exposure to disclosure rather than mutation, but
  disclosure of payment data is not a small thing. The CORS headers are attached
  only to `/api/` routes; the webhook endpoint is called by Razorpay, not a
  browser, and advertises no origin.

- **The API's queries are shared with `cmd/trace-payment`, in `internal/trace`.**
  They began in the command, which could not be imported because it is
  `package main`. Rather than copy them, they moved to
  [backend/internal/trace/trace.go](../backend/internal/trace/trace.go) and the
  command became a formatter over it. Both the terminal trace and the dashboard
  therefore answer from identical SQL — if the two ever disagree about a
  payment's history, that is a bug in one of the renderers, not a difference of
  opinion between two copies of a query. Add columns there, not in a second
  place.

- **`GET /api/payments` returns every matching row, unpaginated.** The table is
  small today. A `LIMIT` and a cursor over `(last_seen_at, payment_id)` — which
  is already the sort order precisely so that it can become one — is the change
  to make before it is not.

- **Nothing writes the `outcomes` table, so the dashboard cannot show one.**
  Both `cmd/trace-payment` and the detail view say "no outcome recorded yet"
  rather than inventing a status, which is honest but does mean no part of the
  system yet learns whether a decision actually recovered the payment. The read
  path is built and waiting on both sides.

- **Recovery outcomes are simulated — no gateway is ever called.** `internal/simulate`
  decides whether a decision "recovered" a payment with a seeded coin flip
  against probabilities written down by hand. No card is charged and no retry is
  attempted against Razorpay. The purpose is to compare two routing policies over
  the same payments and the same random stream; it measures a policy, not an
  outcome.

  | Action | Simulated recovery probability | Why this position in the ordering |
  |---|---|---|
  | `suggest_alternate_method` | 0.65 | A different instrument sidesteps whatever is wrong with the first one. |
  | `retry_now` | 0.55 | A customer-input error (wrong CVV/OTP) is fixable immediately. |
  | `retry_delayed` | 0.40 | Waiting on an uncertain bank-side condition to clear is the weakest bet. |
  | `escalate` | *(none)* | No automated attempt is made, so there is no result to draw. |
  | `no_retry` | *(none)* | Likewise. |
  | *naive baseline* | 0.20 | Blind retry of every category, including ones that cannot succeed. |

  The ordering is the claim being made about the world and is pinned by a test;
  the exact digits are declared assumptions and may be revised. `escalate` and
  `no_retry` have no entry rather than an entry of `0`, because "no attempt was
  made" and "an attempt was made and never succeeds" are different statements —
  the same distinction the `NULL`-versus-`0` confidence rule preserves elsewhere.
  Both always resolve to `escalated_pending`, which a test asserts across 1000
  calls each.

  Recovery outcomes are simulated (no real gateway execution) using the
  category-appropriate probabilities above, chosen as reasoned estimates,
  not measured real-world rates. The naive baseline (20%) is deliberately
  lower than the ~30% naive-retry recovery rate cited in the PRD, because
  this baseline blindly retries every category including hard_decline —
  failures this system would never retry at all — while the PRD's cited
  figure describes retriable failures more broadly. The gap between the
  two numbers reflects category-aware routing avoiding wasted retries on
  structurally-dead declines, not an inconsistency.

  Every batch run stores its `rng_seed` (NOT NULL, on `batch_runs`), so any
  reported rupee figure can be regenerated exactly. A determinism test asserts
  that two runs with one seed agree at every position, not merely in aggregate.
  This is a scope boundary rather than a placeholder: the day something here
  calls a real gateway, these probabilities must be deleted rather than kept
  beside it, because a mixture of measured and invented outcomes in one table is
  worse than either alone.

## Known test-fidelity limitations

- **`TestDedupeConcurrentSameEventID` is probabilistic.** It was validated by
  reverting the deduplication to a non-atomic `Load`-then-`Store` pair: it did
  catch that regression, but not on every run — three consecutive runs passed
  with the bug present, and it failed within fifty. This is a limitation of the
  test's sensitivity, not a gap in the deduplication logic, which uses a single
  atomic `LoadOrStore`. Race detection (`go test -race`) would catch it
  deterministically but is unavailable in the current toolchain, which has no
  64-bit gcc for cgo.
