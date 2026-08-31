An agent that classifies failed payments and recovers them with policy-driven retries.

## Day 7 API surface

Every route the dashboard talks to. Three of them write.

| Method | Route | Writes? | What it does |
|---|---|---|---|
| GET | `/api/payments` | no | Failed payments, most recently seen first; `?category=` and `?action=` filter server-side |
| GET | `/api/payments/{id}` | no | One payment's full history; 404 with a JSON body if never ingested |
| GET | `/api/escalations` | no | Payments whose latest decision was `escalate` or `no_retry`, with reasoning inline |
| GET | `/api/batch-runs` | no | Batch history, most recent first, including runs that never completed |
| GET | `/api/batch-runs/latest` | no | The most recently **completed** run; 404 when none exists |
| POST | `/api/batch-runs` | **YES** | Runs a batch: writes `batch_runs` and `outcomes`, and spends model budget |
| POST | `/api/simulate/failure?category=X` | **YES** | Fires one real webhook and returns the decision |
| POST | `/api/simulate/duplicate` | **YES** | Delivers one event id twice and reports both results |
| POST | `/api/simulate/llm-failure` | **YES** | Forces the decision layer to fail for that one request |

### Every /api/ route requires a shared secret

All nine routes above — reads and writes alike, no exceptions — are behind
`X-API-Key`, checked against `API_ACCESS_KEY` before the request reaches a
handler. A missing or wrong header is `401` with a JSON body.

The check is middleware wrapping the whole `/api/` subtree
([backend/internal/api/auth.go](../backend/internal/api/auth.go)), not a call
inside each handler. A per-handler check is one route away from being
forgotten, and the route most likely to be forgotten is the newest one; here a
route registered on the mux is covered by construction.

**The server refuses to start when `API_ACCESS_KEY` is unset.** `NewAuth`
returns an error and `main` exits. A server that booted without a key and served
anyway would look entirely healthy while exposing every payment record, and
nothing in a successful response would reveal the check was not running.

Three details that are deliberate:

- **Comparison is constant-time** (`subtle.ConstantTimeCompare`). `==` returns
  at the first differing byte, so how long a rejection takes leaks how long a
  correct prefix was.
- **The 401 does not say whether the header was missing or wrong.** Both mean
  "you may not have this"; distinguishing them confirms the mechanism to someone
  probing. The distinction is logged instead, as `api_auth_rejected` with
  `header_present`, because an operator debugging a 401 does need it.
- **CORS preflight passes without the key**, and must: a browser sends `OPTIONS`
  with no custom headers, since the preflight is what asks permission to send
  `X-API-Key` at all. It discloses nothing and is identical either way.

`/webhook/payment-failed` is **not** behind this gate. Razorpay cannot send our
header, and that endpoint's authenticity problem is signature verification,
which it now has — see below. The two mechanisms are separate on purpose: this
secret authenticates our own dashboard, that one authenticates Razorpay.

### The webhook verifies Razorpay's signature

`/webhook/payment-failed` requires a valid `X-Razorpay-Signature`: HMAC-SHA256
of the raw request body under `RAZORPAY_WEBHOOK_SECRET`, hex-encoded, compared
in constant time. A missing or wrong signature is `401` with a JSON body, logged
as `webhook_signature_invalid`. **The server refuses to start when
`RAZORPAY_WEBHOOK_SECRET` is unset**, like `API_ACCESS_KEY`.

`RAZORPAY_WEBHOOK_SECRET` is not `RAZORPAY_KEY_ID`/`RAZORPAY_KEY_SECRET`. Those
are API credentials for calling Razorpay; this one is set separately in their
dashboard when the webhook is registered, and is the only value that can verify
a delivery came from them. Signing with the API secret would verify nothing.

