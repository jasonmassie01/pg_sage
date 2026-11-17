import { describe, expect, it } from 'vitest'
import { landingTab } from './landingTab'

// ux-05a win 1: Actions opens on Needs your review when the role can
// review and approvals wait, else on Proposed changes; it waits while the
// count loads and names a count error instead of guessing.
// No concurrent access tests: a pure function of one snapshot.

const reviewer = { canReview: true, pendingCount: 0, loading: false, error: null }

describe('landingTab', () => {
  it('opens on Needs your review when approvals wait', () => {
    expect(landingTab({ ...reviewer, pendingCount: 3 })).toEqual({ tab: 'pending', error: null })
  })

  it('opens on Needs your review at exactly one waiting approval', () => {
    expect(landingTab({ ...reviewer, pendingCount: 1 }).tab).toBe('pending')
  })

  it('opens on Proposed changes when nothing waits', () => {
    expect(landingTab(reviewer)).toEqual({ tab: 'recommendations', error: null })
  })

  it('never opens a viewer on Needs your review', () => {
    expect(landingTab({ ...reviewer, canReview: false, pendingCount: 5 }))
      .toEqual({ tab: 'recommendations', error: null })
  })

  it('waits while the count loads', () => {
    expect(landingTab({ ...reviewer, loading: true })).toEqual({ tab: null, error: null })
  })

  it('does not wait once a part of the queue already shows waiting work', () => {
    expect(landingTab({ ...reviewer, loading: true, pendingCount: 2 }).tab).toBe('pending')
  })

  it('opens on Proposed changes and names the error when the count fails', () => {
    expect(landingTab({ ...reviewer, error: 'pending list: 500' }))
      .toEqual({ tab: 'recommendations', error: 'pending list: 500' })
  })

  it('treats a missing or non-numeric count as zero', () => {
    expect(landingTab({ ...reviewer, pendingCount: undefined }).tab).toBe('recommendations')
    expect(landingTab({ ...reviewer, pendingCount: Number.NaN }).tab).toBe('recommendations')
    expect(landingTab({ ...reviewer, pendingCount: -1 }).tab).toBe('recommendations')
  })

  it('treats no input as a viewer', () => {
    expect(landingTab({})).toEqual({ tab: 'recommendations', error: null })
    expect(landingTab()).toEqual({ tab: 'recommendations', error: null })
  })
})
