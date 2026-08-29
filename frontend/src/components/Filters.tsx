/**
 * Filters for the payments feed.
 *
 * The option lists are hardcoded rather than derived from the rows on screen,
 * and that is deliberate: deriving them would mean a category could only be
 * filtered for once a payment in it already happened to be loaded, so the one
 * filter you reach for when you suspect something is missing would be the one
 * option not offered. These are the closed sets the backend defines —
 * docs/taxonomy.md for categories, internal/decide for actions.
 */

const CATEGORIES = [
  'insufficient_funds',
  'bank_downtime',
  'hard_decline',
  'soft_decline',
  'network_error',
  'unknown',
]

const ACTIONS = [
  'retry_now',
  'retry_delayed',
  'suggest_alternate_method',
  'escalate',
  'no_retry',
]

export interface FilterState {
  category: string
  action: string
}

interface Props {
  value: FilterState
  onChange: (next: FilterState) => void
  /** Disables the controls mid-request so a filter cannot be changed twice before the first answer lands. */
  busy: boolean
}

export default function Filters({ value, onChange, busy }: Props) {
  return (
    <div className="flex flex-wrap items-end gap-4">
      <Select
        label="Category"
        value={value.category}
        options={CATEGORIES}
        disabled={busy}
        onChange={(category) => onChange({ ...value, category })}
      />
      <Select
        label="Latest action"
        value={value.action}
        options={ACTIONS}
        disabled={busy}
        onChange={(action) => onChange({ ...value, action })}
      />

      {(value.category || value.action) && (
        <button
          type="button"
          onClick={() => onChange({ category: '', action: '' })}
          disabled={busy}
          className="rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-700 hover:bg-slate-50 disabled:opacity-50"
        >
          Clear filters
        </button>
      )}
    </div>
  )
}

interface SelectProps {
  label: string
  value: string
  options: string[]
  disabled: boolean
  onChange: (value: string) => void
}

function Select({ label, value, options, disabled, onChange }: SelectProps) {
  return (
    <label className="flex flex-col gap-1 text-sm">
      <span className="font-medium text-slate-700">{label}</span>
      <select
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        className="min-w-52 rounded-md border border-slate-300 bg-white px-3 py-2 text-sm text-slate-900 disabled:opacity-50"
      >
        {/* The empty value is what the API client omits from the query string,
            so "All" is genuinely no filter rather than a filter on "". */}
        <option value="">All</option>
        {options.map((option) => (
          <option key={option} value={option}>
            {option}
          </option>
        ))}
      </select>
    </label>
  )
}
