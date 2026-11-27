import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  BULK_APPROVE_PATH, BULK_MAX, bulkItems, bulkSummary, classGroups, isSafeTier, keepSelected,
  postBulkApprove,
} from './bulkApprove'

// ux-07b: the bulk approve helpers. The server decides each item on its
// own; these only build the request, group same-class safe items and keep
// the refused ones selected.
// No concurrent access tests: pure functions, and one fetch per call.

afterEach(() => { vi.unstubAllGlobals() })

describe('isSafeTier', () => {
  it('is true only for the safe tier', () => {
    expect(isSafeTier('safe')).toBe(true)
    expect(isSafeTier(' SAFE ')).toBe(true)
    for (const tier of ['moderate', 'high', 'high_risk', 'unknown', '', null, undefined, 3]) {
      expect(isSafeTier(tier), String(tier)).toBe(false)
    }
  })
})

describe('classGroups', () => {
  const entry = (key, cls, database, safe = true) => ({ key, cls, database, safe })

  it('groups two or more safe items of one class on one database', () => {
    const groups = classGroups([
      entry('orders:1', 'analyze_table', 'orders'),
      entry('orders:2', 'analyze_table', 'orders'),
      entry('billing:3', 'analyze_table', 'billing'),
      entry('orders:4', 'create_index', 'orders', false),
      entry('orders:5', 'create_index', 'orders'),
      entry('orders:6', 'analyze_table', 'orders', false),
    ])
    expect(groups).toEqual([{ cls: 'analyze_table', database: 'orders',
      keys: ['orders:1', 'orders:2'] }])
  })

  it('is empty for no entries or nothing grouped', () => {
    expect(classGroups([])).toEqual([])
    expect(classGroups(undefined)).toEqual([])
    expect(classGroups([entry('a:1', '', 'a'), entry('a:2', '', 'a')])).toEqual([])
  })

  it('sorts groups by database, then class', () => {
    const groups = classGroups([
      entry('z:1', 'vacuum', 'z'), entry('z:2', 'vacuum', 'z'),
      entry('a:1', 'vacuum', 'a'), entry('a:2', 'vacuum', 'a'),
      entry('a:3', 'analyze', 'a'), entry('a:4', 'analyze', 'a'),
    ])
    expect(groups.map(g => `${g.database}/${g.cls}`)).toEqual(['a/analyze', 'a/vacuum', 'z/vacuum'])
  })
})

describe('bulkItems', () => {
  it('sends each card id, database and the hash the person saw', () => {
    expect(bulkItems([{ queue_id: '41', database: 'orders', card_hash: 'h41', sql: 'x' }]))
      .toEqual([{ id: 41, database: 'orders', card_hash: 'h41' }])
    expect(bulkItems([])).toEqual([])
  })
})

describe('keepSelected', () => {
  it('drops what was decided and keeps the refused selected', () => {
    const selected = new Set(['orders:1', 'orders:2', 'orders:3', 'orders:4', 'orders:9'])
    const kept = keepSelected(selected, [
      { id: 1, database: 'orders', status: 'approved' },
      { id: 2, database: 'orders', status: 'refused', reason: 'gate' },
      { id: 3, database: 'orders', status: 'scheduled' },
      { id: 4, database: 'orders', status: 'failed', reason: 'boom' },
    ])
    expect([...kept].sort()).toEqual(['orders:2', 'orders:9'])
    expect(selected.size).toBe(5)
  })

  it('keeps everything when there are no results', () => {
    expect([...keepSelected(new Set(['a:1']), undefined)]).toEqual(['a:1'])
  })
})

describe('bulkSummary', () => {
  it('names each non-zero count', () => {
    expect(bulkSummary({ approved: 2, scheduled: 1, refused: 1, failed: 0 }))
      .toBe('2 approved, 1 scheduled, 1 refused')
    expect(bulkSummary({ approved: 0, scheduled: 0, refused: 3, failed: 1 }))
      .toBe('Nothing ran: 3 refused, 1 failed')
    expect(bulkSummary({})).toBe('Nothing ran')
  })
})

describe('postBulkApprove', () => {
  it('posts the items with the surface header', async () => {
    const fetchMock = vi.fn(async () => ({ ok: true, status: 200,
      json: async () => ({ results: [], approved: 0 }) }))
    vi.stubGlobal('fetch', fetchMock)
    const items = [{ id: 1, database: 'orders', card_hash: 'h1' }]
    await expect(postBulkApprove(items, 'inbox')).resolves.toEqual({ results: [], approved: 0 })
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe(BULK_APPROVE_PATH)
    expect(init.method).toBe('POST')
    expect(init.headers['X-Sage-Surface']).toBe('inbox')
    expect(JSON.parse(init.body)).toEqual({ items })
  })

  it('refuses an empty or oversized list without a request', async () => {
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    await expect(postBulkApprove([], 'page')).rejects.toThrow(/nothing selected/i)
    const many = Array.from({ length: BULK_MAX + 1 },
      (_, i) => ({ id: i + 1, database: 'a', card_hash: 'h' }))
    await expect(postBulkApprove(many, 'page')).rejects.toThrow(/at most 50/)
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('throws the server error message', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: false, status: 400,
      json: async () => ({ error: 'duplicate item 1 on database "a"' }) })))
    await expect(postBulkApprove([{ id: 1, database: 'a', card_hash: 'h' }], 'page'))
      .rejects.toThrow('duplicate item 1 on database "a"')
  })

  it('names the status when the error body is not JSON', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: false, status: 503,
      json: async () => { throw new SyntaxError('bad json') } })))
    await expect(postBulkApprove([{ id: 1, database: 'a', card_hash: 'h' }], 'page'))
      .rejects.toThrow('Bulk approve failed (503)')
  })
})
