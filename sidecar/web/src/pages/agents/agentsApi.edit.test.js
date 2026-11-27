import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  editBody, freezeItems, killBody, killReady, listFreezes, patchAgent, releaseBody,
  releaseFreeze, releaseMessage, retireReady, revokeAgentToken, sponsorOptions,
} from './agentsApi'
import { coder, frozenBot, response, users } from './testStub'

// agentgov-07: the pure rules of edit, retire, scoped kill and release,
// and the REST helpers they call.

afterEach(() => {
  vi.unstubAllGlobals()
})

const unchanged = a => ({ sponsor: a.sponsor_user_id == null ? '' : String(a.sponsor_user_id),
  profile: a.profile, ceiling: a.env_ceiling, reason: '' })

describe('editBody', () => {
  it('is null while nothing changed', () => {
    expect(editBody(coder, unchanged(coder))).toBeNull()
    expect(editBody(coder, { ...unchanged(coder), reason: 'just because' })).toBeNull()
  })

  it('sends only the changed fields, with the reason', () => {
    expect(editBody(coder, { ...unchanged(coder), ceiling: 'prod' }))
      .toEqual({ env_ceiling: 'prod' })
    expect(editBody(coder, { ...unchanged(coder), sponsor: '1', profile: 'coding-agent',
      reason: ' new owner ' }))
      .toEqual({ sponsor_user_id: 1, profile: 'coding-agent', reason: 'new owner' })
  })

  it('assigns a sponsor to an unsponsored legacy agent', () => {
    expect(editBody(frozenBot, unchanged(frozenBot))).toBeNull()
    expect(editBody(frozenBot, { ...unchanged(frozenBot), sponsor: '5' }))
      .toEqual({ sponsor_user_id: 5 })
  })

  it('refuses a malformed sponsor, ceiling or profile', () => {
    for (const bad of [{ sponsor: 'x' }, { sponsor: '0' }, { sponsor: '-3' },
      { sponsor: '1.5' }, { ceiling: 'qa' }, { profile: '  ' }]) {
      expect(editBody(coder, { ...unchanged(coder), ...bad })).toBeNull()
    }
  })

  it('never clears a sponsor: an empty choice is no change', () => {
    expect(editBody(coder, { ...unchanged(coder), sponsor: '' })).toBeNull()
  })

  it('tolerates a missing agent or form', () => {
    expect(editBody(null, unchanged(coder))).toBeNull()
    expect(editBody(coder, null)).toBeNull()
  })
})

describe('sponsorOptions', () => {
  it('offers operators and admins, never viewers', () => {
    expect(sponsorOptions(users).map(u => u.id)).toEqual([1, 5])
    expect(sponsorOptions(null)).toEqual([])
  })
})

describe('retireReady', () => {
  it('needs the exact name and a reason', () => {
    expect(retireReady(coder, 'coder', 'decommissioned')).toBe(true)
    expect(retireReady(coder, 'coder ', 'decommissioned')).toBe(false)
    expect(retireReady(coder, 'Coder', 'decommissioned')).toBe(false)
    expect(retireReady(coder, 'coder', '   ')).toBe(false)
    expect(retireReady(null, '', 'x')).toBe(false)
    expect(retireReady({ ...coder, status: 'retired' }, 'coder', 'x')).toBe(false)
  })
})

describe('scoped kill', () => {
  it('needs the phrase, a reason and, for a scope, its target', () => {
    expect(killReady('KILL AGENTS', 'r')).toBe(true)
    expect(killReady('KILL AGENTS', 'r', 'all', '')).toBe(true)
    expect(killReady('KILL AGENTS', 'r', 'database', '')).toBe(false)
    expect(killReady('KILL AGENTS', 'r', 'database', 'orders')).toBe(true)
    expect(killReady('KILL AGENTS', 'r', 'principal', '')).toBe(false)
    expect(killReady('kill agents', 'r', 'principal', coder.id)).toBe(false)
    expect(killReady('KILL AGENTS', '', 'principal', coder.id)).toBe(false)
    expect(killReady('KILL AGENTS', 'r', 'cluster', 'x')).toBe(false)
  })

  it('builds the body the API takes for each scope', () => {
    expect(killBody({ scope: 'all', target: 'ignored', reason: ' runaway ' }))
      .toEqual({ scope: 'all', reason: 'runaway' })
    expect(killBody({ scope: 'principal', target: coder.id, reason: 'r' }))
      .toEqual({ scope: 'principal', id: coder.id, reason: 'r' })
    expect(killBody({ scope: 'database', target: 'orders', reason: 'r' }))
      .toEqual({ scope: 'database', id: 'orders', reason: 'r' })
  })
})

describe('release', () => {
  it('maps a freeze to the release body', () => {
    expect(releaseBody({ scope: 'fleet', target: '' }, ' closed '))
      .toEqual({ scope: 'all', reason: 'closed' })
    expect(releaseBody({ scope: 'database', target: 'orders' }, 'closed'))
      .toEqual({ scope: 'database', database: 'orders', reason: 'closed' })
    expect(releaseBody({ scope: 'principal', target: coder.id }, 'closed')).toBeNull()
    expect(releaseBody({ scope: 'fleet', target: '' }, '  ')).toBeNull()
  })

  it('explains a pending and an applied release', () => {
    expect(releaseMessage({ pending: true, request_id: 4, requested_by: 'a@x.test' }))
      .toMatch(/second admin.*#4.*a@x\.test/s)
    expect(releaseMessage({ applied: true })).toMatch(/^Released/)
    expect(releaseMessage({ applied: true, single_operator: true }))
      .toMatch(/single-operator/)
  })

  it('keeps fleet and database freezes, fleet first, and drops malformed rows', () => {
    const items = freezeItems({ freezes: [
      { scope: 'principal', target: coder.id, reason: 'r' },
      { scope: 'database', target: 'orders', reason: 'r' },
      null, { scope: 'fleet', target: '', reason: 'r' }, { target: 'x' },
    ] })
    expect(items.map(f => f.scope)).toEqual(['fleet', 'database'])
    expect(freezeItems(null)).toEqual([])
    expect(freezeItems({ freezes: 'nope' })).toEqual([])
  })
})

describe('REST helpers', () => {
  it('patches, lists, releases and revokes on the right routes', async () => {
    const mock = vi.fn(async () => response(200, { freezes: [], truncated: false }))
    vi.stubGlobal('fetch', mock)
    await patchAgent(coder.id, { env_ceiling: 'prod' })
    await listFreezes()
    await releaseFreeze({ scope: 'all', reason: 'closed' })
    await revokeAgentToken('tok 1')
    const calls = mock.mock.calls.map(([url, init]) => [init?.method || 'GET', url,
      init?.body])
    expect(calls).toEqual([
      ['PATCH', `/api/v1/agents/${coder.id}`, '{"env_ceiling":"prod"}'],
      ['GET', '/api/v1/agents/freezes', undefined],
      ['POST', '/api/v1/agents/kill/release', '{"scope":"all","reason":"closed"}'],
      ['DELETE', '/api/v1/mcp/tokens/tok%201', undefined],
    ])
  })

  it('carries the server code and fix of a refusal', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => response(409, { code: 'sponsor_change_frozen',
      error: 'the agent has an open freeze' })))
    await expect(patchAgent(coder.id, { sponsor_user_id: 1 })).rejects.toMatchObject({
      status: 409, reason: 'sponsor_change_frozen', message: 'the agent has an open freeze' })
  })
})
