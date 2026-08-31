import { useEffect, useState, type ReactNode } from 'react'
import { ApiError, getEscalations, type Escalation } from '../api'
import { confidence, rupees, shortId, timestamp } from '../format'
import ActionBadge from './ActionBadge'

/**
 * The escalation queue: every payment the agent stopped working on and left for
 * a person.
 *
 * The reasoning is on the card, not behind a click. The queue's whole job is to
 * answer "why does a human need to look at this", and a reviewer scanning fifty
 * cases will not open fifty pages to find out.
 */
type State =
  | { kind: 'loading' }
  | { kind: 'error'; message: string }
  | { kind: 'loaded'; cases: Escalation[] }

/**
 * The three reasons a payment ends up here, and what each one means for the
 * person reviewing it. They are visually distinct because they call for
 * different work: an uncertain model answer wants a judgement, an exhausted
 * budget wants a decision about the customer, and a failed decision layer wants
 * someone to look at the system itself.
 */
interface ReasonStyle {
  key: string
  label: string
  tag: string
  card: string
  blurb: string
}

const REASONS: Record<string, ReasonStyle> = {
  low_confidence: {
    key: 'low_confidence',
    label: 'model uncertainty',
    tag: 'bg-violet-100 text-violet-900 ring-violet-600/25',
    card: 'border-violet-200',
    blurb: 'The model answered, but below the confidence threshold, so the gate overrode it.',
  },
  retry_budget_exhausted: {
    key: 'retry_budget_exhausted',
    label: 'budget exhausted',
    tag: 'bg-sky-100 text-sky-900 ring-sky-600/25',
    card: 'border-sky-200',
    blurb: 'The retry budget for this category ran out, so the stopping rule ended it.',
  },
}

// The third case: escalation_reason is NULL. Not a missing value — the fallback
// fires when the system could not reason at all, so there is no policy reason to
// name. It gets its own identity rather than being rendered as a blank.
const FALLBACK: ReasonStyle = {
  key: 'fallback',
  label: 'LLM failure',
  tag: 'bg-amber-100 text-amber-900 ring-amber-600/25',
  card: 'border-amber-300',
  blurb: 'Both the model call and its retry failed, so a conservative static decision stood in.',
}

const UNKNOWN: ReasonStyle = {
  key: 'unknown',
  label: 'unrecognised reason',
  tag: 'bg-slate-100 text-slate-700 ring-slate-500/25',
  card: 'border-slate-200',
  blurb: 'This reason has no styling yet — it was added to the backend after this view.',
}

function styleFor(e: Escalation): ReasonStyle {
  if (e.escalation_reason === null) return FALLBACK
  return REASONS[e.escalation_reason] ?? UNKNOWN
}

export default function EscalationQueue() {
  const [state, setState] = useState<State>({ kind: 'loading' })
  const [filter, setFilter] = useState<string | null>(null)

  useEffect(() => {
    let current = true
    getEscalations()
      .then((cases) => {
        if (current) setState({ kind: 'loaded', cases })
      })
      .catch((err: unknown) => {
        if (current) setState({ kind: 'error', message: describe(err) })
      })
    return () => {
      current = false
    }
  }, [])

  return (
    <section className="rounded-lg border border-slate-200 bg-white p-4 sm:p-6">
      <h2 className="text-lg font-semibold text-slate-900">Escalation queue</h2>
      <p className="mt-1 text-sm text-slate-500">
        Payments the agent stopped working on, and why each one needs a person.
      </p>

      {state.kind === 'loading' && (
        <p className="mt-5 text-sm text-slate-500">Loading escalations…</p>
      )}
      {state.kind === 'error' && <p className="mt-5 text-sm text-red-700">{state.message}</p>}
      {state.kind === 'loaded' && (
        <Queue cases={state.cases} filter={filter} onFilter={setFilter} />
      )}
    </section>
  )
}

