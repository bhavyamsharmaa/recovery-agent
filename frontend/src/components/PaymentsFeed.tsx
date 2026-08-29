import { useEffect, useState, type KeyboardEvent, type ReactNode } from 'react'
import { ApiError, getPayments, type PaymentSummary } from '../api'
import { confidence, rupees, shortId } from '../format'
import ActionBadge from './ActionBadge'
import Filters, { type FilterState } from './Filters'

/**
 * One state, four shapes, rather than separate loading/error/rows fields.
 *
 * Three independent pieces of state can express things that are not true —
 * loading with an error, rows alongside a failure — and every render then has
 * to decide which of them wins. A union cannot be in two of these at once, so
 * the results below read exactly one answer.
 */
type FeedState =
  | { kind: 'loading' }
  | { kind: 'error'; message: string }
  | { kind: 'loaded'; payments: PaymentSummary[] }

interface Props {
  onSelect: (paymentId: string) => void
}

export default function PaymentsFeed({ onSelect }: Props) {
  const [filters, setFilters] = useState<FilterState>({ category: '', action: '' })
  const [state, setState] = useState<FeedState>({ kind: 'loading' })

  // Loading is entered by the event that causes it — the filter change — and on
  // first mount by the initial state above, rather than being set synchronously
  // inside the effect. The effect only ever writes the answer, which arrives
  // asynchronously.
  function changeFilters(next: FilterState) {
    setFilters(next)
    setState({ kind: 'loading' })
  }

  useEffect(() => {
    // Filtering is done by the API, not by narrowing an already-fetched array:
    // this component never holds the full table, so what is on screen is what
    // the query actually returned rather than a client-side approximation of it.
    let current = true

    getPayments({ category: filters.category, action: filters.action })
      .then((payments) => {
        // A filter changed while this request was in flight, so its answer
        // describes a query nobody is asking any more. Dropping it stops a slow
        // response from overwriting a faster later one.
        if (current) setState({ kind: 'loaded', payments })
      })
      .catch((err: unknown) => {
        // The rows are replaced rather than kept: a table still showing results
        // after the query behind it failed is claiming those rows match the
        // current filters, and they may not.
        if (current) setState({ kind: 'error', message: describe(err) })
      })

    return () => {
      current = false
    }
  }, [filters.category, filters.action])

  const filtered = Boolean(filters.category || filters.action)

  return (
    <section className="flex flex-col gap-4">
      <Filters value={filters} onChange={changeFilters} busy={state.kind === 'loading'} />

      {state.kind === 'loading' && (
        <Panel>
          <span className="inline-flex items-center gap-2 text-slate-500">
            <Spinner />
            Loading payments…
          </span>
        </Panel>
      )}

      {state.kind === 'error' && (
        <Panel>
          <span className="text-red-700">{state.message}</span>
        </Panel>
      )}

      {state.kind === 'loaded' && state.payments.length === 0 && (
        // The two empty messages are different claims. "No payments match these
        // filters" tells the reader to widen the filters; "none recorded yet"
        // tells them the system has seen nothing at all, and showing that while
        // a filter is active would be false.
        <Panel>
          <span className="text-slate-500">
            {filtered ? 'No payments match these filters.' : 'No failed payments recorded yet.'}
          </span>
        </Panel>
      )}

      {state.kind === 'loaded' && state.payments.length > 0 && (
        <>
          {/* Two layouts of the same rows, each built from cellsFor below, so a
              column added or renamed changes both at once. A table forced into
              blocks by CSS would keep one DOM tree but lose its semantics; two
              trees built from one definition keep both honest. */}
          <div className="hidden sm:block">
            <PaymentsTable payments={state.payments} onSelect={onSelect} />
          </div>
          <div className="sm:hidden">
            <PaymentCards payments={state.payments} onSelect={onSelect} />
          </div>

          <p className="text-xs text-slate-500">
            {state.payments.length} payment{state.payments.length === 1 ? '' : 's'}, most
            recently seen first
          </p>
        </>
      )}
    </section>
  )
}

/**
 * cellsFor is the single description of what a payment row shows.
 *
 * Both the table and the card layout render from this list, which is the point:
 * the mobile view is the same data in a different shape, and there is no second
 * place to forget to update.
 */
interface Cell {
  label: string
  node: ReactNode
  /** Right-aligned in the table; numbers read better against a common edge. */
  numeric?: boolean
}

