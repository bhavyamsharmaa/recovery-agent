// Typed client for the backend's read-only JSON API.
//
// The type names below are camelCase but every field is snake_case, because
// these are the wire shapes exactly as Go serialises them. Renaming them here
// would mean a mapping layer whose only job is to disagree with the backend
// about what a field is called, and one more place to update when a column is
// added.

const BASE_URL = import.meta.env.VITE_API_BASE_URL

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
 * A recorded outcome. Nothing in the backend writes this table yet, so the
 * array is reliably empty today; it is typed anyway so the shape does not
 * change on the frontend the day something starts writing to it.
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

async function request<T>(path: string): Promise<T> {
  let response: Response
  try {
    response = await fetch(`${BASE_URL}${path}`)
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
