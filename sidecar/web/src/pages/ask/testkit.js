// testkit.js — shared fixtures and a fetch stub for the Ask Sage page tests.
// Only imported by *.test.jsx files; never by application code.

import { vi } from 'vitest'

export const operator = { role: 'operator', email: 'bob@example.com' }
export const viewer = { role: 'viewer', email: 'vera@example.com' }
export const admin = { role: 'admin', email: 'ada@example.com' }

export const BASE = '/api/v1/databases/orders/ask'

export function response(status, body) {
  return {
    ok: status >= 200 && status < 300, status, statusText: `status ${status}`,
    json: async () => body,
  }
}

export const budgetBody = (over = {}) => ({
  day: '2026-10-04', database_used: 10, database_limit: 100,
  user_used: 3, user_limit: 20, ...over,
})

// makeAnswer builds an Answer with a citation of every linkable shape the
// tests need. The server `text` is a sentinel the UI must not render.
export function makeAnswer(over = {}) {
  return {
    id: 41, conversation_id: 'c-1', database: 'orders',
    question: 'Why is orders slow?', status: 'answered',
    text: 'SERVER-RENDERED-TEXT',
    statements: [
      { text: 'Sequential scans on public.orders dominate.',
        citations: ['finding:17', 'queries:abc'] },
      { text: 'An index on customer_id was proposed.', citations: ['proposal:9'] },
    ],
    not_verified: ['Whether the application retries failed requests'],
    dropped: [
      { text: 'Autovacuum is disabled', reason: 'no citation' },
      { text: 'The disk is full', reason: 'cited evidence does not mention it' },
    ],
    citations: [
      { id: 'finding:17', kind: 'finding', ref: '17', label: 'Finding #17: seq scans',
        digest: 'a1b2c3d4e5f6a7b8c9d0', api_path: '/api/v1/findings/17' },
      { id: 'queries:abc', kind: 'queries', ref: 'abc', label: 'Top queries by time',
        digest: '0123456789abcdef', api_path: '/api/v1/queries' },
      { id: 'proposal:9', kind: 'proposal', ref: '9', label: 'Proposal #9',
        digest: 'ffeeddccbbaa99887766', api_path: '/api/v1/actions/9' },
    ],
    actions: [],
    stop: 'end_turn', tokens: 812, created_at: '2026-10-04T10:00:00Z',
    ...over,
  }
}

export const proposalAction = (over = {}) => ({
  kind: 'proposal', id: 9, status: 'queued', verdict: 'needs_approval', reason: '',
  sql: 'CREATE INDEX CONCURRENTLY idx_orders_customer ON public.orders (customer_id)',
  rollback_sql: 'DROP INDEX CONCURRENTLY idx_orders_customer',
  rollback_class: 'reversible', prediction: null, evidence_id: 'proposal:9', ...over,
})

export const investigationAction = (over = {}) => ({
  kind: 'investigation', id: 'inv-5', status: 'opened', verdict: '', reason: '',
  sql: '', rollback_sql: '', rollback_class: '', prediction: null,
  evidence_id: 'investigation:inv-5', ...over,
})

function route(url, method) {
  const path = String(url).split('?')[0]
  const m = path.match(/^\/api\/v1\/databases\/[^/]+\/ask(\/.*)?$/)
  if (!m) return null
  const rest = m[1] || ''
  if (method === 'POST' && rest === '') return 'ask'
  if (method !== 'GET') return null
  if (rest === '/budget') return 'budget'
  if (rest === '/conversations') return 'conversations'
  if (rest.startsWith('/conversations/')) return 'conversation'
  return null
}

// stubAsk installs a fetch stub. Each handler receives (url, init) and
// returns a response (or a promise of one, or throws to simulate a network
// failure). Unhandled routes answer 404.
export function stubAsk(handlers = {}) {
  const defaults = {
    ask: () => response(200, makeAnswer()),
    budget: () => response(200, budgetBody()),
    conversations: () => response(200, { conversations: [] }),
    conversation: () => response(404, { error: 'not found', code: 'not_found' }),
  }
  const h = { ...defaults, ...handlers }
  const mock = vi.fn(async (url, init = {}) => {
    const which = route(url, init.method || 'GET')
    if (!which) return response(404, { error: 'no route', code: 'not_found' })
    return h[which](String(url), init)
  })
  vi.stubGlobal('fetch', mock)
  return mock
}

export const posts = mock => mock.mock.calls
  .filter(([, init]) => init?.method === 'POST')
export const postBodies = mock => posts(mock).map(([, init]) => JSON.parse(init.body))
export const getsTo = (mock, path) => mock.mock.calls
  .filter(([url, init]) => (!init?.method || init.method === 'GET')
    && String(url).split('?')[0] === path)
