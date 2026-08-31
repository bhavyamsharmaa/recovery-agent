// Typed client for the backend's read-only JSON API.
//
// The type names below are camelCase but every field is snake_case, because
// these are the wire shapes exactly as Go serialises them. Renaming them here
// would mean a mapping layer whose only job is to disagree with the backend
// about what a field is called, and one more place to update when a column is
// added.

const BASE_URL = import.meta.env.VITE_API_BASE_URL

/**
 * The shared secret every /api/ route requires. Sent as X-API-Key on every
 * request by `request()` below, which is the only place fetch is called — so a
 * new endpoint added to this file is authenticated by construction rather than
 * by remembering to add a header.
 *
 * This is a build-time value baked into the bundle by Vite, which means anyone
 * who can load the dashboard can read it out of the JavaScript. It is not a
 * per-user credential and does not pretend to be one: it keeps the API closed
 * to anyone who has not been given the dashboard, and the moment the dashboard
 * is public the key is too. An operator session is what replaces it.
 */
const API_KEY = import.meta.env.VITE_API_ACCESS_KEY

/** One row of GET /api/payments: a failed payment plus its most recent decision. */
export interface PaymentSummary {
  payment_id: string
  category: string
  payment_method: string
  amount_paise: number
  error_reason: string
  /** RFC 3339, always UTC with a trailing Z. */
  first_failed_at: string
  last_seen_at: string
  attempt_count: number

  // Null, not absent, when no decision has been recorded against the payment
  // yet. The backend sends null rather than a zero value on purpose: a
  // confidence of 0 is a score a model can return, and "no model was involved"
  // is a different statement. Consumers must handle null rather than coercing.
  latest_action: string | null
  latest_confidence: number | null
  latest_source: string | null
}

/** The failed_payments row inside GET /api/payments/{id}. */
export interface PaymentRecord {
  payment_id: string
  category: string
  error_code: string
  error_reason: string
  error_source: string
  payment_method: string
  amount_paise: number
  attempt_count: number
  first_failed_at: string
  last_seen_at: string
}

/** One decision made about a payment. */
export interface Decision {
  id: number
  attempt_number: number
  /** Which layer decided: "llm", "stopping_rule", "confidence_gate", "fallback_rule". */
  source: string
  action: string
  /** Null for stopping-rule and fallback decisions, which had no model behind them. */
  confidence: number | null
  reasoning: string | null
  customer_message: string | null
  alternate_method: string | null
  /** Set when the decision was an escalation, naming why. */
  escalation_reason: string | null
  /** Set when this decision overrode an earlier one, naming what it replaced. */
  original_action: string | null
  created_at: string
}

/**
 * A recorded outcome.
 *
 * Written only by batch runs, never by live webhook traffic — so a payment that
 * arrived through a real webhook has an empty array here, correctly, because
 * nothing has confirmed what happened to it. Payments generated inside a batch
 * carry one, and it is a seeded simulation rather than an observation.
 */
export interface Outcome {
  outcome: string
  decision_id: number | null
  recorded_at: string
}

/** The full trace returned by GET /api/payments/{id}. */
export interface PaymentDetail {
  payment: PaymentRecord
  decisions: Decision[]
  outcomes: Outcome[]
}

/**
 * One batch run: N simulated failures put through the real pipeline, scored
 * against a blind-retry baseline.
 *
 * Every result field is nullable because the row is written when a run starts
 * and filled in when it finishes. Null here means "this run never completed",
 * which is a different statement from a run that completed having recovered
 * nothing — so these must not be coerced to 0 on the way in.
 */
export interface BatchRun {
  id: number
  /** RFC 3339, always UTC with a trailing Z. */
  started_at: string
  completed_at: string | null
  batch_size: number
  /** The seed that reproduces this run's scenario mix, amounts and outcome draws. */
  rng_seed: number

  total_at_risk_paise: number | null
  total_recovered_paise: number | null
  /** 0..1, not a percentage. Multiply for display. */
  recovery_rate: number | null
  baseline_recovered_paise: number | null
  baseline_recovery_rate: number | null

  /**
   * Payments in this run whose decision came from the fallback — the model call
   * and its retry both failed, so no decision was formed at all.
   *
   * Null means the run predates this being recorded, or never completed; 0 is a
   * real answer meaning every payment reached the model. The two must not be
   * collapsed: null cannot be reported as a clean run.
   */
  fallback_decisions: number | null

