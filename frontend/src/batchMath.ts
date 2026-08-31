/**
 * The arithmetic and formatting behind the Batch Recovery Summary.
 *
 * This is the one place in the dashboard that computes rather than renders, and
 * the numbers it produces are the headline figures on screen. They live here as
 * pure functions — no fetching, no React — so they can be tested against
 * hand-verified values instead of only through a rendered component.
 *
 * What is NOT computed here, deliberately: the improvement in percentage points.
 * The backend sends `improvement_points` already derived, because it is the
 * feature's headline number and two clients deriving it independently is two
 * chances to derive it differently. `improvementPoints` below exists only so a
 * test can check the server's value against an independent derivation; the view
 * renders what the API sent.
 */

/**
 * formatRate turns a 0..1 rate into a one-decimal percentage.
 *
 * One decimal, not zero: 32.9% and 33.0% are different claims about money, and
 * rounding to a whole number would hide a real difference between two runs.
 * Not raw either — 0.6234 shown as "0.6%" or "0.6234%" would be off by two
 * orders of magnitude, which is the exact class of bug this guards.
 */
export function formatRate(rate: number): string {
  return `${(rate * 100).toFixed(1)}%`
}

/**
 * formatPoints renders a percentage-point difference with an explicit sign.
 *
 * The sign is always shown, including for a gain. "+14.1" and "14.1" read the
 * same to most people, but a reader scanning for a negative needs the positive
 * case to be unambiguous too — and a batch where the baseline wins is a real
 * outcome, not an error state.
 *
 * Negative values already carry their own minus sign from toFixed, so only the
 * non-negative branch prepends one.
 */
export function formatPoints(points: number): string {
  return `${points >= 0 ? '+' : ''}${points.toFixed(1)}`
}

/**
 * recoveredDelta is the money difference between the two strategies, in paise.
 *
 * Signed: negative when the naive baseline recovered more. The view takes the
 * absolute value for display and lets the surrounding words carry the direction
 * ("Additional" versus "Less"), but the sign has to survive to this function's
 * caller or that choice cannot be made correctly.
 */
export function recoveredDelta(recoveredPaise: number, baselinePaise: number): number {
  return recoveredPaise - baselinePaise
}

/**
 * isImprovement reports whether the agent beat the baseline.
 *
 * Zero counts as an improvement rather than a regression: the two strategies
 * tied, nothing was lost, and colouring a tie as a failure would overstate it.
 * The boundary is stated here rather than inline so it is one decision in one
 * place, testable on its own.
 */
export function isImprovement(points: number): boolean {
  return points >= 0
}

/**
 * improvementPoints derives the percentage-point difference from two rates.
 *
 * NOT used by the view — the API sends this value and the view renders it. It
 * exists so a test can verify the server's arithmetic independently, and so the
 * definition of "improvement" is written down in the frontend in a form that can
 * be checked rather than only assumed.
 */
export function improvementPoints(rate: number, baselineRate: number): number {
  return (rate - baselineRate) * 100
}
