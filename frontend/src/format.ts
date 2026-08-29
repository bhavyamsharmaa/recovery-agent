/**
 * Formatting helpers shared by the views.
 *
 * These live apart from the components because the same value has to read the
 * same way everywhere: an amount that renders as ₹4,375.60 in the feed and
 * ₹4375.6 in the detail view is two claims about one number.
 */

/**
 * rupees renders paise as currency.
 *
 * The backend stores paise because that is what Razorpay sends and integers do
 * not drift. The division happens here, at the last possible moment, and only
 * for display — nothing downstream of this function does arithmetic on it.
 */
export function rupees(paise: number): string {
  return new Intl.NumberFormat('en-IN', {
    style: 'currency',
    currency: 'INR',
    minimumFractionDigits: 2,
  }).format(paise / 100)
}

/**
 * confidence renders a model score as a percentage.
 *
 * Null is rendered as an em dash rather than 0%, because the two are different
 * claims: a stopping-rule or fallback decision had no model behind it at all,
 * and showing 0% would report it as a decision the model was certain was wrong.
 */
export function confidence(score: number | null): string {
  if (score === null) return '—'
  return `${Math.round(score * 100)}%`
}

/**
 * shortId truncates a payment id for a table cell, keeping the tail.
 *
 * The tail is the distinguishing part — ids from one simulator run share a
 * prefix and differ only at the end — so a leading truncation would render a
 * column of identical-looking rows.
 */
export function shortId(paymentId: string, keep = 18): string {
  if (paymentId.length <= keep) return paymentId
  return `…${paymentId.slice(-keep)}`
}

/**
 * timestamp renders an RFC 3339 string as readable UTC.
 *
 * UTC rather than the reader's local zone, and labelled as such, because this
 * is an audit view: a screenshot of it has to be comparable against what
 * cmd/trace-payment prints and against the raw column, and a reader should
 * never have to work out which zone the person who took the screenshot was in.
 * The backend sends every timestamp as UTC with a trailing Z, so this only has
 * to keep it that way.
 *
 * The string is returned unchanged if it will not parse, so an unexpected
 * format shows as itself rather than as "Invalid Date".
 */
export function timestamp(iso: string): string {
  const parsed = new Date(iso)
  if (Number.isNaN(parsed.getTime())) return iso

  return `${parsed.toLocaleString('en-GB', {
    timeZone: 'UTC',
    day: '2-digit',
    month: 'short',
    year: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  })} UTC`
}
