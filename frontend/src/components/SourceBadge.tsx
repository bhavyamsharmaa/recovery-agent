/**
 * SourceBadge names which layer produced a decision.
 *
 * This is the single most important thing about a decision entry, because it
 * says how much to trust the confidence beside it. A model answer and a rule
 * that fired without consulting the model are different kinds of claim, and the
 * table would be misleading if they looked alike.
 *
 * The label expands the raw column value rather than replacing it: the value is
 * what a `SELECT source FROM decisions` shows and what the logs use, so
 * hiding it would make the UI harder to reconcile against the database.
 */

interface Style {
  className: string
  label: string
}

const STYLES: Record<string, Style> = {
  // The model actually answered.
  llm: {
    className: 'bg-sky-100 text-sky-900 ring-sky-600/20',
    label: 'llm — the model decided',
  },
  // The budget was spent; the stopping rule answered before the model was asked.
  stopping_rule: {
    className: 'bg-slate-200 text-slate-800 ring-slate-500/25',
    label: 'stopping_rule — decided before the model ran',
  },
  // Both the call and its retry failed. Nobody decided; a static answer stood in.
  fallback_rule: {
    className: 'bg-amber-100 text-amber-900 ring-amber-600/25',
    label: 'fallback_rule — both model calls failed',
  },
  // The gate overrode a low-confidence model answer.
  confidence_gate: {
    className: 'bg-violet-100 text-violet-900 ring-violet-600/20',
    label: 'confidence_gate — overrode a low-confidence answer',
  },
}

export default function SourceBadge({ source }: { source: string }) {
  // An unrecognised source still renders, plainly. A source added to the
  // backend should appear here looking unstyled, not silently vanish.
  const style = STYLES[source]

  return (
    <span
      className={`inline-flex items-center rounded-md px-2 py-0.5 text-xs font-medium ring-1 ring-inset ${
        style?.className ?? 'bg-slate-100 text-slate-700 ring-slate-500/20'
      }`}
    >
      {style?.label ?? source}
    </span>
  )
}
