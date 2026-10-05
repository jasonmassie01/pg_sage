import { describe, expect, it } from 'vitest'
import {
  MAX_QUESTION_LENGTH, askURL, budgetURL, citationHref, citationNumbers,
  conversationURL, conversationsURL, describeAskError, isSingleDatabase,
  shortDigest, statusLabel,
} from './ask'

// lib/ask.js holds the pure helpers behind the Ask Sage page: the API URL
// builders, the evidence-kind -> UI route mapping, status labels, digest
// shortening, citation numbering and error classification.
// No concurrency tests: every helper is a pure function without shared state.

describe('URL builders', () => {
  it('builds the ask, conversation and budget URLs for a database', () => {
    expect(askURL('orders')).toBe('/api/v1/databases/orders/ask')
    expect(conversationsURL('orders')).toBe('/api/v1/databases/orders/ask/conversations')
    expect(conversationURL('orders', 'c-1'))
      .toBe('/api/v1/databases/orders/ask/conversations/c-1')
    expect(budgetURL('orders')).toBe('/api/v1/databases/orders/ask/budget')
  })

  it('URL-encodes the database name and the conversation id', () => {
    expect(askURL('my db/x?y')).toBe('/api/v1/databases/my%20db%2Fx%3Fy/ask')
    expect(budgetURL('a&b')).toBe('/api/v1/databases/a%26b/ask/budget')
    expect(conversationURL('a b', '../c/1'))
      .toBe('/api/v1/databases/a%20b/ask/conversations/..%2Fc%2F1')
  })
})

describe('isSingleDatabase', () => {
  it('accepts a named database', () => {
    expect(isSingleDatabase('orders')).toBe(true)
  })

  it.each([['all'], [''], ['   '], [null], [undefined]])(
    'rejects %j (fleet mode or nothing selected)', db => {
      expect(isSingleDatabase(db)).toBe(false)
    })
})

describe('citationHref', () => {
  it.each([
    ['finding', '#/cases'], ['findings', '#/cases'],
    ['investigation', '#/cases'], ['investigations', '#/cases'],
    ['incidents', '#/cases'],
    ['action', '#/actions'], ['actions', '#/actions'],
    ['approvals', '#/actions'], ['proposal', '#/actions'],
    ['trust', '#/trust'], ['facts', '#/facts'],
  ])('links %s evidence to %s', (kind, href) => {
    expect(citationHref(kind)).toBe(href)
  })

  it.each([['config'], ['doc'], ['table'], ['queries'], ['bogus'], [''],
    [null], [undefined], ['__proto__'], ['constructor'], ['javascript:alert(1)']])(
    'has no link for %j', kind => {
      expect(citationHref(kind)).toBeNull()
    })
})

describe('statusLabel', () => {
  it('gives every status a distinct label', () => {
    const labels = {
      answered: 'Answered', not_observed: 'Not observed',
      budget_exhausted: 'Budget exhausted', llm_unavailable: 'LLM unavailable',
      incomplete: 'Incomplete',
    }
    for (const [status, label] of Object.entries(labels)) {
      expect(statusLabel(status)).toBe(label)
    }
    expect(new Set(Object.keys(labels).map(statusLabel)).size).toBe(5)
  })

  it('shows an unknown status as is and a missing one as Unknown', () => {
    expect(statusLabel('rate_limited')).toBe('rate_limited')
    expect(statusLabel('')).toBe('Unknown')
    expect(statusLabel(undefined)).toBe('Unknown')
    expect(statusLabel('toString')).toBe('toString')
  })
})

describe('shortDigest', () => {
  it('keeps the first 12 characters', () => {
    expect(shortDigest('a1b2c3d4e5f6a7b8c9d0')).toBe('a1b2c3d4e5f6')
  })

  it('keeps short digests whole, including exactly 12 characters', () => {
    expect(shortDigest('abc')).toBe('abc')
    expect(shortDigest('0123456789ab')).toBe('0123456789ab')
    expect(shortDigest('0123456789abc')).toBe('0123456789ab')
  })

  it.each([[''], [null], [undefined]])('returns an empty string for %j', d => {
    expect(shortDigest(d)).toBe('')
  })
})

describe('citationNumbers', () => {
  it('numbers citations from 1 in list order', () => {
    expect(citationNumbers([{ id: 'finding:1' }, { id: 'trust:x' }, { id: 'doc:y' }]))
      .toEqual({ 'finding:1': 1, 'trust:x': 2, 'doc:y': 3 })
  })

  it('keeps the first number of a duplicated id', () => {
    expect(citationNumbers([{ id: 'a' }, { id: 'b' }, { id: 'a' }]))
      .toEqual({ a: 1, b: 2 })
  })

  it.each([[null], [undefined], [[]]])('returns an empty map for %j', list => {
    expect(citationNumbers(list)).toEqual({})
  })

  it('does not inherit numbers for prototype keys', () => {
    expect(citationNumbers([{ id: 'a' }]).toString).toBeUndefined()
  })
})

describe('MAX_QUESTION_LENGTH', () => {
  it('is 2000 characters', () => {
    expect(MAX_QUESTION_LENGTH).toBe(2000)
  })
})

describe('describeAskError', () => {
  it('classifies a disabled Ask Sage', () => {
    expect(describeAskError(503, { error: 'ask is disabled', code: 'ask_disabled' }))
      .toEqual({ kind: 'disabled', message: 'Ask Sage is disabled' })
  })

  it('keeps the server message for unavailable, invalid and not found', () => {
    expect(describeAskError(503, { error: 'llm pool exhausted', code: 'ask_unavailable' }))
      .toEqual({ kind: 'unavailable', message: 'llm pool exhausted' })
    expect(describeAskError(400, { error: 'question too long', code: 'invalid_request' }))
      .toEqual({ kind: 'invalid', message: 'question too long' })
    expect(describeAskError(404, { error: 'conversation not found', code: 'not_found' }))
      .toEqual({ kind: 'not_found', message: 'conversation not found' })
  })

  it('reports an expired session on 401', () => {
    const e = describeAskError(401, { error: 'unauthorized' })
    expect(e.kind).toBe('auth')
    expect(e.message).toMatch(/session expired/i)
  })

  it('falls back to the HTTP status when the body has no error', () => {
    const e = describeAskError(500, null)
    expect(e.kind).toBe('http')
    expect(e.message).toContain('500')
    const bare = describeAskError(400, {})
    expect(bare.kind).toBe('invalid')
    expect(bare.message).toContain('400')
  })

  it('treats a 503 without a known code as unavailable', () => {
    const e = describeAskError(503, { error: 'try later' })
    expect(e).toEqual({ kind: 'unavailable', message: 'try later' })
  })
})
