import { describe, expect, it } from 'vitest'
import {
  LETTER_STYLES, contrastRatio, dimensionUnknownReason, gradeBadge, modelPassText,
  overallUnknownReason, planHref, reviewHref, reviewParams, shortReason, viewedURL,
  gradeURL,
} from './review'

// review-01b: the Review page's wording and arithmetic. No concurrent
// access tests: pure functions of their arguments.

describe('gradeBadge', () => {
  it('shows a valid letter and score as that letter', () => {
    expect(gradeBadge('B', 84.5)).toEqual({ letter: 'B', unknown: false, reason: '' })
  })
  it('never shows null as a letter: it is unknown with the reason given', () => {
    expect(gradeBadge(null, 0, 'coverage 40%')).toEqual(
      { letter: null, unknown: true, reason: 'coverage 40%' })
  })
  it('refuses a letter outside A-F and a score outside 0-100', () => {
    for (const [letter, score] of [['Z', 50], ['E', 50], ['', 50], ['b', 50], ['A', 101],
      ['A', -0.01], ['B', Number.NaN], ['B', '80'], [7, 50]]) {
      expect(gradeBadge(letter, score)).toEqual(
        { letter: null, unknown: true, reason: 'invalid grade' })
    }
  })
  it('accepts the score boundaries 0 and 100 and a missing score', () => {
    expect(gradeBadge('F', 0).unknown).toBe(false)
    expect(gradeBadge('A', 100).unknown).toBe(false)
    expect(gradeBadge('A', undefined).unknown).toBe(false)
  })
})

describe('unknown reasons', () => {
  it('names the coverage a dimension lacks', () => {
    expect(dimensionUnknownReason({ letter: null, coverage: 42.5, checks: [] }))
      .toBe('coverage 42.5%, below 60%')
  })
  it('names a dimension with no applicable check', () => {
    expect(dimensionUnknownReason({ letter: null, coverage: 0, checks: [
      { check: 'a', status: 'not_applicable' }] })).toBe('no check applies on this platform')
  })
  it('names how many dimensions the overall needs', () => {
    expect(overallUnknownReason({ known_dimensions: 3 }))
      .toBe('3 of 8 dimensions known, 4 needed')
    expect(overallUnknownReason({})).toBe('0 of 8 dimensions known, 4 needed')
  })
})

describe('shortReason', () => {
  it('keeps a 100-character reason whole, with no title', () => {
    const text = 'x'.repeat(100)
    expect(shortReason(text)).toEqual({ text, title: undefined })
  })
  it('truncates a 101-character reason and keeps the full text in title', () => {
    const text = 'y'.repeat(101)
    const got = shortReason(text)
    expect(got.title).toBe(text)
    expect(got.text).toHaveLength(100)
    expect(got.text.endsWith('…')).toBe(true)
  })
  it('is empty for no reason', () => {
    expect(shortReason(undefined)).toEqual({ text: '', title: undefined })
  })
})

describe('modelPassText', () => {
  it('says which model did the pass', () => {
    expect(modelPassText({ status: 'done', model: 'gpt-x' })).toBe('Model pass: done by gpt-x')
  })
  it('says why it was skipped or failed', () => {
    expect(modelPassText({ status: 'skipped', reason: 'no model configured' }))
      .toBe('Model pass: skipped: no model configured')
    expect(modelPassText({ status: 'failed', reason: 'timeout' }))
      .toBe('Model pass: failed: timeout')
    expect(modelPassText({ status: 'skipped' })).toBe('Model pass: skipped: no reason given')
    expect(modelPassText(null)).toBe('Model pass: skipped: no reason given')
  })
})

describe('links', () => {
  it('encodes the database in every URL and link', () => {
    expect(gradeURL('a b/c')).toBe('/api/v1/databases/a%20b%2Fc/review/grade')
    expect(viewedURL('a b', 12)).toBe('/api/v1/databases/a%20b/review/runs/12/viewed')
    expect(reviewHref('a&b')).toBe('#/review?db=a%26b')
    expect(planHref('p-1', 'a b')).toBe('#/actions?tab=plans&plan=p-1&database=a%20b')
  })
  it('reads the database from ?db=, else the picker, never "all"', () => {
    expect(reviewParams('#/review?db=orders', 'all')).toBe('orders')
    expect(reviewParams('/review', 'billing')).toBe('billing')
    expect(reviewParams('/review', 'all')).toBe(null)
    expect(reviewParams('/review?db=', undefined)).toBe(null)
  })
})

describe('letter colours', () => {
  it('reach 4.5:1 against their text for every letter and unknown', () => {
    for (const [key, style] of Object.entries(LETTER_STYLES)) {
      expect(contrastRatio(style.color, style.background), key).toBeGreaterThanOrEqual(4.5)
    }
    expect(Object.keys(LETTER_STYLES).sort()).toEqual(['A', 'B', 'C', 'D', 'F', 'unknown'])
  })
  it('computes the WCAG ratio of black on white as 21', () => {
    expect(contrastRatio('#000000', '#ffffff')).toBeCloseTo(21, 5)
    expect(contrastRatio('#777777', '#777777')).toBeCloseTo(1, 5)
  })
})
