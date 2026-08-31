import { useState, type ReactNode } from 'react'
import {
  ApiError,
  simulateDuplicate,
  simulateFailure,
  simulateLLMFailure,
  type SimulateDuplicateResult,
  type SimulateFailureResult,
  type SimulateLLMFailureResult,
  type SimulatedDecision,
} from '../api'
import { confidence, rupees, shortId } from '../format'
import ActionBadge from './ActionBadge'
import SourceBadge from './SourceBadge'

/**
 * The demo control panel: fire a real failure, a real redelivery, or a forced
 * decision-layer failure, and see what the pipeline actually did.
 *
 * Every button here produces a visible result rather than firing and forgetting.
 * That is the whole point — this is what gets clicked live, and a button that
 * silently succeeds demonstrates nothing to somebody watching.
 *
 * Nothing on this panel is mocked. Each click sends a real webhook through the
 * real classifier, stopping rule, decision layer and confidence gate, and the
 * decision shown is read back from the database afterwards.
 */

const CATEGORIES = [
  'insufficient_funds',
  'bank_downtime',
  'hard_decline',
  'soft_decline',
  'network_error',
]

type Result =
  | { kind: 'failure'; data: SimulateFailureResult }
  | { kind: 'duplicate'; data: SimulateDuplicateResult }
  | { kind: 'llm'; data: SimulateLLMFailureResult }

export default function ControlPanel() {
  // One "busy" key rather than a boolean, so the button actually clicked shows
  // the spinner and the others simply disable. A single flag would spin all of
  // them and hide which action is running.
  const [busy, setBusy] = useState<string | null>(null)
  const [result, setResult] = useState<Result | null>(null)
  const [error, setError] = useState<string | null>(null)

  async function fire<T>(key: string, call: () => Promise<T>, wrap: (data: T) => Result) {
    setBusy(key)
    setError(null)
    // The previous result is cleared before the new call, so what is on screen
    // is never a stale answer sitting under a spinner while a new one runs.
    setResult(null)
    try {
      setResult(wrap(await call()))
    } catch (err) {
      setError(describe(err))
    } finally {
      setBusy(null)
    }
  }

  const running = busy !== null

  return (
    <section className="rounded-lg border border-slate-200 bg-white p-4 sm:p-6">
      <h2 className="text-lg font-semibold text-slate-900">Control panel</h2>
      <p className="mt-1 text-sm text-slate-500">
        Each button fires a real webhook through the real pipeline and shows what it decided.
      </p>

      <div className="mt-5 flex flex-col gap-5">
        <div>
          <Label>Simulate a failure</Label>
          <div className="mt-2 flex flex-wrap gap-2">
            {CATEGORIES.map((c) => (
              <button
                key={c}
                type="button"
                disabled={running}
                onClick={() =>
                  fire(`failure:${c}`, () => simulateFailure(c), (data) => ({
                    kind: 'failure',
                    data,
                  }))
                }
                className="inline-flex items-center gap-2 rounded-md border border-slate-300 bg-white px-3 py-2 text-sm font-medium text-slate-800 hover:bg-slate-50 disabled:cursor-not-allowed disabled:opacity-50"
              >
                {busy === `failure:${c}` && <Spinner />}
                {c}
              </button>
            ))}
          </div>
        </div>

        <div className="flex flex-wrap gap-2">
          <button
            type="button"
            disabled={running}
            onClick={() =>
              fire('duplicate', simulateDuplicate, (data) => ({ kind: 'duplicate', data }))
            }
            className="inline-flex items-center gap-2 rounded-md border border-sky-300 bg-sky-50 px-3 py-2 text-sm font-medium text-sky-900 hover:bg-sky-100 disabled:cursor-not-allowed disabled:opacity-50"
          >
            {busy === 'duplicate' && <Spinner />}
            Simulate duplicate webhook
          </button>

          <button
            type="button"
            disabled={running}
            onClick={() =>
              fire('llm', simulateLLMFailure, (data) => ({ kind: 'llm', data }))
            }
            className="inline-flex items-center gap-2 rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-sm font-medium text-amber-900 hover:bg-amber-100 disabled:cursor-not-allowed disabled:opacity-50"
          >
            {busy === 'llm' && <Spinner />}
            Simulate LLM timeout
          </button>
        </div>
      </div>

      {running && (
        <p className="mt-5 text-sm text-slate-600">
          Firing a real webhook — the model is being asked, so this takes a second.
        </p>
      )}
      {error !== null && (
        <p className="mt-5 rounded-md bg-red-50 px-3 py-2 text-sm text-red-800 ring-1 ring-inset ring-red-600/20">
          {error}
        </p>
      )}

      {result !== null && (
        <div className="mt-5">
          {result.kind === 'failure' && <DecisionCard decision={result.data.decision} />}
          {result.kind === 'llm' && <LLMFailureResult data={result.data} />}
          {result.kind === 'duplicate' && <DuplicateResult data={result.data} />}
        </div>
      )}
    </section>
  )
}

/**
 * DecisionCard shows what the pipeline decided, in the same vocabulary the
 * detail view uses — same badges, same null-confidence handling — so the panel
 * and the payment page never describe one decision two different ways.
 */
