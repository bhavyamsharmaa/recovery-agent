import { describe, expect, it } from 'vitest'
import {
  formatPoints,
  formatRate,
  improvementPoints,
  isImprovement,
  recoveredDelta,
} from './batchMath'
import { rupees } from './format'

/**
 * The recovery-math surface, tested against hand-verified values.
 *
 * Nothing here touches the network. Every expected value below was worked out
 * by hand, because a test that computes its expectation the same way the code
 * does would agree with a wrong implementation.
 */

describe('formatRate — a 0..1 rate rendered as a percentage', () => {
  // The case named in the brief, plus the two ways it is commonly got wrong.
  it('renders 0.6234 as "62.3%"', () => {
    expect(formatRate(0.6234)).toBe('62.3%')
  })

  it('does not render the raw fraction as a percentage', () => {
    // 0.6234 shown as "0.6%" or "0.6234%" is wrong by two orders of magnitude:
    // ₹62 lakh of recovery reported as ₹62 thousand.
    expect(formatRate(0.6234)).not.toBe('0.6234%')
    expect(formatRate(0.6234)).not.toBe('0.6%')
  })

  it('keeps one decimal rather than rounding to a whole number', () => {
    // 32.9% and 33.0% are different claims about money, and a whole-number
    // format would erase the difference between two runs.
    expect(formatRate(0.3286465184812608)).toBe('32.9%')
    expect(formatRate(0.33)).toBe('33.0%')
    expect(formatRate(0.329)).not.toBe('33%')
  })

  it.each([
    // rate, expected — each verified by hand.
    [0, '0.0%'],
    [1, '100.0%'],
    [0.5, '50.0%'],
    [0.199, '19.9%'],
    [0.2, '20.0%'],
    // Real values taken from stored batch_runs rows.
    [0.3095770617170779, '31.0%'],
    [0.19871817726380062, '19.9%'],
    [0.37213483146067416, '37.2%'],
    [0.2313180639585134, '23.1%'],
  ])('formats %s as %s', (rate, expected) => {
    expect(formatRate(rate)).toBe(expected)
  })

  it('rounds at the .05 boundary in a known, pinned way', () => {
    // Worth pinning because it is not obvious and it is not uniform. 0.1255*100
    // is exactly 12.55 in binary floating point and rounds up to 12.6; but
    // 0.1256*100 is 12.559999999999999, which also gives 12.6. Two different
    // float representations, same rendered answer.
    //
    // This is recorded as observed behaviour, not as a guarantee about decimal
    // rounding: toFixed operates on the float that survived the multiply, and
    // the multiply is where the imprecision enters.
    expect(formatRate(0.1255)).toBe('12.6%')
    expect(formatRate(0.1256)).toBe('12.6%')
    // A case where the float lands below the boundary and rounds down.
    expect(formatRate(0.1254)).toBe('12.5%')
  })
})

describe('formatPoints — a percentage-point difference with an explicit sign', () => {
  it('prefixes a gain with +', () => {
    expect(formatPoints(14.081676750216076)).toBe('+14.1')
    expect(formatPoints(8.78)).toBe('+8.8')
  })

  it('keeps the minus sign on a loss without doubling it', () => {
    // The bug this guards: prepending '-' to an already-negative toFixed would
    // produce "--7.2".
    expect(formatPoints(-7.159562)).toBe('-7.2')
    expect(formatPoints(-7.159562)).not.toBe('--7.2')
  })

  it('treats an exact tie as a signed zero, not a bare one', () => {
    expect(formatPoints(0)).toBe('+0.0')
  })
})

describe('recoveredDelta — the money difference between the two strategies', () => {
  it('is positive when the agent recovered more', () => {
    // Real run #5: 17222400 - 10705400 = 6517000 paise = ₹65,170.00
    expect(recoveredDelta(17222400, 10705400)).toBe(6517000)
    expect(rupees(6517000)).toBe('₹65,170.00')
  })

  it('is negative when the baseline recovered more', () => {
    // Real run #1, where a 3-payment batch went the baseline's way:
    // 3481100 - 4018000 = -536900 paise = ₹5,369.00 the other way.
    expect(recoveredDelta(3481100, 4018000)).toBe(-536900)
    expect(rupees(Math.abs(-536900))).toBe('₹5,369.00')
  })

  it('is zero on a tie', () => {
    expect(recoveredDelta(5000, 5000)).toBe(0)
  })

  it('stays exact in paise rather than drifting through rupees', () => {
    // Integer paise subtraction must not go via a float division. 1 paise of
    // drift across a batch is a wrong number on screen.
    expect(recoveredDelta(100000001, 100000000)).toBe(1)
  })
})

describe('isImprovement — which way the comparison went', () => {
  it('is true for a gain', () => {
    expect(isImprovement(11.09)).toBe(true)
  })

  it('is false for a loss', () => {
    expect(isImprovement(-7.16)).toBe(false)
  })

  it('counts an exact tie as not a regression', () => {
    // Nothing was lost, and colouring a tie as a failure would overstate it.
    expect(isImprovement(0)).toBe(true)
  })
})

describe('improvementPoints — an independent check on the server arithmetic', () => {
  // The view renders the API's improvement_points. These cases verify the
  // server's value against a derivation written separately, using figures read
  // back from real batch_runs rows.
  it.each([
    // rate, baselineRate, server's improvement_points
    [0.3095770617170779, 0.19871817726380062, 11.085888445327727],
    [0.3286465184812608, 0.240844, 8.780251848126079],
    [0.37213483146067416, 0.2313180639585134, 14.081676750216076],
  ])('derives the same points as the backend for %s vs %s', (rate, baseline, expected) => {
    expect(improvementPoints(rate, baseline)).toBeCloseTo(expected, 10)
  })

  it('is negative when the baseline wins', () => {
    // Run #1: 0.46420237095118083 vs 0.535798 → -7.159562904881916 points,
    // which formatPoints renders as "-7.2".
    expect(improvementPoints(0.46420237095118083, 0.535798)).toBeCloseTo(-7.159562904881916, 10)
    expect(formatPoints(improvementPoints(0.46420237095118083, 0.535798))).toBe('-7.2')
  })

  it('is not the ratio of the two rates', () => {
    // A plausible wrong implementation: rate / baseline * 100. For 0.4 and 0.2
    // that gives 200, where the correct answer is 20 percentage points.
    expect(improvementPoints(0.4, 0.2)).toBe(20)
    expect(improvementPoints(0.4, 0.2)).not.toBe(200)
  })
})

describe('rupees — paise rendered as currency', () => {
  // Formatting is part of the same surface: a right number displayed wrong is
  // just as misleading on the judging screen as a wrong number.
  it.each([
    [0, '₹0.00'],
    [100, '₹1.00'],
    [12345, '₹123.45'],
    // Indian digit grouping: the last three digits, then pairs.
    [46280000, '₹4,62,800.00'],
    [248121600, '₹24,81,216.00'],
    [272065700, '₹27,20,657.00'],
    [6517000, '₹65,170.00'],
  ])('renders %s paise as %s', (paise, expected) => {
    expect(rupees(paise)).toBe(expected)
  })

  it('never loses the paise component', () => {
    // Truncating to whole rupees would silently drop money.
    expect(rupees(444050)).toBe('₹4,440.50')
    expect(rupees(1)).toBe('₹0.01')
  })
})
