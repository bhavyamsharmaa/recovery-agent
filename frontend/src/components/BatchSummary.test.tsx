import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it } from 'vitest'
import type { BatchRun } from '../api'
import { Figures } from './BatchSummary'

/**
 * The one render test in this project, and it exists for a specific reason: to
 * prove the comparison does not assume the agent wins.
 *
 * Figures is a pure function of a BatchRun, so this needs no fetch mock and no
 * backend — the run objects below are hand-written, with the figures worked out
 * by hand rather than copied from a live response.
 */

afterEach(cleanup)

function run(overrides: Partial<BatchRun> = {}): BatchRun {
  return {
    id: 1,
    started_at: '2026-08-29T14:25:24Z',
    completed_at: '2026-08-29T14:26:03Z',
    batch_size: 20,
    rng_seed: 1788013525,
    total_at_risk_paise: 46280000, // ₹4,62,800.00
    total_recovered_paise: 17222400, // ₹1,72,224.00
    recovery_rate: 0.37213483146067416, // 37.2%
    baseline_recovered_paise: 10705400, // ₹1,07,054.00
    baseline_recovery_rate: 0.2313180639585134, // 23.1%
    improvement_points: 14.081676750216076, // +14.1
    ...overrides,
  }
}

describe('Figures — the agent ahead of the baseline', () => {
  it('shows both rates and a positive improvement', () => {
    render(<Figures run={run()} />)

    expect(screen.getByText('37.2%')).toBeDefined()
    expect(screen.getByText('23.1%')).toBeDefined()
    expect(screen.getByText('+14.1 percentage points')).toBeDefined()
    expect(screen.getByText('₹1,72,224.00')).toBeDefined()
    expect(screen.getByText('₹1,07,054.00')).toBeDefined()
    expect(screen.getByText('₹4,62,800.00')).toBeDefined()
    // The rupee delta is computed client-side: 17222400 - 10705400 = 6517000.
    expect(screen.getByText('₹65,170.00')).toBeDefined()
    expect(screen.getByText(/Additional money recovered/)).toBeDefined()
  })
})

describe('Figures — the baseline ahead of the agent', () => {
  // A real occurrence, not a hypothetical: batch_runs id=1, a 3-payment run,
  // came out this way on small-sample noise. The view must report it plainly
  // rather than formatting around an assumption that the agent always wins.
  const losing = run({
    id: 1,
    batch_size: 3,
    rng_seed: 111,
    total_at_risk_paise: 7499100, // ₹74,991.00
    total_recovered_paise: 3481100, // ₹34,811.00
    recovery_rate: 0.46420237095118083, // 46.4%
    baseline_recovered_paise: 4018000, // ₹40,180.00
    baseline_recovery_rate: 0.535798, // 53.6%
    improvement_points: -7.159562904881917, // -7.2
  })

  it('still renders both rates correctly', () => {
    render(<Figures run={losing} />)
    // Neither figure is suppressed, swapped, or clamped because the comparison
    // went the wrong way.
    expect(screen.getByText('46.4%')).toBeDefined()
    expect(screen.getByText('53.6%')).toBeDefined()
    expect(screen.getByText('₹34,811.00')).toBeDefined()
    expect(screen.getByText('₹40,180.00')).toBeDefined()
  })

  it('shows the improvement as negative rather than hiding or flipping it', () => {
    render(<Figures run={losing} />)

    expect(screen.getByText('-7.2 percentage points')).toBeDefined()
    // Not an absolute value dressed up as a gain.
    expect(screen.queryByText('+7.2 percentage points')).toBeNull()
    expect(screen.queryByText('7.2 percentage points')).toBeNull()
  })

  it('changes the wording to match the direction', () => {
    render(<Figures run={losing} />)

    // "Additional money recovered: ₹5,369.00" would be a false statement when
    // the baseline is the one that recovered more.
    expect(screen.getByText(/Less money recovered/)).toBeDefined()
    expect(screen.queryByText(/Additional money recovered/)).toBeNull()
    // The magnitude is still shown: |3481100 - 4018000| = 536900 paise.
    expect(screen.getByText('₹5,369.00')).toBeDefined()
  })

  it('marks the losing case visually rather than styling it as a win', () => {
    const { container } = render(<Figures run={losing} />)
    // The improvement panel turns amber when the comparison went the wrong way.
    // Asserting on the class is coarse, but the alternative is asserting nothing
    // about the one signal a reader scanning the screen actually notices.
    expect(container.innerHTML).toContain('amber')
  })
})

describe('Figures — an exact tie', () => {
  it('reports +0.0 rather than treating the tie as a loss', () => {
    render(
      <Figures
        run={run({
          total_recovered_paise: 10705400,
          recovery_rate: 0.2313180639585134,
          improvement_points: 0,
        })}
      />,
    )
    expect(screen.getByText('+0.0 percentage points')).toBeDefined()
    expect(screen.getByText(/Additional money recovered/)).toBeDefined()
    expect(screen.getByText('₹0.00')).toBeDefined()
  })
})

describe('Figures — an incomplete run', () => {
  it('says it has no figures rather than rendering zeros', () => {
    // A run that started and crashed has NULL result columns. Rendering 0%
    // would claim it completed having recovered nothing, which is a different
    // and false statement.
    render(
      <Figures
        run={run({
          completed_at: null,
          total_at_risk_paise: null,
          total_recovered_paise: null,
          recovery_rate: null,
          baseline_recovered_paise: null,
          baseline_recovery_rate: null,
          improvement_points: null,
        })}
      />,
    )
    expect(screen.getByText(/never completed/)).toBeDefined()
    expect(screen.queryByText('0.0%')).toBeNull()
  })
})
