import { describe, expect, it } from 'vitest'
import { filterDatabases, matchesDatabase } from './databaseFilter'

// ux-05e (win 19): the fleet filter matches a database's name, host or tags,
// case-insensitively and literally (no regular expression).
// No concurrency test: pure functions over their arguments.

const orders = { name: 'orders', host: 'db1.example.com', tags: { env: 'prod' } }
const billing = { name: 'Billing', host: 'replica.example.com', tags: { team: 'fin' } }
const odd = { name: 'a.*b', host: 'h', tags: null }

describe('matchesDatabase', () => {
  it('matches the name, host or a tag, ignoring case', () => {
    expect(matchesDatabase(orders, 'ORD')).toBe(true)
    expect(matchesDatabase(billing, 'billing')).toBe(true)
    expect(matchesDatabase(billing, 'REPLICA.example')).toBe(true)
    expect(matchesDatabase(orders, 'prod')).toBe(true)
    expect(matchesDatabase(orders, 'env')).toBe(true)
    expect(matchesDatabase(orders, 'env=prod')).toBe(true)
    expect(matchesDatabase(billing, 'prod')).toBe(false)
  })

  it('matches regular expression characters literally', () => {
    expect(matchesDatabase(orders, '.*')).toBe(false)
    expect(matchesDatabase(odd, '.*')).toBe(true)
    expect(matchesDatabase(orders, 'o[rd]')).toBe(false)
    expect(matchesDatabase(orders, '(')).toBe(false)
  })

  it('treats an empty or blank query as matching everything', () => {
    expect(matchesDatabase(orders, '')).toBe(true)
    expect(matchesDatabase(orders, '   ')).toBe(true)
    expect(matchesDatabase(orders, undefined)).toBe(true)
  })

  it('tolerates missing fields and non-string tag values', () => {
    expect(matchesDatabase({}, 'x')).toBe(false)
    expect(matchesDatabase(null, 'x')).toBe(false)
    expect(matchesDatabase({ name: 'n', tags: { tier: 1 } }, 'tier=1')).toBe(true)
    expect(matchesDatabase({ name: 'n', tags: ['a'] }, 'zzz')).toBe(false)
  })
})

describe('filterDatabases', () => {
  it('keeps the matching databases in order', () => {
    expect(filterDatabases([orders, billing, odd], 'example').map(d => d.name))
      .toEqual(['orders', 'Billing'])
    expect(filterDatabases([orders, billing], ' ord ').map(d => d.name)).toEqual(['orders'])
  })

  it('returns the list itself for an empty query and [] for no list', () => {
    const list = [orders, billing]
    expect(filterDatabases(list, '')).toBe(list)
    expect(filterDatabases(null, 'x')).toEqual([])
    expect(filterDatabases(undefined, '')).toEqual([])
  })
})
