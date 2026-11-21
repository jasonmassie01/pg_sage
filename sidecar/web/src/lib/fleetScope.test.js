import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  FLEET_SUMMARY_FROM, PICKER_PAGE, RECENT_MAX, databaseSearchURL, parseTagScope,
  pushRecent, readRecent, summarizes,
} from './fleetScope'

// ux-10b: the scope helpers behind the header picker and the "all" scope
// fleet summaries. No concurrent access tests: pure functions plus one
// synchronous localStorage key.

beforeEach(() => localStorage.clear())
afterEach(() => vi.restoreAllMocks())

describe('summarizes', () => {
  it('summarizes the all scope from the threshold up', () => {
    expect(summarizes('all', FLEET_SUMMARY_FROM)).toBe(true)
    expect(summarizes('all', 500)).toBe(true)
    expect(summarizes(undefined, FLEET_SUMMARY_FROM)).toBe(true)
    expect(summarizes('', FLEET_SUMMARY_FROM)).toBe(true)
  })

  it('keeps details below the threshold and for one database', () => {
    expect(summarizes('all', FLEET_SUMMARY_FROM - 1)).toBe(false)
    expect(summarizes('all', 0)).toBe(false)
    expect(summarizes('orders', 500)).toBe(false)
  })

  it('treats a missing count as no fleet', () => {
    expect(summarizes('all', undefined)).toBe(false)
    expect(summarizes('all', Number.NaN)).toBe(false)
  })
})

describe('parseTagScope', () => {
  it('reads a tag scope typed into the picker', () => {
    expect(parseTagScope('tag:tier=prod')).toBe('tier=prod')
    expect(parseTagScope('  TAG: tier = prod ')).toBe('tier = prod')
    expect(parseTagScope('tag:prod')).toBe('prod')
  })

  it('is null for a name, an empty tag or no text', () => {
    expect(parseTagScope('payments')).toBeNull()
    expect(parseTagScope('tag:')).toBeNull()
    expect(parseTagScope('tag:   ')).toBeNull()
    expect(parseTagScope('tag:=prod')).toBeNull()
    expect(parseTagScope('')).toBeNull()
    expect(parseTagScope(null)).toBeNull()
    expect(parseTagScope(undefined)).toBeNull()
  })
})

describe('databaseSearchURL', () => {
  it('asks the fleet list for one page of names matching the query', () => {
    expect(PICKER_PAGE).toBe(50)
    expect(databaseSearchURL({ q: 'pay' }))
      .toBe('/api/v1/databases?q=pay&sort=name&order=asc&limit=50')
  })

  it('filters by a tag scope and continues from a cursor', () => {
    expect(databaseSearchURL({ tag: 'tier=prod', cursor: 'abc' }))
      .toBe('/api/v1/databases?tag=tier%3Dprod&sort=name&order=asc&limit=50&cursor=abc')
  })

  it('escapes a query instead of passing it through', () => {
    expect(databaseSearchURL({ q: 'a&b=c' }))
      .toBe('/api/v1/databases?q=a%26b%3Dc&sort=name&order=asc&limit=50')
  })

  it('lists the first page with no query', () => {
    expect(databaseSearchURL({})).toBe('/api/v1/databases?sort=name&order=asc&limit=50')
  })
})

describe('recent databases', () => {
  it('keeps the most recent first, without repeats, at most RECENT_MAX', () => {
    for (const name of ['a', 'b', 'c', 'b', 'd', 'e', 'f', 'g']) pushRecent(name)
    const recent = readRecent()
    expect(recent).toEqual(['g', 'f', 'e', 'd', 'b'])
    expect(recent).toHaveLength(RECENT_MAX)
  })

  it('never records the all scope or an empty name', () => {
    pushRecent('orders')
    pushRecent('all')
    pushRecent('')
    pushRecent(undefined)
    expect(readRecent()).toEqual(['orders'])
  })

  it('reads a corrupt or foreign value as no recents', () => {
    localStorage.setItem('pg_sage_recent_dbs', '{not json')
    expect(readRecent()).toEqual([])
    localStorage.setItem('pg_sage_recent_dbs', '{"a":1}')
    expect(readRecent()).toEqual([])
    localStorage.setItem('pg_sage_recent_dbs', '["ok", 7, "", null]')
    expect(readRecent()).toEqual(['ok'])
  })

  it('works with site storage blocked', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new DOMException('blocked', 'SecurityError')
    })
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('blocked', 'SecurityError')
    })
    expect(readRecent()).toEqual([])
    expect(pushRecent('orders')).toEqual(['orders'])
  })
})