**Verification runs on the raw bytes, before parsing.** The handler reads the
body with `io.ReadAll`, verifies those exact bytes, and only then unmarshals the
same slice. Parsing first and re-serialising to verify would hash a different
byte sequence — Go's encoder orders keys, drops insignificant whitespace and
reformats numbers — and every genuine delivery would fail its own signature.
`TestSemanticallyEqualBodyWithDifferentBytesDoesNotVerify` pins this: four
bodies that `json.Unmarshal` treats as identical all produce different
signatures.

**Rejection precedes everything.** No parsing, no classification, no attempt
counting, no database write, and the event id is not consumed — recording it
would make a later genuine delivery of that event look like a duplicate and be
silently dropped. The tests assert the decider was never called and the stores
are empty, not merely that the status was 401: a handler that rejected *after*
doing all that work would pass a status-only assertion.

A handler built without `WithVerifier` **rejects everything**. The default is a
verifier holding a fresh random secret, not a nil that means "skip the check" —
that would make "verification is off" both the default and invisible, on the one
endpoint facing the public internet. There is no test-only bypass; the tests
sign their requests like everyone else.

The body is bounded at 1 MiB (`http.MaxBytesReader`) before it is read, since
the signature cannot be checked without buffering and the endpoint is
unauthenticated until it is.

**The rejection discloses nothing.** No secret, no expected signature, and no
payment id — the body has not been parsed, and parsing unverified input to
enrich a log line is doing exactly the work the rejection exists to avoid. What
is logged is `header_present` and `signature_length`, which separate "the sender
is not signing at all" (a misconfiguration) from "signed with the wrong secret"
(a key mismatch or a forgery), plus `body_bytes` — a mismatch when everything
else looks right points at a proxy re-encoding the body in transit, which is
otherwise invisible.

**Local development signs with the same variable.** `internal/simulate.Send`
computes the signature the handler expects, so `cmd/simulate`, `cmd/run-batch`
and the dashboard's control panel all work end to end without a Razorpay
account. The control panel's in-process dispatch signs too, rather than
bypassing the check — a demo endpoint that skipped verification would exercise a
path production does not have.

One consequence worth stating: `Sign` is shared between the sender and the
receiver, so if it is wrong, the simulator and the tests are wrong in exactly
the same way and would still agree. The real check is against Razorpay's own
deliveries, and no local reimplementation performs that check for us.

#### What this is not

One shared secret is not an operator session. It does not identify who is
calling, cannot be revoked for one person without rotating it for everyone, and
appears in full in any log that records request headers. On the frontend it is
a build-time value baked into the bundle by Vite, so **anyone who can load the
dashboard can read the key out of the JavaScript** — the moment the dashboard is
public, the key is too.

It closes the gap between "anyone who finds the port" and "anyone who has been
given the dashboard", which is the one worth closing first. Still outstanding
before this is public:

- a real operator session with per-user identity and revocation,
- `FRONTEND_ORIGIN` tightened from its `http://localhost:5173` default,
- rate limiting — the key bounds *who* can spend model budget, not how fast,
- and a decision about whether `/api/simulate/` should exist outside a demo
  build at all.

`POST /api/batch-runs` also caps `size` at 200 and serialises runs behind a
lock, returning `409` if one is already in progress.

### The forced decision failure is request-scoped, and that is the point

`POST /api/simulate/llm-failure` makes the decision layer fail for its own
request only. Day 4 had a `FORCE_DECIDE_FAILURE` environment variable for this
and it was removed before merge, because a global switch that breaks decisions is
one deployment mistake away from breaking them in production with nothing in a
request to reveal it.

The replacement is a value on the request's `context`, set by that one endpoint
(`ingest.WithForcedDecideFailure`) and read by `ingest.ForcedFailureDecider`,
which is otherwise a pass-through to the real client. The key type is unexported,
so no package outside `ingest` can set it even by accident, and a context value
cannot cross an HTTP boundary — which is why the simulate endpoints dispatch
in-process rather than over a loopback.

`TestWebhookCannotBeForcedToFailByAnyInput` pins the guarantee: it drives the
real handler with headers, query parameters and body fields named after the
mechanism, including all of them at once, and asserts the model was reached every
time. `TestSimulateEndpointsDoNotLeakTheForcedFailure` covers the subtler
variant — that forcing one failure does not affect the next request.