  /**
   * recovery_rate - baseline_recovery_rate, already in percentage points.
   *
   * Computed by the backend rather than here on purpose: it is the headline
   * number of the whole feature, and two clients deriving it independently is
   * two chances to derive it differently.
   */
  improvement_points: number | null
}

/**
 * A decision as the demo control panel reports it, straight after firing a
 * simulated failure through the real pipeline.
 *
 * `action` and `source` are empty strings only if the payment somehow received
 * no decision at all, which the panel renders as such rather than hiding.
 */
export interface SimulatedDecision {
  payment_id: string
  category: string
  amount_paise: number
  attempt_count: number
  action: string
  source: string
  /** Null for stopping-rule and fallback decisions — never 0. */
  confidence: number | null
  reasoning: string | null
  customer_message: string | null
  escalation_reason: string | null
  original_action: string | null
}

/** One webhook delivery in the duplicate demo. */
export interface SimulatedDelivery {
  event_id: string
  /**
   * "processed" or "duplicate", derived by the backend from what the database
   * shows before and after the delivery — not from the HTTP status, which is
   * 200 for both by design.
   */
  status: string
  attempt_count: number
  decisions_for_payment: number
  http_status: number
}

export interface SimulateFailureResult {
  event_id: string
  decision: SimulatedDecision
}

export interface SimulateDuplicateResult {
  event_id: string
  deliveries: SimulatedDelivery[]
  decision: SimulatedDecision
}

export interface SimulateLLMFailureResult {
  event_id: string
  webhook_http_status: number
  forced: boolean
  decision: SimulatedDecision
}

/** The backend's error body, sent on a 404 and on a failed query. */
interface ApiErrorBody {
  error?: string
  payment_id?: string
}

/**
 * ApiError carries the HTTP status so a caller can tell "this payment does not
 * exist" (404) apart from "the backend is broken" (500) without parsing a
 * message string.
 */
export class ApiError extends Error {
  readonly status: number

  constructor(status: number, message: string, cause?: unknown) {
    super(message, { cause })
    this.name = 'ApiError'
    this.status = status
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let response: Response
  try {
    response = await fetch(`${BASE_URL}${path}`, {
      ...init,
      // Spread the caller's headers first so the key cannot be dropped by a
      // call site that passes its own Content-Type — every route requires it,
      // so no caller gets to opt out.
      headers: { ...init?.headers, 'X-API-Key': API_KEY },
    })
  } catch (cause) {
    // fetch rejects only on a network-level failure — the server being down,
    // DNS, CORS. Status 0 marks "the request never got an answer", which is a
    // different problem from any status the server could return. The cause is
    // kept because the browser's own message is often the only thing that
    // distinguishes a CORS rejection from a dead port.
    throw new ApiError(0, `could not reach the API at ${BASE_URL}${path}`, cause)
  }

  if (!response.ok) {
    // The error body is JSON by contract, but a proxy or a crash can return
    // HTML instead, and a parse failure here would mask the real status.
    let detail = response.statusText
    try {
      const body = (await response.json()) as ApiErrorBody
      if (body.error) detail = body.error
    } catch {
      // Keep statusText.
    }

    // The backend's 401 body says only "unauthorized", deliberately — it will
    // not tell a caller whether the header was missing or merely wrong. That
    // is right for the wire and useless on screen, so the one thing the
    // operator can actually act on is said here instead.
    if (response.status === 401) {
      detail = API_KEY
        ? 'the API rejected this key: check VITE_API_ACCESS_KEY matches the backend API_ACCESS_KEY'
        : 'VITE_API_ACCESS_KEY is not set in this build, so no key was sent'
    }

    throw new ApiError(response.status, detail)
  }

  return (await response.json()) as T
}

/**
 * getPayments lists failed payments, most recently seen first.
 *
 * Both filters are optional and combinable, and match exactly. An empty or
 * undefined value is omitted from the query string rather than sent as an empty
 * parameter, so the URL says what it is actually asking for.
 */
export async function getPayments(filters?: {
  category?: string
  action?: string
}): Promise<PaymentSummary[]> {
  const params = new URLSearchParams()
  if (filters?.category) params.set('category', filters.category)
  if (filters?.action) params.set('action', filters.action)

  const query = params.toString()
  return request<PaymentSummary[]>(`/api/payments${query ? `?${query}` : ''}`)
}

/**
 * getPaymentDetail fetches one payment's full history.
 *
 * Throws an ApiError with status 404 if the payment was never ingested. The id
 * is encoded because it lands in the path, not a query parameter.
 */
export async function getPaymentDetail(paymentId: string): Promise<PaymentDetail> {
  return request<PaymentDetail>(`/api/payments/${encodeURIComponent(paymentId)}`)
}

/**
 * getLatestBatchRun fetches the most recently completed run.
 *
 * Throws an ApiError with status 404 when no batch has ever been run. That is
 * an ordinary answer on a fresh database, not a malfunction, and the caller
 * should render it as "no runs yet" rather than as an error.
 */
export async function getLatestBatchRun(): Promise<BatchRun> {
  return request<BatchRun>('/api/batch-runs/latest')
}

/** getBatchRuns fetches run history, most recent first. */
export async function getBatchRuns(): Promise<BatchRun[]> {
  return request<BatchRun[]>('/api/batch-runs')
}

/**
 * runBatch triggers a new batch and resolves with its finished summary.
 *
 * This is the one call in this client that writes. It is also slow by nature —
 * every payment in the batch makes a real model call through the real webhook
 * path — so the request stays open for as long as the run takes, and the caller
 * must show a loading state rather than assuming it will return promptly.
 *
 * A 409 means a run is already in progress; the backend refuses to start a
 * second one because two concurrent batches would interleave through one
 * attempt counter and produce figures describing nothing reproducible.
 */
export async function runBatch(options?: { size?: number; seed?: number }): Promise<BatchRun> {
  return request<BatchRun>('/api/batch-runs', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(options ?? {}),
  })
}

