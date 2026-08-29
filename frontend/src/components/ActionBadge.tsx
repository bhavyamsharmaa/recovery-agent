/**
 * ActionBadge colours a decision's action by what it means for the payment.
 *
 * The split is between actions that leave the payment in play and actions that
 * end automated handling of it. Someone scanning the feed is looking for the
 * second kind — those are the payments that need a person — so escalate and
 * no_retry are the ones that carry warm colour, and the retry actions stay
 * quiet. Colour is not the only signal: the action's own name is in the badge,
 * so the table is still readable to someone who cannot distinguish the hues.
 */

const STYLES: Record<string, string> = {
  // Terminal for the automation: a human, or the customer, has to act.
  escalate: 'bg-red-100 text-red-800 ring-red-600/20',
  no_retry: 'bg-amber-100 text-amber-900 ring-amber-600/20',
  // Still in play — the agent intends to try again.
  retry_now: 'bg-emerald-100 text-emerald-800 ring-emerald-600/20',
  retry_delayed: 'bg-sky-100 text-sky-800 ring-sky-600/20',
  // Neither: the payment continues, but on a different instrument.
  suggest_alternate_method: 'bg-violet-100 text-violet-800 ring-violet-600/20',
}

const UNKNOWN = 'bg-slate-100 text-slate-700 ring-slate-500/20'

export default function ActionBadge({ action }: { action: string | null }) {
  // Null is not an action. It means no decision has been recorded against this
  // payment yet, which is a real state — the row is counted before any decision
  // layer runs — and saying so is more honest than an empty cell.
  if (action === null) {
    return <span className="text-slate-400 italic">no decision yet</span>
  }

  // An unrecognised action still renders. A new action added to the backend
  // should show up here looking plain, not vanish behind a lookup miss.
  const style = STYLES[action] ?? UNKNOWN

  return (
    <span
      className={`inline-flex items-center rounded-md px-2 py-1 text-xs font-medium ring-1 ring-inset ${style}`}
    >
      {action}
    </span>
  )
}
