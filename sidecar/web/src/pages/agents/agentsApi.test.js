import { describe, expect, it } from 'vitest'
import {
  KILL_PHRASE, createBody, errorFrom, killReady, sponsorLabel, tokenBody, tokensFor,
  unfreezeMessage,
} from './agentsApi'
import { agentToken, coder, frozenBot, otherToken, response, users } from './testStub'

// Pure rules of the Agents page: what the create and token forms send,
// how unfreeze and errors read, and the kill switch's typed phrase.

describe('createBody', () => {
  const form = { name: ' coder ', sponsor: '5', profile: 'readonly-analyst',
    ceiling: 'dev', tenant: '' }

  it('builds the body with a numeric sponsor and trims the name', () => {
    expect(createBody(form)).toEqual({ name: 'coder', sponsor_user_id: 5,
      profile: 'readonly-analyst', env_ceiling: 'dev' })
  })

  it('sends the tenant only when set', () => {
    expect(createBody({ ...form, tenant: ' data ' }).tenant).toBe('data')
  })

  it('refuses a form without a name, sponsor or profile', () => {
    expect(createBody({ ...form, name: '  ' })).toBeNull()
    expect(createBody({ ...form, sponsor: '' })).toBeNull()
    expect(createBody({ ...form, sponsor: '0' })).toBeNull()
    expect(createBody({ ...form, profile: '' })).toBeNull()
  })

  it('refuses a ceiling that is not an environment', () => {
    expect(createBody({ ...form, ceiling: 'production' })).toBeNull()
    for (const env of ['branch', 'dev', 'stage', 'prod']) {
      expect(createBody({ ...form, ceiling: env }).env_ceiling).toBe(env)
    }
  })
})

describe('tokenBody', () => {
  const form = { name: 'laptop', propose: false, databases: '', days: '30' }

  it('defaults to a read token for every database the agent may use', () => {
    expect(tokenBody(form)).toEqual({ name: 'laptop', scopes: ['read'],
      databases: ['*'], expires_in_days: 30 })
  })

  it('adds propose and lists databases', () => {
    expect(tokenBody({ ...form, propose: true, databases: 'orders, billing,' }))
      .toEqual({ name: 'laptop', scopes: ['read', 'propose'],
        databases: ['orders', 'billing'], expires_in_days: 30 })
  })

  it('never offers approve, and refuses bad days or no name', () => {
    expect(tokenBody(form).scopes).not.toContain('approve')
    for (const days of ['0', '91', '', '2.5', 'x']) {
      expect(tokenBody({ ...form, days })).toBeNull()
    }
    expect(tokenBody({ ...form, days: '1' }).expires_in_days).toBe(1)
    expect(tokenBody({ ...form, days: '90' }).expires_in_days).toBe(90)
    expect(tokenBody({ ...form, name: ' ' })).toBeNull()
  })
})

describe('tokensFor', () => {
  it('keeps only the agent principal tokens', () => {
    expect(tokensFor([agentToken, otherToken], coder.id)).toEqual([agentToken])
    expect(tokensFor([], coder.id)).toEqual([])
    expect(tokensFor(undefined, coder.id)).toEqual([])
  })
})

describe('sponsorLabel', () => {
  it('names the sponsor, or says the agent is unsponsored', () => {
    expect(sponsorLabel(coder, users)).toBe('oncall@x.test')
    expect(sponsorLabel(frozenBot, users)).toMatch(/unsponsored/i)
    expect(sponsorLabel({ ...coder, sponsor_user_id: 42 }, users)).toBe('user #42')
    expect(sponsorLabel({ ...coder, sponsor_active: false }, users)).toMatch(/inactive/i)
  })
})

describe('unfreezeMessage', () => {
  it('says a second admin is needed while pending', () => {
    const msg = unfreezeMessage({ applied: false, pending: true, request_id: 7,
      requested_by: 'admin@x.test', quorum: 2 })
    expect(msg).toMatch(/second admin/i)
    expect(msg).toContain('admin@x.test')
    expect(msg).toContain('#7')
    expect(msg).toMatch(/sponsor/i)
  })

  it('reports an applied unfreeze and the credential rotation', () => {
    const msg = unfreezeMessage({ applied: true, pending: false, quorum: 2,
      clusters: [{ cluster_key: 'k', action_id: 3, rotated: true }] })
    expect(msg).toMatch(/unfrozen/i)
    expect(msg).toMatch(/rotated/i)
    expect(unfreezeMessage({ applied: true, quorum: 1 })).toMatch(/unfrozen/i)
  })

  it('names single-operator mode when it applied one admin', () => {
    expect(unfreezeMessage({ applied: true, quorum: 1, single_operator: true }))
      .toMatch(/single-operator/i)
  })
})

describe('killReady', () => {
  it('needs the exact phrase and a reason', () => {
    expect(KILL_PHRASE).toBe('KILL AGENTS')
    expect(killReady('KILL AGENTS', 'runaway')).toBe(true)
    expect(killReady('kill agents', 'runaway')).toBe(false)
    expect(killReady('KILL AGENTS ', 'runaway')).toBe(false)
    expect(killReady('KILL AGENTS', '   ')).toBe(false)
    expect(killReady('', '')).toBe(false)
  })
})

describe('errorFrom', () => {
  it('carries a blocked verdict with its reason and fix', async () => {
    const err = await errorFrom(response(409, { verdict: 'blocked',
      reason_code: 'grantor_lacks_privilege', error: 'pg_sage lacks GRANT OPTION',
      fix: 'GRANT SELECT ON t TO sage WITH GRANT OPTION' }), 'approve')
    expect(err.message).toContain('pg_sage lacks GRANT OPTION')
    expect(err.reason).toBe('grantor_lacks_privilege')
    expect(err.fix).toContain('WITH GRANT OPTION')
    expect(err.status).toBe(409)
  })

  it('uses the code of an error body, else the status', async () => {
    const coded = await errorFrom(response(403, { error: 'the sponsor cannot approve',
      code: 'sponsor_cannot_approve' }), 'unfreeze')
    expect(coded.reason).toBe('sponsor_cannot_approve')
    expect(coded.message).toBe('the sponsor cannot approve')
    const bare = await errorFrom({ ok: false, status: 502, statusText: 'Bad Gateway',
      json: async () => { throw new SyntaxError('not json') } }, 'load agents')
    expect(bare.message).toBe('Could not load agents (502 Bad Gateway)')
    expect(bare.status).toBe(502)
  })
})