function DecisionCard({ decision: d, tone = 'neutral' }: { decision: SimulatedDecision; tone?: 'neutral' | 'warn' }) {
  const overridden = d.original_action !== null && d.original_action !== d.action

  return (
    <article
      className={`rounded-lg border p-4 ${
        tone === 'warn' ? 'border-amber-300 bg-amber-50' : 'border-emerald-300 bg-emerald-50'
      }`}
    >
      <header className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <span className="font-mono text-xs text-slate-900" title={d.payment_id}>
          {shortId(d.payment_id)}
        </span>
        <span className="text-xs text-slate-600">{d.category}</span>
        <span className="text-xs tabular-nums text-slate-600">{rupees(d.amount_paise)}</span>
        <span className="text-xs text-slate-600">attempt {d.attempt_count}</span>
      </header>

      {d.action === '' ? (
        // No decision at all is a real state and is shown as one rather than
        // rendered as a blank card.
        <p className="mt-3 text-sm text-slate-600 italic">
          No decision was recorded for this payment.
        </p>
      ) : (
        <>
          <div className="mt-3 flex flex-wrap items-center gap-2">
            {overridden && (
              <>
                <span className="text-sm text-slate-500 line-through">{d.original_action}</span>
                <span className="text-slate-400" aria-label="overridden to">
                  →
                </span>
              </>
            )}
            <ActionBadge action={d.action} />
            <SourceBadge source={d.source} />
            {d.escalation_reason !== null && (
              <span className="text-xs text-slate-600">
                ({overridden ? 'overridden: ' : ''}
                {d.escalation_reason})
              </span>
            )}
            <span className="ml-auto text-sm tabular-nums">
              <span className="text-xs uppercase tracking-wide text-slate-500">Confidence </span>
              {/* N/A, never 0% — a rule-made decision had no model behind it. */}
              <span
                className={d.confidence === null ? 'text-slate-500' : 'font-medium text-slate-900'}
                title={
                  d.confidence === null
                    ? 'no model confidence — this decision came from a rule, not the model'
                    : undefined
                }
              >
                {d.confidence === null ? 'N/A' : confidence(d.confidence)}
              </span>
            </span>
          </div>

          {d.reasoning !== null && (
            <p className="mt-3 text-sm text-slate-700">
              <Label>Reasoning</Label> {d.reasoning}
            </p>
          )}
          {d.customer_message !== null && (
            <p className="mt-3 rounded-md bg-white/70 p-3 text-sm text-slate-800">
              <Label>Told the customer</Label> {d.customer_message}
            </p>
          )}
        </>
      )}
    </article>
  )
}

/**
 * DuplicateResult shows both deliveries.
 *
 * The two rows carry the same event id on purpose — that is the thing being
 * demonstrated — and both show HTTP 200, because answering anything else would
 * make Razorpay redeliver something already handled. What separates them is the
 * attempt count, which does not move on the second delivery.
 */
function DuplicateResult({ data }: { data: SimulateDuplicateResult }) {
  return (
    <div className="flex flex-col gap-3">
      <p className="text-sm text-slate-700">
        <Label>Event id delivered twice</Label>{' '}
        <span className="font-mono text-xs">{data.event_id}</span>
      </p>

      <ol className="flex flex-col gap-2">
        {data.deliveries.map((d, i) => {
          const dup = d.status === 'duplicate'
          return (
            <li
              key={i}
              className={`flex flex-wrap items-center gap-x-3 gap-y-1 rounded-lg border p-3 text-sm ${
                dup
                  ? 'border-amber-300 bg-amber-50 text-amber-900'
                  : 'border-emerald-300 bg-emerald-50 text-emerald-900'
              }`}
            >
              <span className="font-semibold">Delivery {i + 1}</span>
              <span
                className={`rounded-md px-2 py-0.5 text-xs font-medium ring-1 ring-inset ${
                  dup
                    ? 'bg-amber-100 text-amber-900 ring-amber-600/25'
                    : 'bg-emerald-100 text-emerald-800 ring-emerald-600/25'
                }`}
              >
                {d.status}
              </span>
              <span className="text-xs">HTTP {d.http_status}</span>
              <span className="text-xs tabular-nums">attempt_count {d.attempt_count}</span>
              <span className="text-xs tabular-nums">
                decisions {d.decisions_for_payment}
              </span>
            </li>
          )
        })}
      </ol>

      <p className="text-xs text-slate-600">
        Both deliveries answer HTTP 200 by design — telling Razorpay to retry something already
        handled is worse than acknowledging it. The proof is that the second one left{' '}
        <span className="font-medium">attempt_count</span> and the decision count exactly where
        the first put them.
      </p>

      <DecisionCard decision={data.decision} />
    </div>
  )
}

/** LLMFailureResult shows the fallback and says explicitly how it was caused. */
function LLMFailureResult({ data }: { data: SimulateLLMFailureResult }) {
  return (
    <div className="flex flex-col gap-3">
      <p className="rounded-md bg-amber-50 px-3 py-2 text-xs text-amber-900 ring-1 ring-inset ring-amber-600/20">
        <strong className="font-semibold">Decision layer forced to fail</strong> for this one
        request — both the call and its retry. No environment variable and no restart: the
        instruction rode on this request only, and the real webhook endpoint cannot be made to do
        it. The webhook still answered HTTP {data.webhook_http_status}, so a model outage never
        becomes Razorpay{"'"}s problem, and the payment still got a decision and a customer
        message.
      </p>
      <DecisionCard decision={data.decision} tone="warn" />
    </div>
  )
}

function Label({ children }: { children: ReactNode }) {
  return <span className="text-xs uppercase tracking-wide text-slate-500">{children}</span>
}

function Spinner() {
  return (
    <span
      aria-hidden
      className="size-3.5 animate-spin rounded-full border-2 border-slate-300 border-t-slate-600"
    />
  )
}

function describe(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 0) {
      return 'Could not reach the backend. Is the server running on port 8080?'
    }
    return `The backend returned ${err.status}: ${err.message}`
  }
  return 'Something went wrong.'
}
