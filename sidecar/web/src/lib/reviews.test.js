import { describe, expect, it } from 'vitest'
import {
  canDecideReviews, concurrenceTotal, dbaVerdictLabel, formatDuration, groupFindings,
  parseReviewRoute, prLabel, receiptState, reviewDetailURL, reviewLink, reviewsListURL,
  reviewSummaryURL, reviewVerdictURL,
} from './reviews'

// Change Review helpers: API URLs per database selection, the review
// route, labels and duration formatting. Unknown is never shown as zero.

describe('review URLs', () => {
  it('lists the fleet or one database', () => {
    expect(reviewsListURL('all')).toBe('/api/v1/change-reviews')
    expect(reviewsListURL('')).toBe('/api/v1/change-reviews')
    expect(reviewsListURL(undefined)).toBe('/api/v1/change-reviews')
    expect(reviewsListURL('orders')).toBe('/api/v1/change-reviews?database=orders')
    expect(reviewSummaryURL('all')).toBe('/api/v1/change-reviews/summary')
    expect(reviewSummaryURL('a b')).toBe('/api/v1/change-reviews/summary?database=a%20b')
  })

  it('addresses one review and its verdict by database and id', () => {
    expect(reviewDetailURL('orders', 'abc')).toBe(
      '/api/v1/databases/orders/change-reviews/abc')
    expect(reviewDetailURL('o/x', 'a?b')).toBe(
      '/api/v1/databases/o%2Fx/change-reviews/a%3Fb')
    expect(reviewVerdictURL('orders', 'abc')).toBe(
      '/api/v1/databases/orders/change-reviews/abc/verdict')
    expect(reviewDetailURL('all', 'abc')).toBeNull()
    expect(reviewDetailURL('', 'abc')).toBeNull()
    expect(reviewDetailURL('orders', '')).toBeNull()
  })

  it('round-trips the review route', () => {
    const link = reviewLink('db one', 'id&1')
    expect(link).toBe('#/reviews?id=id%261&database=db+one')
    expect(parseReviewRoute(link)).toEqual({ id: 'id&1', database: 'db one' })
    expect(parseReviewRoute('#/reviews')).toEqual({ id: '', database: '' })
    expect(parseReviewRoute('')).toEqual({ id: '', database: '' })
    expect(parseReviewRoute('#/reviews?id=x')).toEqual({ id: 'x', database: '' })
  })
})

describe('review labels', () => {
  it('names a PR by title, repo#number, URL or review id', () => {
    expect(prLabel({ pr_title: 'T', repo: 'a/b', pr_number: 3 })).toBe('T')
    expect(prLabel({ repo: 'a/b', pr_number: 3, pr_url: 'https://x' })).toBe('a/b#3')
    expect(prLabel({ pr_url: 'https://x/pull/3' })).toBe('https://x/pull/3')
    expect(prLabel({ id: '12345678-aaaa' })).toBe('review 12345678')
    expect(prLabel(null)).toBe('review')
  })

  it('labels the DBA verdict', () => {
    expect(dbaVerdictLabel({ dba_verdict: 'concur' })).toBe('Concur')
    expect(dbaVerdictLabel({ dba_verdict: 'concur_with_edits' })).toBe('Concur with edits')
    expect(dbaVerdictLabel({ dba_verdict: 'override', dba_direction: 'too_strict' }))
      .toBe('Override (too strict)')
    expect(dbaVerdictLabel({ state: 'complete', verdict: 'risky' })).toBe('Awaiting DBA')
    expect(dbaVerdictLabel({ state: 'complete', verdict: 'review' })).toBe('Awaiting DBA')
    expect(dbaVerdictLabel({ state: 'complete', verdict: 'safe' })).toBe('—')
    expect(dbaVerdictLabel({ state: 'running' })).toBe('—')
  })

  it('labels the receipt state from landing and receipt', () => {
    const r = over => ({ state: 'complete', landing_state: 'awaiting',
      receipt_verdict: 'pending', ...over })
    expect(receiptState(r())).toBe('awaiting landing')
    expect(receiptState(r({ landing_state: 'landed' }))).toBe('landed, receipt pending')
    expect(receiptState(r({ landing_state: 'landed', receipt_verdict: 'clean' })))
      .toBe('clean')
    expect(receiptState(r({ landing_state: 'landed',
      receipt_verdict: 'clean_low_traffic' }))).toBe('clean, low traffic')
    expect(receiptState(r({ landing_state: 'landed', receipt_verdict: 'regressed' })))
      .toBe('regressed')
    expect(receiptState(r({ landing_state: 'expired', receipt_verdict: 'unverifiable' })))
      .toBe('landing expired')
    expect(receiptState(r({ landing_state: 'not_applicable' }))).toBe('already in place')
    expect(receiptState(r({ state: 'running' }))).toBe('review running')
    expect(receiptState(r({ state: 'failed' }))).toBe('review failed')
  })

  it('lets only operators and admins decide', () => {
    expect(canDecideReviews({ role: 'admin' })).toBe(true)
    expect(canDecideReviews({ role: 'operator' })).toBe(true)
    expect(canDecideReviews({ role: 'viewer' })).toBe(false)
    expect(canDecideReviews(null)).toBe(false)
  })
})

describe('review figures', () => {
  it('groups findings most severe first and drops empty groups', () => {
    const groups = groupFindings([
      { severity: 'info', title: 'i' }, { severity: 'critical', title: 'c' },
      { severity: 'info', title: 'i2' },
    ])
    expect(groups.map(g => [g.severity, g.findings.map(f => f.title)])).toEqual([
      ['critical', ['c']], ['info', ['i', 'i2']]])
    expect(groupFindings(null)).toEqual([])
  })

  it('formats durations and shows unknown as a dash', () => {
    expect(formatDuration(null)).toBe('—')
    expect(formatDuration(undefined)).toBe('—')
    expect(formatDuration(0)).toBe('0m')
    expect(formatDuration(95)).toBe('2m')
    expect(formatDuration(3599)).toBe('60m')
    expect(formatDuration(3600)).toBe('1.0h')
    expect(formatDuration(5400)).toBe('1.5h')
    expect(formatDuration(86399)).toBe('24.0h')
    expect(formatDuration(86400)).toBe('1.0d')
  })

  it('totals concurrence across verdict classes', () => {
    expect(concurrenceTotal({ safe: { n: 1, of: 2 }, risky: { n: 3, of: 4 } }))
      .toEqual({ n: 4, of: 6, rate: 4 / 6 })
    expect(concurrenceTotal({})).toEqual({ n: 0, of: 0, rate: null })
    expect(concurrenceTotal(null)).toEqual({ n: 0, of: 0, rate: null })
  })
})
