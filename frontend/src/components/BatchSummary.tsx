import { useEffect, useState, type ReactNode } from 'react'
import { ApiError, getLatestBatchRun, runBatch, type BatchRun } from '../api'
import { formatPoints, formatRate, isImprovement, recoveredDelta } from '../batchMath'
import { rupees, timestamp } from '../format'

/**
 * The batch recovery summary: what this agent's routing recovered, against what
 * a blind retry-everything strategy would have.
 *
 * The comparison is the point of the screen, so the two sides are laid out
 * against each other rather than stacked, and the improvement is stated as its
 * own figure instead of being left as mental arithmetic between two percentages.
 *
 * Every number here is simulated. That is said on the screen, not only in the
 * README, because a rupee figure with no provenance is the easiest thing on a
 * dashboard to mistake for a measurement.
 */
type State =
  | { kind: 'loading' }
  | { kind: 'empty' } // no batch has ever been run
  | { kind: 'error'; message: string }
  | { kind: 'loaded'; run: BatchRun }

// Batch size fixed at 100 for the dashboard UI — smaller batches produce
// statistically noisy results (an 8-payment run scored -6.4pp vs baseline in
// testing) that would misrepresent the system during a live demo. The API and
// CLI still accept any size 1-200; this is a UI-only guardrail.
//
// Sent explicitly rather than relying on the endpoint's own default, which is 20
// and deliberately small because it holds a browser request open. Leaving it
// implicit is what made the button run 20 payments while the CLI ran 100.
const DASHBOARD_BATCH_SIZE = 100

export default function BatchSummary() {
  const [state, setState] = useState<State>({ kind: 'loading' })
  const [running, setRunning] = useState(false)
  const [runError, setRunError] = useState<string | null>(null)

  useEffect(() => {
    let current = true
    getLatestBatchRun()
      .then((run) => {
        if (current) setState({ kind: 'loaded', run })
      })
      .catch((err: unknown) => {
        if (!current) return
        // 404 is "nobody has run a batch yet", which is an ordinary state on a
        // fresh database and not an error to report as one.
        if (err instanceof ApiError && err.status === 404) {
          setState({ kind: 'empty' })
          return
        }
        setState({ kind: 'error', message: describe(err) })
      })
    return () => {
      current = false
    }
  }, [])

  async function onRun() {
    setRunning(true)
    setRunError(null)
    try {
      const run = await runBatch({ size: DASHBOARD_BATCH_SIZE })
      setState({ kind: 'loaded', run })
    } catch (err) {
      setRunError(describe(err))
    } finally {
      setRunning(false)
    }
  }

  return (
    <section className="rounded-lg border border-slate-200 bg-white p-4 sm:p-6">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h2 className="text-lg font-semibold text-slate-900">Batch recovery summary</h2>
          <p className="mt-1 text-sm text-slate-500">
            What category-aware routing recovered, against a blind retry of every failure.
          </p>
        </div>

        <button
          type="button"
          onClick={onRun}
          disabled={running}
          className="inline-flex items-center gap-2 rounded-md bg-sky-700 px-4 py-2 text-sm font-medium text-white hover:bg-sky-800 disabled:cursor-not-allowed disabled:bg-slate-400"
        >
          {running && <Spinner />}
          {running
            ? `Running batch… (${DASHBOARD_BATCH_SIZE} payments)`
            : `Run new batch (${DASHBOARD_BATCH_SIZE} payments)`}
        </button>
      </header>

      {/* Stated on screen, not only in the README. Every figure below is a
          seeded draw against declared probabilities — no gateway is called. */}
      <p className="mt-3 rounded-md bg-amber-50 px-3 py-2 text-xs text-amber-900 ring-1 ring-inset ring-amber-600/20">
        <strong className="font-semibold">Simulated outcomes.</strong> Decisions are made by the
        real pipeline, but whether a payment recovered is a seeded draw against declared
        probabilities — no payment gateway is called and no money moves.
      </p>

      {running && (
        <p className="mt-4 text-sm text-slate-600">
          Running {DASHBOARD_BATCH_SIZE} payments {'—'} each one goes through the real
          classifier, stopping rule and decision layer, so this takes a few minutes.
        </p>
      )}
      {runError !== null && <p className="mt-4 text-sm text-red-700">{runError}</p>}

      <div className="mt-6">
        {state.kind === 'loading' && <p className="text-sm text-slate-500">Loading latest run…</p>}
        {state.kind === 'error' && <p className="text-sm text-red-700">{state.message}</p>}
        {state.kind === 'empty' && (
          <p className="text-sm text-slate-500">
            No batch has been run yet. Press <strong>Run new batch</strong> to score{' '}
            {DASHBOARD_BATCH_SIZE} simulated failures.
          </p>
        )}
        {state.kind === 'loaded' && <Figures run={state.run} />}
      </div>
    </section>
  )
}

/**
 * Figures renders a completed run. Exported for tests: it is a pure function of
 * a BatchRun, so the whole comparison can be asserted without mocking fetch or
 * standing up a backend.
 */