### Outcomes on demo traffic

Only `cmd/run-batch` and `POST /api/batch-runs` write `outcomes`. A payment that
arrives through `/webhook/payment-failed`, including via the control panel's
simulate buttons, gets a decision and no outcome — correctly, because nothing has
confirmed what happened to it. The detail view says "no outcome recorded yet" for
those, which is accurate rather than a gap.

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

- **The JSON API's authentication is one shared secret, not a session
  (partially addressed).** Every `/api/` route now requires `X-API-Key` and the
  server will not start without `API_ACCESS_KEY` set — see "Every /api/ route
  requires a shared secret" above, which also records what a single shared
  secret still does not give you: no per-user identity, no revocation without
  rotating for everyone, and a key readable in the frontend bundle by anyone who
  can load the dashboard. The permitted CORS origin —
  `http://localhost:5173` by default, overridable with `FRONTEND_ORIGIN` —
  still wants tightening to the real frontend before deployment. The CORS
  headers are attached only to `/api/` routes; the webhook endpoint is called by
  Razorpay, not a browser, advertises no origin, and is deliberately outside the
  key gate because its authenticity problem is signature verification, which it
  now has — see "The webhook verifies Razorpay's signature" above.

- **The webhook's signature secret is a single static value with no rotation
  path.** Verification is against one `RAZORPAY_WEBHOOK_SECRET`, so rotating it
  means a window where either the old or the new secret is rejected: Razorpay
  signs with whatever their dashboard holds, and this verifies against whatever
  the process was started with. Accepting two secrets during a rollover is the
  fix and is not built. In practice a rotation means a brief spike of
  `webhook_signature_invalid` and redeliveries, which Razorpay's own retries
  recover once both sides agree — the deliveries are not lost, but they are
  delayed, and a long enough mismatch outlasts the retry schedule.

- **`API_ACCESS_KEY` is bundled into the frontend's shipped JavaScript by Vite
  at build time**, since `VITE_`-prefixed env vars are inlined into the client
  bundle by design. This means the shared-secret gate protects against
  opportunistic scanning of the bare API by someone who doesn't know it exists —
  it does **not** protect against someone who has legitimate access to the
  dashboard URL extracting the key from browser dev tools and calling the API
  directly with it. A true fix would require a server-side proxy
  (backend-for-frontend) so the secret never reaches the browser at all — out of
  scope for this project's timeline. Accepted as a documented tradeoff: this
  gate's job is to stop casual/automated discovery of an open port, not to
  resist a deliberate user of the dashboard itself.

- **The listen port comes from `PORT`, defaulting to 8080.** It was hardcoded,
  which fails on any host that assigns a port dynamically: the process binds
  somewhere the platform is not routing to, then fails a health check while its
  own logs look perfectly healthy. The `server_up` line prints the address it
  actually bound, so the log answers the question rather than implying it.

- **SIGTERM shuts down gracefully, with a 10s budget.** The listener stops
  accepting, in-flight requests finish, then the process exits. This matters
  most for a webhook delivery caught mid-processing: it has already been counted
  as an attempt and may have spent a model call, so killing it there leaves a
  payment recorded with no decision, and Razorpay redelivers into a system that
  already charged an attempt against it.

  The 10s bound is deliberate and it is not enough for everything. **A batch run
  holds its request open far longer and will be cut off**, logged as
  `graceful shutdown incomplete`. That is the right trade: hosts send SIGKILL on
  their own deadline regardless, so an unbounded wait would hand the decision to
  the host rather than making it here. Verified by signalling the server with a
  batch mid-flight — the request returned 200, then `shutdown_complete`.

  SIGINT is handled identically, which is what makes this testable on Windows:
  a console CTRL_BREAK arrives as SIGINT and exercises the same path.

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

