# Smart Payment Recovery Agent

An agent that detects revenue at risk, classifies why a payment failed, decides the right
intervention through an LLM, and executes a bounded, auditable decision workflow — with
measured results against a naive baseline, compliant escalation, and a complete audit trail.

Built for Razorpay AI Buildathon 2026, Track 03 — AI Revenue Recovery.

**Live demo:** https://recovery-agent-roan.vercel.app
**Backend API:** https://recovery-agent-mcyz.onrender.com
**PRD:** [`Smart-Payment-Recovery-Agent-PRD.pdf`](./docs/Smart-Payment-Recovery-Agent-PRD.pdf)

---

## What this is

A real Razorpay `payment.failed` webhook is signature-verified, deduplicated, classified by a
deterministic rule engine, checked against a per-category retry budget, reasoned about by a
real Claude API call, gated by a confidence threshold, and logged with a full audit trail —
end to end, in production, today. See [`docs/architecture.png`](./docs/architecture.png) for
the request lifecycle.

**What's real:** the entire decision pipeline — ingestion, classification, LLM reasoning,
stopping rules, compliant escalation, fallback handling — runs against real webhook traffic.

**What's simulated:** whether a decided-upon action (e.g. "retry this payment") actually
*recovers* the payment. No Razorpay retry/execution API is ever called. Recovery outcomes are
a seeded, documented statistical draw, not a real gateway result. This boundary is stated
explicitly on the dashboard's batch summary screen and in the PRD — see the PRD's Simulation
Boundary section for the full reasoning.

---

## Architecture

```
Razorpay webhook (payment.failed)
        │
        ▼
Signature + idempotency check   ── HMAC-SHA256 over raw body, dedup by event id
        │
        ▼
Rule-based classifier           ── decline code → category, no AI
        │
        ▼
Stopping-rule check             ── attempts vs. category retry budget
        │                               │
        ▼ (within budget)               ▼ (exhausted)
LLM decision layer (Claude)      escalation queue ◄─┐
        │                                            │
        ▼                                            │
Confidence gate (0.75)  ── override to escalate ─────┘
        │
        ▼
Structured log → Postgres
        │
        ▼
Dashboard (React, authenticated API)
```

A rendered version of this diagram is at [`docs/architecture.png`](./docs/architecture.png).

### Stack

| Layer | Choice |
|---|---|
| Backend | Go, standard library `net/http`, no framework |
| LLM | Anthropic Claude API (`claude-haiku-4-5`), direct HTTPS, no SDK, temperature 0 |
| Database | Postgres (Neon), idempotent embedded migrations |
| Frontend | React + TypeScript + Tailwind (Vite) |
| Deployment | Backend on Render, frontend on Vercel |
| Auth | Shared-secret header on `/api/*`; HMAC-SHA256 signature verification on the webhook |

---

## Running it locally

### Prerequisites
- Go 1.21+
- Node 18+
- A Postgres database (Neon free tier works)
- An Anthropic API key
- A Razorpay test-mode account (key id/secret + a webhook secret you choose)

### Backend

```bash
cd backend
cp .env.example .env   # fill in real values, see table below
go run ./cmd/server
```

Migrations run automatically at startup. The server refuses to start if any required
environment variable is unset — this is deliberate, not a bug.

| Variable | Purpose |
|---|---|
| `ANTHROPIC_API_KEY` | Claude API access |
| `RAZORPAY_KEY_ID` / `RAZORPAY_KEY_SECRET` | Razorpay test-mode credentials |
| `RAZORPAY_WEBHOOK_SECRET` | Used to verify the `X-Razorpay-Signature` header |
| `DATABASE_URL` | Postgres connection string |
| `API_ACCESS_KEY` | Shared secret required on every `/api/*` request |
| `FRONTEND_ORIGIN` | CORS allow-origin for the dashboard |
| `PUBLIC_BASE_URL` | This instance's own public URL — used when the dashboard triggers a batch run |
| `PORT` | Optional, defaults to `8080` |

### Frontend