function cellsFor(p: PaymentSummary): Cell[] {
  return [
    {
      label: 'Payment',
      node: (
        <span className="font-mono text-xs text-slate-900" title={p.payment_id}>
          {shortId(p.payment_id)}
        </span>
      ),
    },
    { label: 'Category', node: <span className="text-slate-700">{p.category}</span> },
    { label: 'Latest action', node: <ActionBadge action={p.latest_action} /> },
    {
      label: 'Confidence',
      numeric: true,
      node: (
        // An em dash where a score would be: this decision had no model behind
        // it. The title says so for anyone unsure.
        <span
          className={p.latest_confidence === null ? 'text-slate-400' : 'text-slate-700'}
          title={
            p.latest_confidence === null
              ? 'no model confidence — decided by a rule, not the model'
              : undefined
          }
        >
          {confidence(p.latest_confidence)}
        </span>
      ),
    },
    {
      label: 'Amount',
      numeric: true,
      node: <span className="text-slate-900">{rupees(p.amount_paise)}</span>,
    },
    {
      label: 'Attempts',
      numeric: true,
      node: <span className="text-slate-700">{p.attempt_count}</span>,
    },
  ]
}

interface LayoutProps {
  payments: PaymentSummary[]
  onSelect: (paymentId: string) => void
}

function PaymentsTable({ payments, onSelect }: LayoutProps) {
  const headers = cellsFor(payments[0])

  return (
    <div className="overflow-hidden rounded-lg border border-slate-200 bg-white">
      <table className="w-full text-left text-sm">
        <thead className="border-b border-slate-200 bg-slate-50 text-xs uppercase tracking-wide text-slate-600">
          <tr>
            {headers.map((c) => (
              <th
                key={c.label}
                className={`px-4 py-3 font-medium ${c.numeric ? 'text-right' : ''}`}
              >
                {c.label}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="divide-y divide-slate-100">
          {payments.map((p) => (
            <tr key={p.payment_id} {...rowHandlers(p.payment_id, onSelect)} className={ROW_STYLE}>
              {cellsFor(p).map((c) => (
                <td
                  key={c.label}
                  className={`px-4 py-3 ${c.numeric ? 'text-right tabular-nums' : ''}`}
                >
                  {c.node}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

/**
 * The under-640px layout: one card per payment, each field on its own line
 * with its column header as a label.
 *
 * The header text has to move into the rows because there is no header row to
 * carry it — a bare "68%" or "2" in a stack means nothing without its name.
 */
function PaymentCards({ payments, onSelect }: LayoutProps) {
  return (
    <ul className="flex flex-col gap-3">
      {payments.map((p) => (
        <li key={p.payment_id}>
          <article
            {...rowHandlers(p.payment_id, onSelect)}
            className={`rounded-lg border border-slate-200 bg-white p-4 ${ROW_STYLE}`}
          >
            <dl className="flex flex-col gap-2 text-sm">
              {cellsFor(p).map((c) => (
                <div key={c.label} className="flex items-center justify-between gap-3">
                  <dt className="text-xs uppercase tracking-wide text-slate-500">{c.label}</dt>
                  <dd className={c.numeric ? 'tabular-nums' : 'text-right'}>{c.node}</dd>
                </div>
              ))}
            </dl>
          </article>
        </li>
      ))}
    </ul>
  )
}

const ROW_STYLE =
  'cursor-pointer hover:bg-slate-50 focus:bg-slate-50 focus:outline-2 focus:-outline-offset-2 focus:outline-sky-600'

/**
 * rowHandlers makes a whole row or card activate the detail view.
 *
 * Keyboard-reachable as well as clickable: a click target this large is
 * invisible to anyone not using a mouse unless it is focusable and responds to
 * Enter and Space.
 */
function rowHandlers(paymentId: string, onSelect: (paymentId: string) => void) {
  return {
    onClick: () => onSelect(paymentId),
    tabIndex: 0,
    onKeyDown: (e: KeyboardEvent) => {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault()
        onSelect(paymentId)
      }
    },
  }
}

/** Panel carries the loading, error, and empty messages at every width. */
function Panel({ children }: { children: ReactNode }) {
  return (
    <div className="rounded-lg border border-slate-200 bg-white px-4 py-10 text-center text-sm">
      {children}
    </div>
  )
}

function Spinner() {
  return (
    <span
      aria-hidden
      className="size-4 animate-spin rounded-full border-2 border-slate-300 border-t-slate-600"
    />
  )
}

/**
 * describe turns a thrown value into something worth putting on screen.
 *
 * Status 0 is the client's marker for a request that never got an answer, and
 * it is worth calling out separately: "the backend is not running" is a
 * different problem from "the backend answered with an error", and the fix for
 * each is different.
 */
function describe(err: unknown): string {
  if (err instanceof ApiError) {
    return err.status === 0
      ? 'Could not reach the backend. Is the server running on port 8080?'
      : `The backend returned ${err.status}: ${err.message}`
  }
  return 'Something went wrong loading payments.'
}
