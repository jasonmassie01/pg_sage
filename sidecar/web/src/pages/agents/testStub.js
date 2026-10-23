// Fixtures and a routed fetch stub for the Agents page tests. Routes are
// "METHOD /path" keys (the path without its query); a handler gets the
// parsed URL, the parsed JSON body and the call, and returns a response.
import { vi } from 'vitest'

export function iso(minutesFromNow) {
  return new Date(Date.now() + minutesFromNow * 60000).toISOString()
}

export function response(status, body) {
  return {
    ok: status >= 200 && status < 300, status, statusText: `status ${status}`,
    json: async () => body,
  }
}

export const admin = { id: 1, email: 'admin@x.test', role: 'admin' }
export const operator = { id: 5, email: 'oncall@x.test', role: 'operator' }

export const users = [
  { id: 1, email: 'admin@x.test', role: 'admin' },
  { id: 5, email: 'oncall@x.test', role: 'operator' },
  { id: 6, email: 'viewer@x.test', role: 'viewer' },
]

export const coder = {
  id: 'agp_aaaaaaaaaaaaaaaaaaaa', name: 'coder', sponsor_user_id: 5, tenant: '',
  profile: 'readonly-analyst', env_ceiling: 'stage', status: 'active',
  created_by: 'admin@x.test', created_at: iso(-600), updated_at: iso(-60),
  sponsor_active: true, tainted: false,
}

export const frozenBot = {
  id: 'agp_bbbbbbbbbbbbbbbbbbbb', name: 'etl-bot', sponsor_user_id: null, tenant: 'data',
  profile: 'legacy', env_ceiling: 'dev', status: 'frozen',
  frozen_reason: 'killed: fleet incident', created_by: 'migration',
  created_at: iso(-6000), updated_at: iso(-5), sponsor_active: false, tainted: true,
}

export const agentToken = {
  id: 'tok-1', name: 'coder-laptop', kind: 'agent', scopes: ['read'],
  databases: ['orders'], principal_id: coder.id, created_by: 'admin@x.test',
  created_at: iso(-100), expires_at: iso(60 * 24 * 20), prefix: 'sage_mcp_Cod',
}

export const otherToken = {
  id: 'tok-2', name: 'someone-else', kind: 'agent', scopes: ['read'],
  databases: ['*'], principal_id: frozenBot.id, created_by: 'admin@x.test',
  created_at: iso(-100), expires_at: iso(60 * 24 * 20), prefix: 'sage_mcp_Oth',
}

export const activity = {
  principal_id: coder.id,
  items: [{
    database: 'orders', roles: ['sage_agentb_k2m4q7x9ab'],
    sessions: [{ pid: 4242, role: 'sage_agentb_k2m4q7x9ab',
      application_name: 'pg_sage agent:coder', state: 'active', query_start: iso(-1) }],
    statements: [{ query_id: '77', role: 'sage_agentb_k2m4q7x9ab',
      query: 'SELECT id FROM orders WHERE id = $1', calls: 12, rows: 12,
      total_exec_ms: 3.5 }],
    queries: [{ id: 9, at: iso(-2), verdict: 'blocked', reason: 'agent_classification',
      step: 'D5', classes: ['pii'], envelope_hash: 'abc' }],
    attribution: { source: 'pg_stat_statements', complete: true, dropped: false,
      dealloc: 0, audited_executions: 1, attributed_calls: 12 },
  }],
}

export const grant = {
  id: 31, database_id: 'db-1', principal_id: coder.id, lane: 'broker',
  capability: 'read', object_kind: 'relation', object: 'public.orders',
  columns: ['id', 'total'], privileges: ['SELECT'], grantor: 'pg_sage',
  granted_at: iso(-30), expires_at: iso(90), state: 'active', grant_action_id: 4,
}

export const pendingRequest = {
  id: 12, database_id: 'db-1', principal_id: coder.id, capability: 'read',
  objects: [{ object: 'public.customers', columns: ['email'] }], duration_minutes: 60,
  reason: 'debug a failed checkout', status: 'pending', requested_at: iso(-3),
  expires_at: iso(60), grant_ids: [],
}

export const killReport = {
  kill_id: 3, scope: 'all', reason: 'runaway agent', principals: [coder.id],
  tokens_revoked: 2, approvals_cancelled: 1, verified: false,
  databases: [
    { name: 'orders', roles_disabled: 2, backends_terminated: 1, statements_cancelled: 1,
      approvals_cancelled: 1, verified: true,
      replicas: [{ name: 'replica1', configured: true, backends_terminated: 1,
        logins_blocked: true, verified: true }] },
    { name: 'billing', roles_disabled: 0, backends_terminated: 0, statements_cancelled: 0,
      approvals_cancelled: 0, verified: false, error: 'connection refused',
      replicas: [{ name: 'standby-east', configured: false, backends_terminated: 0,
        logins_blocked: false, verified: false,
        note: 'unconfigured standby: sessions end within statement_timeout' }] },
  ],
  started_at: iso(-1), finished_at: iso(0),
}

// stubAPI installs a routed fetch mock. routes overrides or adds handlers;
// an unrouted call answers 404 so a test sees the unexpected request.
export function stubAPI(routes = {}) {
  const defaults = {
    'GET /api/v1/agents': () => response(200, { items: [coder, frozenBot],
      next_cursor: '' }),
    [`GET /api/v1/agents/${coder.id}`]: () => response(200, coder),
    [`GET /api/v1/agents/${frozenBot.id}`]: () => response(200, frozenBot),
    'GET /api/v1/users': () => response(200, { users }),
    'GET /api/v1/mcp/tokens': () => response(200, { tokens: [agentToken, otherToken] }),
    [`GET /api/v1/agents/${coder.id}/activity`]: () => response(200, activity),
    [`GET /api/v1/agents/${frozenBot.id}/activity`]: () => response(200,
      { principal_id: frozenBot.id, items: [] }),
    [`GET /api/v1/agents/${coder.id}/grants`]: () => response(200,
      { items: [grant], next_cursor: '' }),
    [`GET /api/v1/agents/${coder.id}/grant-requests`]: () => response(200,
      { items: [pendingRequest], next_cursor: '' }),
    [`GET /api/v1/agents/${frozenBot.id}/grants`]: () => response(200,
      { items: [], next_cursor: '' }),
    [`GET /api/v1/agents/${frozenBot.id}/grant-requests`]: () => response(200,
      { items: [], next_cursor: '' }),
  }
  const table = { ...defaults, ...routes }
  const mock = vi.fn(async (url, init = {}) => {
    const parsed = new URL(String(url), 'http://sage.test')
    const method = (init.method || 'GET').toUpperCase()
    const handler = table[`${method} ${parsed.pathname}`]
    if (!handler) return response(404, { error: `unexpected ${method} ${parsed.pathname}` })
    const body = init.body ? JSON.parse(init.body) : undefined
    return handler({ url: parsed, body, init })
  })
  vi.stubGlobal('fetch', mock)
  return mock
}

// callsTo lists the calls of method to path (exact path, query ignored).
export const callsTo = (mock, method, path) => mock.mock.calls.filter(([url, init]) =>
  (init?.method || 'GET').toUpperCase() === method
  && new URL(String(url), 'http://sage.test').pathname === path)

export const bodiesOf = (mock, method, path) =>
  callsTo(mock, method, path).map(([, init]) => JSON.parse(init.body))