```bash
cd frontend
cp .env.example .env   # VITE_API_BASE_URL, VITE_API_ACCESS_KEY
npm install
npm run dev
```

### Simulating traffic without a real Razorpay account

```bash
go run ./cmd/simulate --scenario insufficient_funds
go run ./cmd/run-batch --size 100
```

Both sign their requests with `RAZORPAY_WEBHOOK_SECRET`, exercising the exact same signature
verification path a real Razorpay delivery would.

### Tests

```bash
go test ./...                          # unit tests, no live dependencies
RECOVERY_LIVE_TESTS=1 go test ./...    # includes tests requiring a real database
cd frontend && npm run test            # recovery-rate calculation tests
```

---

## Known limitations

Stated plainly, as deliberate scope decisions or documented tradeoffs — not gaps discovered
after the fact.

- **No real payment execution.** See "What this is" above and the PRD's Simulation Boundary
  section. `RAZORPAY_KEY_ID`/`RAZORPAY_KEY_SECRET` are configured but not called by any code
  path — real gateway execution is out of scope for this build.
- **`API_ACCESS_KEY` is bundled into the frontend's shipped JavaScript.** Vite inlines
  `VITE_`-prefixed env vars into the client bundle by design. This gate stops opportunistic
  scanning of the bare API; it does not resist someone who loads the dashboard and inspects
  its network requests. A true fix needs a server-side proxy, out of scope for this build.
- **`AttemptStore.Increment` has no error return.** A database failure is signaled via a
  fail-closed sentinel rather than a distinct error type, since the interface predates persistence.
- **Outcomes are only written by batch runs.** Real webhook traffic gets a full decision trace
  but no outcome row — nothing observes whether a real retry succeeded, since no real retry
  is executed.
- **The naive baseline (20%) is deliberately lower than the ~30% naive-retry figure commonly
  cited for this problem space.** This build's baseline blindly retries every category,
  including ones this system would never retry — the gap reflects category-aware routing
  avoiding wasted retries on structurally dead declines.
- **A confidence-gate override changes the decided action but not the model's original
  customer-facing message.** Observed on real production data; not yet fixed.
- **No webhook-secret rotation path.** Rotating `RAZORPAY_WEBHOOK_SECRET` causes a brief
  window of rejected deliveries until Razorpay's retry succeeds against the new secret.
- **No rate limiting on the dashboard API** beyond a size cap and a run-lock on batch triggers.
- **Render's free tier can idle-sleep**, adding latency to the first request after a period
  of inactivity.

---

## Repository layout

```
backend/
  cmd/
    server/          entrypoint
    simulate/        single-scenario webhook simulator
    run-batch/       batch simulation CLI
    trace-payment/   CLI trace tool
  internal/
    ingest/          webhook handling, classification wiring, stopping rules, confidence gate
    classify/        rule-based classifier
    decide/          LLM client, prompt construction, validation
    simulate/        seeded outcome simulation
    batch/           batch aggregation
    api/             dashboard read/write API + auth middleware
    trace/           shared query layer (used by both the API and the CLI)
    db/              connection + migration runner
  migrations/
frontend/
  src/
    components/
    api.ts           typed API client
    batchMath.ts     recovery-rate calculation (the one place with real client-side math)
docs/
  architecture.png
  Smart-Payment-Recovery-Agent-PRD.pdf
```

---

## Found and fixed during the build

Two issues worth naming, each closed with a GitHub issue opened before the fix, a dedicated
branch, and a merged PR:

1. **Double LLM failure left a payment with no decision at all.** The bounded retry on the
   LLM call had no defined behavior if the retry also failed. Fixed with a static, conservative
   fallback (`no_retry`, `source: fallback_rule`) rather than a third model call.
2. **A hardcoded `localhost` default silently broke every deployed batch run.** The
   dashboard's batch-trigger path never set the webhook URL it posts to, so every payment in
   a 100-payment batch failed to send while the run still reported a clean success with
   all-zero figures. Diagnosed by elapsed-time evidence, reproduced locally, and fixed by
   deriving the correct public URL from configuration.

Full detail on both is in the PRD.