- **`outcomes` is written only by `cmd/run-batch`, never by live webhook
  traffic.** The table had no writer at all until batch runs arrived; it now has
  exactly one, and it is the simulated one. A payment that reaches the system
  through a real webhook still gets a decision and no outcome, so
  `cmd/trace-payment` and the detail view continue to say "no outcome recorded
  yet" for it — correctly, because nothing has confirmed what happened to it.
  Only payments generated inside a batch run carry an outcome row, and every one
  of those is a seeded coin flip rather than an observation. The consequence
  worth stating plainly: this system still does not learn from production
  traffic. It can only score a policy against a simulation of one.

  Outcome rows carry `decision_id`, not just `payment_id`, so an outcome is
  attached to the specific decision it followed rather than to the payment as a
  whole — a payment with three decisions would otherwise have an outcome nobody
  could attribute.

- **A batch run's ids are deliberately NOT derived from its seed.** The scenario
  mix, the amounts and the outcome draws all come from `--seed`, which is what
  makes the rupee figures reproducible. The payment and event ids come from the
  clock instead. Seeding them too would look more reproducible and would be
  worse: a rerun would replay the same payment ids into a database that still
  remembers them, so attempt counts would climb, the stopping rule would fire on
  payments that had budget the first time, and the second run would be measuring
  a different system than the first.

  Each payment's outcome draw is derived from `(seed, stream, index)` rather
  than taken from one shared stream, because `escalate` and `no_retry` consume
  no draw. With a shared stream, one payment receiving a different action
  between runs would shift every subsequent draw and the two runs would diverge
  entirely; deriving per payment confines the difference to the payment it
  happened to.

  **This was observed, not merely anticipated.** Two runs at seed 20260829
  produced identical stored figures to six decimal places — `at_risk=248121600`,
  `recovered=81544300`, `rate=0.328647`, `baseline_rate=0.240844` — while their
  outcome mixes differed by one payment (33/33 versus 32/34 still_failed /
  escalated_pending). That is the documented residual variance of the decision
  layer at `temperature: 0` showing up in a batch: one payment was routed
  differently, and because the draws are per payment, nothing downstream of it
  moved. The money totals matched because that payment was not recovered under
  either action. **A same-seed rerun is therefore reproducible in its inputs and
  its scoring, but not perfectly reproducible in its decisions** — the seed
  pins everything this project controls, and the model is not one of those
  things.

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

- **Vitest's default forked-worker pool hangs on this machine (worked around, root
  cause unknown).** `vitest run` with the default `pool: 'forks'` never completes
  its worker handshake here: every run dies after 60s with
  `[vitest-pool-runner]: Timeout waiting for worker to respond` and then reports
  `Test Files no tests` / `Tests no tests`. **That output reads as a pass to
  anyone skimming**, which is the worst failure mode a suite can have — a green-
  looking run that executed nothing.

  Worked around in `frontend/vite.config.ts` with `pool: 'threads'` and
  `fileParallelism: false`; the suite then runs in about 4 seconds. Serial
  threads were needed, not just threads: two workers racing to hand-shake in
  parallel both hit the timeout.

  **That was not sufficient either, and the residue was worse than the original.**
  Serialising the files still starts one worker per file, and the second one —
  `BatchSummary.test.tsx`, the only file needing jsdom — kept exceeding the 60s
  handshake budget on its own. The run then reported `Test Files 1 passed (1)` /
  `Tests 36 passed (36)`, a green summary, with the timeout demoted to an
  "Unhandled Error" block above it. The `.tsx` file's 11 tests silently did not
  run. This is the same hazard as the "no tests" output and harder to catch,
  because the tally is non-zero and rising.

  Reduced by holding the run to a single worker — `maxWorkers: 1` (`minWorkers`
  is not in the config type and fails `tsc -b`). The timeout is hit during
  worker *startup*, so starting fewer workers helps where raising the timeout
  would only make a broken run slower. The suite runs both files, 47 tests, in
  about 3 seconds.

  **`maxWorkers: 1` made it rare, not impossible.** A later run dropped the
  jsdom file again — 36 tests, one "Unhandled Error", exit 0 — so it depends on
  what else the machine is doing. Tuning a race is not a fix, so the race is no
  longer allowed to pass silently: `npm test` runs
  `frontend/scripts/check-suite.mjs` after vitest, which compares the files that
  reported in the JSON output against the files on disk matching the same
  `include` glob, and exits 1 naming any that did not run. It is compared
  against the glob rather than a hardcoded count so that adding a test file
  needs no change and deleting one cannot silently lower the bar.

  That guard was watched to fail before being trusted, the same way the dedupe
  concurrency test was: running vitest against only `batchMath.test.ts` and
  then invoking it reports `1 test file(s) did not run:
  src/components/BatchSummary.test.tsx` and exits 1.

  These are top-level `test` options in Vitest 4. The v3 spelling,
  `poolOptions.threads.singleThread`, was removed in that release and is
  **ignored without erroring** — it was tried first, and the suite went green
  for a different reason than the config claimed. If this ever regresses, check
  that the option still exists before trusting it.

  This is recorded as a machine-specific environment quirk and was deliberately
  not investigated further. It has not been reproduced on another machine, and it
  may not occur in CI. If the suite ever reports "no tests" again, this is the
  first thing to check. Related: `defineConfig` must be imported from
  `vitest/config` rather than `vite`, because vite's own type rejects the `test`
  key and `npm run build` fails type-checking.

