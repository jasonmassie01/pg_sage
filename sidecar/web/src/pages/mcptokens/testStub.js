// Shared fixtures and a stateful fetch stub for the MCP tokens page tests.
// The stub answers the token and users APIs the way the sidecar does: the
// list never carries the plaintext secret, a created token is listed newest
// first, and a revoked token stays listed with revoked_at/revoked_by set.
import { vi } from 'vitest'

export const TOKENS_URL = '/api/v1/mcp/tokens'
export const USERS_URL = '/api/v1/users'

export function iso(daysFromNow) {
  return new Date(Date.now() + daysFromNow * 86400000).toISOString()
}

export const activeAgent = {
  id: 'a1b2c3d4-0000-4000-8000-000000000001', name: 'claude-code-laptop',
  kind: 'agent', scopes: ['read', 'propose'], databases: ['orders', 'billing'],
  created_by: 'creator@x.test', created_at: iso(-3), expires_at: iso(27),
  last_used_at: iso(-1), prefix: 'sage_mcp_AbC',
}

export const activeOperator = {
  id: 'a1b2c3d4-0000-4000-8000-000000000002', name: 'oncall-cursor',
  kind: 'operator', scopes: ['read', 'propose', 'approve'], databases: ['*'],
  owner_user_id: 5, created_by: 'creator@x.test', created_at: iso(-5),
  expires_at: iso(2), last_used_at: null, prefix: 'sage_mcp_OpR',
}

export const revokedAgent = {
  id: 'a1b2c3d4-0000-4000-8000-000000000003', name: 'old-ci-agent',
  kind: 'agent', scopes: ['read'], databases: ['orders'],
  created_by: 'creator@x.test', created_at: iso(-20), expires_at: iso(10),
  revoked_at: iso(-2), revoked_by: 'secops@x.test', last_used_at: iso(-4),
  prefix: 'sage_mcp_ReV',
}

export const expiredAgent = {
  id: 'a1b2c3d4-0000-4000-8000-000000000004', name: 'trial-agent',
  kind: 'agent', scopes: ['read'], databases: ['*'],
  created_by: 'creator@x.test', created_at: iso(-40), expires_at: iso(-1),
  last_used_at: null, prefix: 'sage_mcp_ExP',
}

export const users = [
  { id: 1, email: 'admin@x.test', role: 'admin' },
  { id: 5, email: 'oncall@x.test', role: 'operator' },
  { id: 6, email: 'viewer@x.test', role: 'viewer' },
]

export const admin = { id: 1, email: 'admin@x.test', role: 'admin' }

export function response(status, body) {
  return {
    ok: status >= 200 && status < 300, status, statusText: `status ${status}`,
    json: async () => body,
  }
}

function defaultDelete(list, id) {
  const tok = list.find(t => t.id === id)
  if (!tok) return response(404, { error: 'token not found' })
  if (tok.revoked_at) return response(400, { error: 'token is already revoked' })
  const revoked = { ...tok, revoked_at: new Date().toISOString(),
    revoked_by: 'admin@x.test' }
  list.splice(list.indexOf(tok), 1, revoked)
  return response(200, revoked)
}

// stubAPI installs a fetch mock. onPost(body) and onDelete(id, list) may
// return a response (or a promise of one) to override the default replies.
export function stubAPI({
  tokens = [], userList = users, listStatus = 200, onPost, onDelete,
} = {}) {
  const list = [...tokens]
  const mock = vi.fn(async (url, init = {}) => {
    const path = String(url).split('?')[0]
    const method = (init.method || 'GET').toUpperCase()
    if (path === USERS_URL && method === 'GET') {
      return response(200, { users: userList })
    }
    if (path === TOKENS_URL && method === 'GET') {
      if (listStatus !== 200) {
        return response(listStatus, { error: 'database unavailable' })
      }
      return response(200, { tokens: list.map(t => ({ ...t })) })
    }
    if (path === TOKENS_URL && method === 'POST') {
      const body = JSON.parse(init.body)
      const reply = onPost ? await onPost(body) : null
      if (reply) {
        if (reply.status === 201) {
          const { token: _secret, ...listed } = await reply.json()
          list.unshift(listed)
          return response(201, { ...listed, token: _secret })
        }
        return reply
      }
      return response(500, { error: 'no POST reply configured' })
    }
    if (path.startsWith(`${TOKENS_URL}/`) && method === 'DELETE') {
      const id = path.slice(TOKENS_URL.length + 1)
      const reply = onDelete ? await onDelete(id, list) : null
      return reply || defaultDelete(list, id)
    }
    return response(404, { error: `unexpected ${method} ${path}` })
  })
  vi.stubGlobal('fetch', mock)
  return mock
}

export const callsTo = (mock, method, path) => mock.mock.calls.filter(([url, init]) =>
  (init?.method || 'GET').toUpperCase() === method
  && (path instanceof RegExp ? path.test(String(url)) : String(url).split('?')[0] === path))

export function stubClipboard() {
  const writeText = vi.fn().mockResolvedValue(undefined)
  Object.defineProperty(window.navigator, 'clipboard', {
    value: { writeText }, configurable: true,
  })
  return writeText
}
