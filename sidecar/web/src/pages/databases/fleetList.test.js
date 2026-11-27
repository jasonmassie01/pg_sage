import { describe, expect, it } from 'vitest'
import { DEFAULT_SORT, addTag, ariaSort, fleetListURL, fleetRows, nextSort, parseTagAssignment,
  tagUpdateBody } from './fleetList'

// ux-10a: the fleet table's URL, sort and join helpers. No concurrency
// test: pure functions.

describe('fleetListURL', () => {
  it('always asks for a sorted page of 50', () => {
    expect(fleetListURL()).toBe('/api/v1/databases?sort=name&order=asc&limit=50')
  })

  it('trims the search, keeps each tag once and encodes them', () => {
    expect(fleetListURL({ q: '  a&b ', tags: ['tier=prod', ' tier=prod ', '', 'x'],
      sort: { sort: 'health', order: 'desc' }, limit: 500 }))
      .toBe('/api/v1/databases?q=a%26b&tag=tier%3Dprod&tag=x&sort=health&order=desc&limit=500')
  })

  it('drops a blank search', () => {
    expect(fleetListURL({ q: '   ' })).not.toContain('q=')
  })
})

describe('nextSort and ariaSort', () => {
  it('starts a new column at its default order and flips the current one', () => {
    const health = nextSort(DEFAULT_SORT, 'Health')
    expect(health).toEqual({ sort: 'health', order: 'asc' })
    expect(nextSort(health, 'Health')).toEqual({ sort: 'health', order: 'desc' })
    expect(nextSort(DEFAULT_SORT, 'Findings')).toEqual({ sort: 'open_issues', order: 'desc' })
    expect(nextSort(DEFAULT_SORT, 'Name')).toEqual({ sort: 'name', order: 'desc' })
  })

  it('leaves an unsortable column alone', () => {
    expect(nextSort(DEFAULT_SORT, 'Host')).toBe(DEFAULT_SORT)
    expect(ariaSort(DEFAULT_SORT, 'Host')).toBeUndefined()
  })

  it('names the sorted column and its direction', () => {
    expect(ariaSort(DEFAULT_SORT, 'Name')).toBe('ascending')
    expect(ariaSort({ sort: 'name', order: 'desc' }, 'Name')).toBe('descending')
    expect(ariaSort({ sort: 'name', order: 'desc' }, 'Database')).toBe('descending')
    expect(nextSort({ sort: 'health', order: 'asc' }, 'Database'))
      .toEqual({ sort: 'name', order: 'asc' })
    expect(ariaSort(DEFAULT_SORT, 'Health')).toBe('none')
  })
})

describe('fleetRows', () => {
  const regs = [{ id: 3, name: 'orders' }, { id: 0, name: 'local' }]

  it('joins on database id, then name, and marks config-file rows', () => {
    const rows = fleetRows(regs, [
      { name: 'orders-old', database_id: 3 }, { name: 'local', database_id: 0 },
      { name: 'yaml', database_id: 0 },
    ])
    expect(rows.map(r => [r.key, r.registration?.name ?? null])).toEqual([
      ['orders-old', 'orders'], ['local', 'local'], ['yaml', null]])
  })

  it('never joins a same-named row of another database id', () => {
    expect(fleetRows(regs, [{ name: 'orders', database_id: 9 }])[0].registration).toBeNull()
  })

  it('returns no rows for an empty page', () => {
    expect(fleetRows(regs, [])).toEqual([])
  })
})

describe('addTag', () => {
  it('adds a trimmed tag once, ignoring case and blanks', () => {
    expect(addTag([], ' tier=prod ')).toEqual(['tier=prod'])
    const tags = ['tier=prod']
    expect(addTag(tags, 'TIER=PROD')).toBe(tags)
    expect(addTag(tags, '  ')).toBe(tags)
    expect(addTag(tags, undefined)).toBe(tags)
  })

  it('refuses a tag with no key, which the server answers with a 400', () => {
    const tags = ['tier=prod']
    expect(addTag(tags, '=prod')).toBe(tags)
    expect(addTag(tags, ' = prod')).toBe(tags)
    expect(addTag(tags, 'team=')).toEqual(['tier=prod', 'team='])
  })
})

describe('parseTagAssignment', () => {
  it('reads a trimmed key=value, keeping an = inside the value', () => {
    expect(parseTagAssignment(' tier = prod ')).toEqual({ key: 'tier', value: 'prod' })
    expect(parseTagAssignment('url=a=b')).toEqual({ key: 'url', value: 'a=b' })
  })

  it('refuses a bare tag, a blank key or value, and nothing', () => {
    for (const raw of ['prod', '=prod', 'tier=', ' = ', '', undefined, null]) {
      expect(parseTagAssignment(raw)).toBeNull()
    }
  })
})

describe('tagUpdateBody', () => {
  const rec = { id: 4, name: 'orders', host: 'h', port: 6432, database_name: 'app',
    username: 'sage', sslmode: 'verify-full', max_connections: 5, enabled: true,
    trust_level: 'advisory', execution_mode: 'manual', tags: { tier: 'dev', team: 'core' },
    action_identity: { username: 'sage_exec' }, created_at: 'x' }

  it('sends every stored field back with the tag set, and keeps the password', () => {
    expect(tagUpdateBody(rec, { key: 'tier', value: 'prod' })).toEqual({
      name: 'orders', host: 'h', port: 6432, database_name: 'app', username: 'sage',
      password: '', sslmode: 'verify-full', max_connections: 5, trust_level: 'advisory',
      execution_mode: 'manual', tags: { tier: 'prod', team: 'core' },
    })
    expect(rec.tags).toEqual({ tier: 'dev', team: 'core' })
  })

  it('adds the first tag to a record without tags', () => {
    expect(tagUpdateBody({ ...rec, tags: null }, { key: 'k', value: 'v' }).tags)
      .toEqual({ k: 'v' })
  })
})
