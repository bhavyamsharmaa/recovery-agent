# Recovery Agent Dashboard

A read-only view of what the recovery agent saw and decided: which payments
failed, what the agent chose to do about each one, and why.

Two views, switched by state rather than a router — a feed of failed payments,
and one payment's full decision history. Every number on screen comes from the
backend's `/api/` endpoints; there is no mock data anywhere in the app.

## Running it

The dashboard reads from the Go backend and does nothing useful without it, so
start that first:

```sh
cd ../backend
go run ./cmd/server        # needs DATABASE_URL and ANTHROPIC_API_KEY
```

Then, in this directory:

```sh
npm install
npm run dev                # http://localhost:5173
```

If the table shows "Could not reach the backend", the server is not listening on
port 8080 — that message is specifically about a request that got no answer at
all, as opposed to one the backend refused.

To populate an empty database, fire some webhooks at the running server:

```sh
cd ../backend
go run ./cmd/simulate --scenario insufficient_funds --count 3
```

## Configuration

One variable, in `.env` (gitignored; `.env.example` is the tracked copy):

```
VITE_API_BASE_URL=http://localhost:8080
```

The backend must allow this app's origin. It permits `http://localhost:5173` by
default and reads `FRONTEND_ORIGIN` to override that, so if Vite starts on
another port — it does when 5173 is already taken — set `FRONTEND_ORIGIN` on the
backend to match, or the browser will block the requests as a CORS failure with
no visible error.

## Scripts

| Command | What it does |
|---|---|
| `npm run dev` | Dev server with HMR |
| `npm run build` | Type-check (`tsc -b`) and build to `dist/` |
| `npm run lint` | oxlint |
| `npm run preview` | Serve the built `dist/` |

## Layout

```
src/
  api.ts                     typed client + wire types for the two endpoints
  format.ts                  rupees, confidence, timestamps — shared by both views
  App.tsx                    the two views and the state that switches them
  components/
    PaymentsFeed.tsx         table (≥640px) and cards (<640px) from one cell definition
    PaymentDetail.tsx        payment facts, decision timeline, outcomes
    Filters.tsx              category and action dropdowns
    ActionBadge.tsx          colours an action by whether it ends automated handling
    SourceBadge.tsx          names which layer decided, and what that implies
```

## Things worth knowing before changing it

**Wire types are snake_case on purpose.** `PaymentSummary` and `PaymentDetail`
in `api.ts` are the Go structs' JSON shapes exactly. Renaming the fields to
camelCase would mean a mapping layer whose only job is to disagree with the
backend about what a column is called.

**Null is not zero.** A `confidence` of `null` means no model stood behind that
decision — a stopping rule or the static fallback answered. The UI renders `—`
in the feed and `N/A` in the detail view, never `0` and never blank. This
distinction is load-bearing in the database (see the backend's `docs/README.md`)
and the UI must not erase it.

**Filtering is server-side.** The feed never holds the unfiltered table; every
filter change re-queries `/api/payments`. Responses from a superseded filter are
dropped so a slow answer cannot overwrite a faster later one.

**Timestamps render in UTC**, labelled, so a screenshot of this UI can be
compared directly against `cmd/trace-payment` output and the raw columns.

**The feed renders two layouts from one definition.** `cellsFor()` in
`PaymentsFeed.tsx` describes the six fields once; the table and the card list
both map over it. Add a column there, not in two places. Both trees are in the
DOM at every width with CSS showing one — fine at this row count, worth
revisiting if the feed ever paginates into the thousands.

**No router.** Two views and one nullable string did not justify the dependency.
The costs are real and accepted: no shareable URL for a payment, and the
browser's back button leaves the app rather than returning to the feed. If
either starts mattering, that is the moment to add react-router.

## Not built

- **Outcomes.** Nothing in the backend writes the `outcomes` table yet, so every
  payment reads "No outcome recorded yet". The view renders the array whenever
  it fills up; it does not invent a status in the meantime.
- **Authentication.** The API it reads is unauthenticated, so this dashboard is
  local-development only. See the backend's `docs/README.md`.
- **Tests.** The backend is well covered; this app is not. What is worth testing
  here is small and specific — the null-confidence rendering, the override
  guard, the two distinct empty states.