function Queue({
  cases,
  filter,
  onFilter,
}: {
  cases: Escalation[]
  filter: string | null
  onFilter: (key: string | null) => void
}) {
  if (cases.length === 0) {
    // An empty queue is a good state, and reads as one.
    return (
      <p className="mt-5 text-sm text-slate-500">
        Nothing is waiting on a person. Every payment either resolved or is still being retried.
      </p>
    )
  }

  // Counts drive the summary chips, which double as filters. Grouping is done
  // here rather than server-side because the queue is already fully loaded —
  // this narrows what is on screen, it does not change what was fetched.
  const counts = new Map<string, number>()
  for (const c of cases) {
    const k = styleFor(c).key
    counts.set(k, (counts.get(k) ?? 0) + 1)
  }

  const shown = filter === null ? cases : cases.filter((c) => styleFor(c).key === filter)
  const order = [REASONS.low_confidence, REASONS.retry_budget_exhausted, FALLBACK, UNKNOWN]

  return (
    <div className="mt-5 flex flex-col gap-4">
      <div className="flex flex-wrap items-center gap-2">
        <Chip active={filter === null} onClick={() => onFilter(null)} className="bg-slate-100 text-slate-800 ring-slate-500/25">
          all {cases.length}
        </Chip>
        {order.map((r) =>
          counts.has(r.key) ? (
            <Chip
              key={r.key}
              active={filter === r.key}
              onClick={() => onFilter(filter === r.key ? null : r.key)}
              className={r.tag}
            >
              {r.label} {counts.get(r.key)}
            </Chip>
          ) : null
        )}
      </div>

      <ul className="flex flex-col gap-3">
        {shown.map((c) => (
          <li key={c.decision_id}>
            <Case escalation={c} />
          </li>
        ))}
      </ul>

      <p className="text-xs text-slate-500">
        Showing {shown.length} of {cases.length}, most recently decided first.
      </p>
    </div>
  )
}

function Case({ escalation: e }: { escalation: Escalation }) {
  const style = styleFor(e)
  // An override is only an override if the recorded original differs from what
  // was finally done — the same guard the detail view uses.
  const overridden = e.original_action !== null && e.original_action !== e.action

  return (
    <article className={`rounded-lg border bg-white p-4 ${style.card}`}>
      <header className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <span
          className={`inline-flex items-center rounded-md px-2 py-0.5 text-xs font-medium ring-1 ring-inset ${style.tag}`}
        >
          {style.label}
        </span>
        <ActionBadge action={e.action} />
        {overridden && (
          <span className="text-xs text-slate-600">
            overrode <span className="line-through">{e.original_action}</span>
          </span>
        )}
        <span className="font-mono text-xs text-slate-700" title={e.payment_id}>
          {shortId(e.payment_id)}
        </span>
        <span className="text-xs text-slate-600">{e.category}</span>
        <span className="text-xs font-medium tabular-nums text-slate-900">
          {rupees(e.amount_paise)}
        </span>
        <span className="ml-auto text-xs text-slate-500">{timestamp(e.decided_at)}</span>
      </header>

      <p className="mt-2 text-xs text-slate-600">
        {style.blurb}{' '}
        <span className="text-slate-500">
          {'· '}
          {e.escalation_reason === null ? (
            <span className="font-mono">escalation_reason: null</span>
          ) : (
            <span className="font-mono">{e.escalation_reason}</span>
          )}
          {' · attempt '}
          {e.attempt_count}
          {' · confidence '}
          {/* N/A, never 0% — a rule-made decision had no model behind it. */}
          {e.confidence === null ? 'N/A' : confidence(e.confidence)}
        </span>
      </p>

      {e.reasoning !== null ? (
        <p className="mt-3 rounded-md bg-slate-50 p-3 text-sm text-slate-800">
          <Label>Why</Label> {e.reasoning}
        </p>
      ) : (
        // Stopping-rule decisions store no reasoning: the rule fired before the
        // model was ever asked, so there is no model reasoning to show. Saying
        // that is more useful than an empty box.
        <p className="mt-3 rounded-md bg-slate-50 p-3 text-sm text-slate-500 italic">
          No model reasoning — the stopping rule fired before the model was asked.
        </p>
      )}

      {e.customer_message !== null && (
        <p className="mt-2 text-sm text-slate-700">
          <Label>Told the customer</Label> {e.customer_message}
        </p>
      )}
    </article>
  )
}

function Chip({
  children,
  active,
  onClick,
  className,
}: {
  children: ReactNode
  active: boolean
  onClick: () => void
  className: string
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`rounded-md px-2.5 py-1 text-xs font-medium ring-1 ring-inset ${className} ${
        active ? 'outline-2 outline-offset-1 outline-slate-700' : 'opacity-80 hover:opacity-100'
      }`}
    >
      {children}
    </button>
  )
}

function Label({ children }: { children: ReactNode }) {
  return <span className="text-xs uppercase tracking-wide text-slate-500">{children}</span>
}

function describe(err: unknown): string {
  if (err instanceof ApiError) {
    return err.status === 0
      ? 'Could not reach the backend. Is the server running on port 8080?'
      : `The backend returned ${err.status}: ${err.message}`
  }
  return 'Something went wrong loading the escalation queue.'
}