export function Figures({ run }: { run: BatchRun }) {
  // A run that never completed has no figures. Showing zeros would report it as
  // a batch that recovered nothing, which is a different and false claim.
  if (
    run.total_at_risk_paise === null ||
    run.total_recovered_paise === null ||
    run.recovery_rate === null ||
    run.baseline_recovered_paise === null ||
    run.baseline_recovery_rate === null
  ) {
    return (
      <p className="text-sm text-slate-500">
        Run #{run.id} started but never completed, so it has no figures to show.
      </p>
    )
  }

  return (
    <div className="flex flex-col gap-6">
      <div>
        <Label>Total at risk</Label>
        <p className="mt-1 text-2xl font-semibold tabular-nums text-slate-900">
          {rupees(run.total_at_risk_paise)}
        </p>
        <p className="mt-1 text-xs text-slate-500">
          across {run.batch_size} simulated failures
        </p>
      </div>

      {/* The two strategies side by side. Same money at risk, same payments,
          same random stream — only the routing differs, which is what makes the
          comparison mean anything. */}
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <StatCard
          title="This agent"
          subtitle="Category-aware routing"
          amount={run.total_recovered_paise}
          rate={run.recovery_rate}
          emphasis
        />
        <StatCard
          title="Naive baseline"
          subtitle="Blind retry of every failure"
          amount={run.baseline_recovered_paise}
          rate={run.baseline_recovery_rate}
        />
      </div>

      <Improvement
        points={run.improvement_points}
        recovered={run.total_recovered_paise}
        baseline={run.baseline_recovered_paise}
      />

      <p className="text-xs text-slate-500">
        Run #{run.id} {'·'} seed <span className="font-mono">{run.rng_seed}</span> {'·'}{' '}
        {run.completed_at === null ? 'not completed' : timestamp(run.completed_at)}
        <br />
        The seed reproduces this run{"'"}s scenario mix, amounts and outcome draws:{' '}
        <span className="font-mono">go run ./cmd/run-batch --size {run.batch_size} --seed{' '}
        {run.rng_seed}</span>
      </p>
    </div>
  )
}

interface StatCardProps {
  title: string
  subtitle: string
  amount: number
  rate: number
  /** The agent's own card carries the emphasis; the baseline is the reference. */
  emphasis?: boolean
}

function StatCard({ title, subtitle, amount, rate, emphasis = false }: StatCardProps) {
  return (
    <div
      className={`rounded-lg border p-5 ${
        emphasis ? 'border-emerald-300 bg-emerald-50' : 'border-slate-200 bg-slate-50'
      }`}
    >
      <p className={`text-sm font-semibold ${emphasis ? 'text-emerald-900' : 'text-slate-700'}`}>
        {title}
      </p>
      <p className={`text-xs ${emphasis ? 'text-emerald-700' : 'text-slate-500'}`}>{subtitle}</p>

      <p
        className={`mt-4 text-4xl font-bold tabular-nums ${
          emphasis ? 'text-emerald-800' : 'text-slate-700'
        }`}
      >
        {formatRate(rate)}
      </p>
      <p className={`text-xs ${emphasis ? 'text-emerald-700' : 'text-slate-500'}`}>recovery rate</p>

      <p
        className={`mt-3 text-lg font-semibold tabular-nums ${
          emphasis ? 'text-emerald-900' : 'text-slate-800'
        }`}
      >
        {rupees(amount)}
      </p>
      <p className={`text-xs ${emphasis ? 'text-emerald-700' : 'text-slate-500'}`}>recovered</p>
    </div>
  )
}

/**
 * The improvement, stated rather than left to be worked out.
 *
 * It is signed and coloured by direction: a batch where the baseline happened to
 * win should look like it lost, not be quietly presented as a gain. Small
 * batches do produce that, and hiding it would make the screen a advertisement
 * rather than a measurement.
 */
function Improvement({
  points,
  recovered,
  baseline,
}: {
  points: number | null
  recovered: number
  baseline: number
}) {
  if (points === null) {
    return (
      <p className="text-sm text-slate-500">
        Improvement unavailable {'—'} this run is missing one of the two rates.
      </p>
    )
  }

  const better = isImprovement(points)
  const delta = recoveredDelta(recovered, baseline)

  return (
    <div
      className={`rounded-lg border p-5 ${
        better ? 'border-emerald-200 bg-white' : 'border-amber-300 bg-amber-50'
      }`}
    >
      <Label>Improvement over naive baseline</Label>
      <p
        className={`mt-1 text-3xl font-bold tabular-nums ${
          better ? 'text-emerald-700' : 'text-amber-800'
        }`}
      >
        {formatPoints(points)} percentage points
      </p>
      <p className="mt-1 text-sm text-slate-600">
        {better ? 'Additional' : 'Less'} money recovered:{' '}
        <span className="font-medium tabular-nums">{rupees(Math.abs(delta))}</span>
      </p>
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
      className="size-4 animate-spin rounded-full border-2 border-white/40 border-t-white"
    />
  )
}

function describe(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 0) {
      return 'Could not reach the backend. Is the server running on port 8080?'
    }
    if (err.status === 409) {
      return 'A batch run is already in progress. Wait for it to finish before starting another.'
    }
    return `The backend returned ${err.status}: ${err.message}`
  }
  return 'Something went wrong.'
}
