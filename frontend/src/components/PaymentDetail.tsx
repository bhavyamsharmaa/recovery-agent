import { useEffect, useState, type ReactNode } from 'react'
import {
  ApiError,
  getPaymentDetail,
  type Decision,
  type PaymentDetail as Detail,
  type PaymentRecord,
} from '../api'
import { confidence, rupees, timestamp } from '../format'
import ActionBadge from './ActionBadge'
import SourceBadge from './SourceBadge'

/**
 * One payment's whole story: what failed, every decision in order, and any
 * outcome. The same three sections cmd/trace-payment prints, from the same
 * queries, rendered instead of formatted.
 */
type DetailState =
  | { kind: 'loading' }
  | { kind: 'error'; message: string }
  | { kind: 'loaded'; detail: Detail }

interface Props {
  paymentId: string
  onBack: () => void
}

export default function PaymentDetail({ paymentId, onBack }: Props) {
  const [state, setState] = useState<DetailState>({ kind: 'loading' })

  useEffect(() => {
    let current = true
    getPaymentDetail(paymentId)
      .then((detail) => {
        if (current) setState({ kind: 'loaded', detail })
      })
      .catch((err: unknown) => {
        if (current) setState({ kind: 'error', message: describe(err, paymentId) })
      })
    return () => {
      current = false
    }
  }, [paymentId])

  return (
    <section className="flex flex-col gap-6">
      <button
        type="button"
        onClick={onBack}
        className="self-start text-sm text-sky-700 hover:underline"
      >
        ← Back to all payments
      </button>

      {state.kind === 'loading' && (
        <p className="text-sm text-slate-500">Loading payment…</p>
      )}
      {state.kind === 'error' && <p className="text-sm text-red-700">{state.message}</p>}
      {state.kind === 'loaded' && (
        <>
          <PaymentFacts payment={state.detail.payment} />
          <DecisionTimeline decisions={state.detail.decisions} />
          <Outcomes outcomes={state.detail.outcomes} />
        </>
      )}
    </section>
  )
}

function PaymentFacts({ payment }: { payment: PaymentRecord }) {
  return (
    <div className="rounded-lg border border-slate-200 bg-white p-4 sm:p-6">
      {/* break-all rather than truncate: a payment id is the one string on this
          page a reader may need to copy in full, so it wraps instead of being
          cut off at a narrow width. */}
      <p className="font-mono text-sm break-all text-slate-900">{payment.payment_id}</p>
      <p className="mt-1 text-2xl font-semibold tabular-nums text-slate-900 sm:text-3xl">
        {rupees(payment.amount_paise)}
      </p>

      <dl className="mt-6 grid grid-cols-1 gap-x-8 gap-y-4 sm:grid-cols-2 lg:grid-cols-3">
        <Fact label="Category" value={payment.category} />
        <Fact label="Payment method" value={payment.payment_method} />
        <Fact label="Attempts" value={String(payment.attempt_count)} />
        <Fact label="Error code" value={payment.error_code} />
        <Fact label="Error reason" value={payment.error_reason} />
        <Fact label="Error source" value={payment.error_source} />
        <Fact label="First failed" value={timestamp(payment.first_failed_at)} />
        <Fact label="Last seen" value={timestamp(payment.last_seen_at)} />
      </dl>
    </div>
  )
}

/**
 * Fact renders one labelled value.
 *
 * An empty string is shown as "(unrecorded)" rather than as a blank, matching
 * cmd/trace-payment: a column that was never written and a column that holds an
 * empty value should not look the same on screen.
 */
function Fact({ label, value }: { label: string; value: string }) {
  const written = value.trim() !== ''
  return (
    <div>
      <dt className="text-xs uppercase tracking-wide text-slate-500">{label}</dt>
      <dd className={`mt-1 text-sm ${written ? 'text-slate-900' : 'text-slate-400 italic'}`}>
        {written ? value : '(unrecorded)'}
      </dd>
    </div>
  )
}

function DecisionTimeline({ decisions }: { decisions: Decision[] }) {
  return (
    <div>
      <h2 className="text-sm font-semibold uppercase tracking-wide text-slate-600">
        Decision history
      </h2>

      {decisions.length === 0 ? (
        <p className="mt-3 rounded-lg border border-slate-200 bg-white p-4 text-sm text-slate-500 sm:p-6">
          No decisions recorded for this payment.
        </p>
      ) : (
        <ol className="mt-3 flex flex-col gap-3">
          {decisions.map((d) => (
            <li key={d.id}>
              <DecisionCard decision={d} />
            </li>
          ))}
        </ol>
      )}
    </div>
  )
}