- **The payments feed table clipped its rightmost columns between 640px and
  ~830px (found and fixed).** The table has a minimum content width of 781px and
  its wrapper carried `overflow-hidden` for the rounded corners, so wherever the
  container was narrower the Amount and Attempts columns were simply not drawn.
  The page never scrolled sideways — `documentElement.scrollWidth` equalled
  `window.innerWidth` throughout — so nothing revealed it. Day 6's responsive
  check measured 1280, 640, 639 and 375 and stepped straight over the range;
  it was found during Day 7's sweep of the three new panels.

  Fixed by moving the card/table switch to a `feed` breakpoint at 840px
  (`--breakpoint-feed` in `frontend/src/index.css`) instead of Tailwind's `sm`.
  Cards exist precisely so a narrow viewport never needs horizontal scrolling, so
  showing them wherever the table cannot fully render is the fix that matches the
  design rather than working around it.

  **840 is measured, not chosen.** The table needs 781px and the page contributes
  a 50px gutter (`max-w-6xl` with `px-4`/`sm:px-6`), so the first viewport at
  which it fits is 831px; 840 adds headroom. The obvious guess of 800px — the
  table's own width rounded up — was tried first and left a 30px band with the
  Attempts column still off-screen, because it ignored the gutter. That is why
  the sweep asserts against the *wrapper's* width and against each rendered
  header's position, not against the viewport.

  The wrapper is now `overflow-x-auto` rather than `overflow-hidden`. The
  breakpoint is what actually prevents a cramped table, so this should never
  engage; it is the failure mode if a column is ever added without moving the
  breakpoint, turning silent truncation into a visible scrollbar.

  Verified by sweeping every 20px from 375 to 1280, plus the boundary widths
  either side of the switch: 54 widths, zero clipped columns, zero horizontal
  page scroll, and one of the two layouts always active. The three Day 7 panels
  were measured across the same range with zero overflowing children.


## Known test-fidelity limitations

- **`TestDedupeConcurrentSameEventID` is probabilistic.** It was validated by
  reverting the deduplication to a non-atomic `Load`-then-`Store` pair: it did
  catch that regression, but not on every run — three consecutive runs passed
  with the bug present, and it failed within fifty. This is a limitation of the
  test's sensitivity, not a gap in the deduplication logic, which uses a single
  atomic `LoadOrStore`. Race detection (`go test -race`) would catch it
  deterministically but is unavailable in the current toolchain, which has no
  64-bit gcc for cgo.
