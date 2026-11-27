// users.ts: Users page payloads and a stateful per-database grant route
// for users-databases.spec.ts (apiauth-08 CHECK-05). Register these routes
// AFTER mockAllAPIs: Playwright matches the newest first.

import { type Page } from '@playwright/test'

// ADMIN_ME is meHandler's answer for the signed-in admin (id, email, role,
// must_change_password, password_login).
export const ADMIN_ME = {
  id: 1, email: 'admin@test.com', role: 'admin',
  must_change_password: false, password_login: true,
}

// userRow copies listUsersHandler's userResp (internal/api/auth_handlers.go).
export function userRow(id: number, email: string, role: string) {
  return {
    id, email, role, created_at: '2026-10-01T09:00:00Z', last_login: null,
    sso_linked: false, sso_issuer: '', password_login: true,
    disabled: false, disabled_at: null,
  }
}

export const USERS = [
  userRow(1, 'admin@test.com', 'admin'),
  userRow(2, 'ops@test.com', 'operator'),
  userRow(3, 'reader@test.com', 'viewer'),
]

export type Grant = { database: string, role: string }

// grantJSON copies auth.DatabaseGrant's JSON tags.
function grantJSON(userID: number, g: Grant) {
  return { user_id: userID, database: g.database, role: g.role, granted_by: 1,
    granted_at: '2026-11-01T10:00:00Z' }
}

// MONITORED are the fleet databases mockDatabases lists: the set handler
// refuses any other name (databaseGrantHandlers.change, monitored = true).
const MONITORED = ['primary', 'replica']

// UNKNOWN_DATABASE is sreErrorCode's 400 for a grant on a database pg_sage
// does not monitor.
export const UNKNOWN_DATABASE = {
  status: 400,
  json: { error: 'unknown database: a grant must name a database pg_sage monitors',
    code: 'unknown_database' },
}

export type GrantCall = { method: string, path: string, body: unknown }

// routeUsers serves GET /api/v1/users and the grant routes over grants
// (user id -> grants), which PUT and DELETE change in place. enforced is
// api.database_grants in fleet mode. Every grant call is recorded in calls.
export async function routeUsers(page: Page, grants: Map<number, Grant[]>,
  { enforced = true, calls = [] as GrantCall[] } = {}) {
  await page.route('**/api/v1/auth/me', route => route.fulfill({ json: ADMIN_ME }))
  await page.route('**/api/v1/users**', route => {
    const req = route.request()
    const path = new URL(req.url()).pathname
    if (path === '/api/v1/users' && req.method() === 'GET') {
      return route.fulfill({ json: { users: USERS } })
    }
    const m = path.match(/^\/api\/v1\/users\/(\d+)\/database-grants(?:\/([^/]+))?$/)
    if (!m) return route.fulfill({ status: 404, json: { error: 'not found' } })
    const id = Number(m[1])
    const database = m[2] === undefined ? undefined : decodeURIComponent(m[2])
    const list = grants.get(id) || []
    calls.push({ method: req.method(), path, body: req.postDataJSON() })
    if (req.method() === 'GET') {
      return route.fulfill({ json: { user_id: id, grants: list.map(g => grantJSON(id, g)),
        enforced, scoped: list.length > 0 } })
    }
    if (req.method() === 'PUT') {
      if (!database || !MONITORED.includes(database)) return route.fulfill(UNKNOWN_DATABASE)
      const role = (req.postDataJSON() as { role: string }).role
      const next = list.filter(g => g.database !== database).concat({ database, role })
      next.sort((a, b) => a.database.localeCompare(b.database))
      grants.set(id, next)
      return route.fulfill({ json: grantJSON(id, { database, role }) })
    }
    grants.set(id, list.filter(g => g.database !== database))
    return route.fulfill({ json: { revoked: true, user_id: id, database } })
  })
}