function DecisionCard({ decision: d }: { decision: Decision }) {
  // An override is only an override if the recorded original differs from what
  // was finally done. The column is sometimes set to the same value, and
  // rendering "escalate → escalate" would invent an event that never happened.
  const overridden = d.original_action !== null && d.original_action !== d.action

  return (
    <article className="rounded-lg border border-slate-200 bg-white p-4 sm:p-5">
      {/* The timestamp is pushed right on a wide card and drops to its own line
          on a narrow one, rather than squeezing the source badge — the badge is
          the thing being read here, and it must not be truncated. */}
      <header className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <span className="text-xs font-semibold text-slate-500">
          Attempt #{d.attempt_number}
        </span>
        <SourceBadge source={d.source} />
        <span className="w-full text-xs text-slate-500 sm:ml-auto sm:w-auto">
          {timestamp(d.created_at)}
        </span>
      </header>

      <div className="mt-4 flex flex-wrap items-center gap-2">
        {/* The override is a real event in the payment's story, not a detail:
            the model asked for one thing and the gate did another. Showing only
            the final action would erase the disagreement. */}
        {overridden && (
          <>
            <span className="text-sm text-slate-500 line-through">{d.original_action}</span>
            <span className="text-slate-400" aria-label="overridden to">
              →
            </span>
          </>
        )}
        <ActionBadge action={d.action} />

        {d.escalation_reason !== null && (
          <span className="text-xs text-slate-600">
            ({overridden ? 'overridden: ' : ''}
            {d.escalation_reason})
          </span>
        )}

        {/* Confidence takes its own full-width line below 640px. Wrapped into
            the action row it would sit tight against the override arrow, and
            the two are separate facts about the decision. */}
        <span className="w-full text-sm tabular-nums sm:ml-auto sm:w-auto">
          <span className="text-xs uppercase tracking-wide text-slate-500">Confidence </span>
          {/* N/A, never 0% and never blank. A null confidence says no model
              stood behind this decision; a 0 would say the model was certain it
              was wrong, which is a different and false claim. */}
          <span
            className={d.confidence === null ? 'text-slate-400' : 'font-medium text-slate-900'}
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

      {d.alternate_method !== null && d.alternate_method !== '' && (
        <p className="mt-3 text-sm text-slate-700">
          <Label>Alternate method</Label> {d.alternate_method}
        </p>
      )}

      {d.reasoning !== null && (
        <p className="mt-3 text-sm text-slate-700">
          <Label>Reasoning</Label> {d.reasoning}
        </p>
      )}

      {d.customer_message !== null && (
        <p className="mt-3 rounded-md bg-slate-50 p-3 text-sm text-slate-800">
          <Label>Told the customer</Label> {d.customer_message}
        </p>
      )}
    </article>
  )
}

function Outcomes({ outcomes }: { outcomes: Detail['outcomes'] }) {
  return (
    <div>
      <h2 className="text-sm font-semibold uppercase tracking-wide text-slate-600">Outcome</h2>

      {outcomes.length === 0 ? (
        // Nothing in the backend writes the outcomes table yet, so this is the
        // honest answer rather than the rare one. Inventing a status here —
        // "pending", "recovered" — would be the dashboard claiming to know
        // something no part of the system has recorded.
        <p className="mt-3 rounded-lg border border-slate-200 bg-white p-4 text-sm text-slate-500 sm:p-6">
          No outcome recorded yet.
        </p>
      ) : (
        <ul className="mt-3 flex flex-col gap-2">
          {outcomes.map((o, i) => (
            <li
              key={`${o.recorded_at}-${i}`}
              className="rounded-lg border border-slate-200 bg-white p-4 text-sm text-slate-800"
            >
              <span className="font-medium">{o.outcome}</span>{' '}
              <span className="text-slate-500">
                ({o.decision_id === null ? 'unlinked' : `decision id ${o.decision_id}`}) —{' '}
                {timestamp(o.recorded_at)}
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

function Label({ children }: { children: ReactNode }) {
  return <span className="text-xs uppercase tracking-wide text-slate-500">{children}</span>
}

function describe(err: unknown, paymentId: string): string {
  if (err instanceof ApiError) {
    // 404 is not a malfunction — it is the backend saying this payment was
    // never ingested, which is worth stating plainly rather than as an error.
    if (err.status === 404) return `No record of ${paymentId} — it has never been ingested.`
    if (err.status === 0) return 'Could not reach the backend. Is the server running on port 8080?'
    return `The backend returned ${err.status}: ${err.message}`
  }
  return 'Something went wrong loading this payment.'
}
