// tokens.ts: MCP token and user payloads for agents-tokens-page.spec.ts
// (agentgov-01: the Agents banner and the Principal and Owner columns).
// Each payload names the Go code whose JSON it copies. Register these
// routes AFTER mockAllAPIs: Playwright matches the newest first.

import { type Page } from '@playwright/test'

// NOW is the fixed browser clock of the spec: every expiry below is
// relative to it, so a token reads active or expired the same on any day.
export const NOW = '2026-11-14T12:00:00Z'

const at = (days: number) => new Date(Date.parse(NOW) + days * 86400000).toISOString()

// agentToken copies mcptoken.Token as listTokensHandler serves it for an
// agent principal (TestList*_PrincipalStatus in internal/mcptoken): the
// principal_id and principal_status come from the LEFT JOIN on
// sage.agent_principals, and an agent token has no owner_user_id.
export function agentToken(id: string, name: string, status: string,
  extra: Record<string, unknown> = {}) {
  return {
    id, name, kind: 'agent', scopes: ['read', 'propose'], databases: ['orders'],
    created_by: 'admin@x.test', created_at: at(-3), expires_at: at(27),
    last_used_at: null, prefix: `sage_mcp_${id.slice(0, 3)}`,
    principal_id: `agp_${name}`, principal_status: status, ...extra,
  }
}

// operatorToken is an operator token: it acts for owner_user_id and has
// no principal (the contract fixture testdata/default-create.json).
export function operatorToken(id: string, name: string, owner: number) {
  return {
    id, name, kind: 'operator', scopes: ['read', 'propose', 'approve'], databases: ['*'],
    owner_user_id: owner, created_by: 'admin@x.test', created_at: at(-5),
    expires_at: at(2), last_used_at: null, prefix: `sage_mcp_${id.slice(0, 3)}`,
  }
}

// USERS copies listUsersHandler's {"users": [...]}: the owners select
// lists the operator and admin users only.
export const USERS = [
  { id: 1, email: 'admin@x.test', role: 'admin' },
  { id: 5, email: 'oncall@x.test', role: 'operator' },
  { id: 6, email: 'viewer@x.test', role: 'viewer' },
]

// TOKEN_HEADERS are the column headers TokenTable draws, in order.
export const TOKEN_HEADERS = ['Name', 'Kind', 'Principal', 'Owner', 'Scopes', 'Databases',
  'Status', 'Expires', 'Last used', '']

// routeTokens answers the token list from tokens and records each POST body.
// A POST is answered 201 with the plaintext secret once (createTokenHandler),
// and the new token is listed first on the next read.
export async function routeTokens(page: Page, tokens: Record<string, unknown>[]) {
  const posts: unknown[] = []
  await page.route(url => url.pathname === '/api/v1/mcp/tokens', route => {
    const request = route.request()
    if (request.method() === 'GET') return route.fulfill({ json: { tokens } })
    const body = request.postDataJSON() as Record<string, unknown>
    posts.push(body)
    const created = { ...operatorToken('new-1', String(body.name), 5) }
    tokens.unshift(created)
    return route.fulfill({ status: 201, json: { ...created, token: 'sage_mcp_SECRET' } })
  })
  await page.route(url => url.pathname === '/api/v1/users', route =>
    route.fulfill({ json: { users: USERS } }))
  return posts
}