/**
 * simulateFailure fires one real webhook in the given category and returns the
 * decision the pipeline actually reached.
 *
 * Real, not mocked: the delivery goes through the same classifier, stopping
 * rule, decision layer and confidence gate as production traffic, so the action
 * and confidence shown are genuinely what the agent decided.
 */
export async function simulateFailure(category: string): Promise<SimulateFailureResult> {
  return request<SimulateFailureResult>(
    `/api/simulate/failure?category=${encodeURIComponent(category)}`,
    { method: 'POST' },
  )
}

/**
 * simulateDuplicate delivers one event id twice and returns both results.
 *
 * Both deliveries answer HTTP 200 — that is the contract, since telling Razorpay
 * to retry something already handled is worse than acknowledging it — so the
 * demonstration is in the returned `status` fields and the unchanged attempt
 * count, which the backend derives from observed database state.
 */
export async function simulateDuplicate(): Promise<SimulateDuplicateResult> {
  return request<SimulateDuplicateResult>('/api/simulate/duplicate', { method: 'POST' })
}

/**
 * simulateLLMFailure forces the decision layer to fail for one request and
 * returns the fallback decision that resulted.
 *
 * The forcing is request-scoped on the backend: no environment variable, no
 * restart, and no way to trigger it from the real webhook endpoint.
 */
export async function simulateLLMFailure(): Promise<SimulateLLMFailureResult> {
  return request<SimulateLLMFailureResult>('/api/simulate/llm-failure', { method: 'POST' })
}

/**
 * One case in the escalation queue: a payment whose latest decision stopped
 * automated handling and left it for a person.
 *
 * The reasoning and customer message are carried inline because the queue's job
 * is to answer "why does a human need to look at this" — an answer behind
 * another request is one a reviewer scanning a list will not read.
 */
export interface Escalation {
  payment_id: string
  category: string
  payment_method: string
  amount_paise: number
  error_reason: string
  attempt_count: number
  first_failed_at: string
  last_seen_at: string

  decision_id: number
  attempt_number: number
  /** Always "escalate" or "no_retry" — the two actions that stop the machine. */
  action: string
  source: string
  decided_at: string

  /** Null for stopping-rule and fallback decisions — never 0. */
  confidence: number | null

  /**
   * Null for a genuine fallback_rule case, and that is not a missing value: the
   * fallback fires when the system could not reason at all, so there is no
   * policy reason to name. Render it as its own category, not as a blank.
   */
  escalation_reason: string | null

  /** Set when the confidence gate overrode the model, naming what it replaced. */
  original_action: string | null

  reasoning: string | null
  customer_message: string | null
}

/** getEscalations lists cases waiting on a human, most recently decided first. */
export async function getEscalations(): Promise<Escalation[]> {
  return request<Escalation[]>('/api/escalations')
}
