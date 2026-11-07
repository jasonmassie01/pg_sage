import { describe, expect, it } from 'vitest'
import { DEFAULT_FORM, requestBody, tokenStatus } from './tokenApi'

// Pure rules of the MCP tokens page (agentgov-01). No concurrency tests:
// these functions are pure and share no state.

const NOW = Date.parse('2026-11-06T12:00:00Z')
const future = new Date(NOW + 86400000).toISOString()
const past = new Date(NOW - 1).toISOString()

describe('tokenStatus', () => {
  it.each([
    ['active operator token', { expires_at: future }, 'active'],
    ['active agent', { expires_at: future, principal_status: 'active' }, 'active'],
    ['retired agent', { expires_at: future, principal_status: 'retired' }, 'agent retired'],
    ['frozen agent', { expires_at: future, principal_status: 'frozen' }, 'agent frozen'],
    ['empty principal status', { expires_at: future, principal_status: '' }, 'active'],
    ['revoked beats retired',
      { expires_at: future, revoked_at: past, principal_status: 'retired' }, 'revoked'],
    ['expired beats frozen', { expires_at: past, principal_status: 'frozen' }, 'expired'],
    ['expiring exactly now is expired', { expires_at: new Date(NOW).toISOString() },
      'expired'],
  ])('%s', (_label, token, want) => {
    expect(tokenStatus(token, NOW)).toBe(want)
  })
})

describe('requestBody', () => {
  const filled = { ...DEFAULT_FORM, name: 'oncall', owner: '5' }

  it('defaults to an operator token with the owner as a number', () => {
    expect(requestBody(filled)).toEqual({ name: 'oncall', kind: 'operator',
      scopes: ['read'], databases: ['*'], expires_in_days: 30, owner_user_id: 5 })
  })

  it('never builds an agent body, whatever the form carries', () => {
    expect(requestBody({ ...filled, kind: 'agent' }).kind).toBe('operator')
  })

  it.each([
    ['no owner', { owner: '' }],
    ['owner zero', { owner: '0' }],
    ['negative owner', { owner: '-2' }],
    ['non-numeric owner', { owner: 'abc' }],
    ['fractional owner', { owner: '1.5' }],
    ['blank name', { name: '   ' }],
    ['no scopes', { scopes: [] }],
    ['no databases', { allDatabases: false, databaseNames: ' , ' }],
    ['expiry above range', { expires: '91' }],
  ])('returns null for %s', (_label, patch) => {
    expect(requestBody({ ...filled, ...patch })).toBeNull()
  })

  it('keeps approve for an operator token, in canonical scope order', () => {
    expect(requestBody({ ...filled, scopes: ['approve', 'read'] }).scopes)
      .toEqual(['read', 'approve'])
  })
})
