import { describe, expect, it } from 'vitest'
import { formatTotal, withCursor } from './listPaging'

// The findings and actions lists report a total capped at 1000 with
// total_capped (perf v1.8.3): past the cap the count is "1000+", never a
// bare 1000 that reads as exact.
describe('formatTotal', () => {
  it('marks a capped total', () => {
    expect(formatTotal({ total: 1000, total_capped: true })).toBe('1000+')
  })
  it('shows an exact total as is', () => {
    expect(formatTotal({ total: 1000, total_capped: false })).toBe('1000')
    expect(formatTotal({ total: 37 })).toBe('37')
  })
  it('treats a missing response or total as zero', () => {
    expect(formatTotal(null)).toBe('0')
    expect(formatTotal({})).toBe('0')
  })
})

describe('withCursor', () => {
  it('adds the cursor to a URL with or without a query', () => {
    expect(withCursor('/api/v1/findings?status=open', 'a b'))
      .toBe('/api/v1/findings?status=open&cursor=a%20b')
    expect(withCursor('/api/v1/actions', 'c1')).toBe('/api/v1/actions?cursor=c1')
  })
  it('replaces an offset (the cursor pages, the offset is capped)', () => {
    expect(withCursor('/api/v1/actions?offset=50&limit=50', 'c2'))
      .toBe('/api/v1/actions?limit=50&cursor=c2')
  })
})
