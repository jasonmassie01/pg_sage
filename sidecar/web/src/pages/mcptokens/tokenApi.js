// MCP v2: REST helpers and pure rules for the admin MCP tokens page.

export const TOKENS_URL = '/api/v1/mcp/tokens'
export const SCOPES = ['read', 'propose', 'approve']
export const MIN_DAYS = 1
export const MAX_DAYS = 90

// errorFrom turns a failed response into an Error carrying the server's
// {"error": "..."} message, or the HTTP status when the body is not JSON.
async function errorFrom(res, what) {
  let message = ''
  try {
    const body = await res.json()
    message = typeof body?.error === 'string' ? body.error : ''
  } catch (err) {
    if (!(err instanceof SyntaxError)) throw err
  }
  const status = `${res.status} ${res.statusText || ''}`.trim()
  return new Error(message || `Could not ${what} (${status})`)
}

export async function listTokens() {
  const res = await fetch(TOKENS_URL, { credentials: 'include' })
  if (!res.ok) throw await errorFrom(res, 'load MCP tokens')
  const data = await res.json()
  return Array.isArray(data?.tokens) ? data.tokens : []
}

export async function createToken(body) {
  const res = await fetch(TOKENS_URL, {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  if (!res.ok) throw await errorFrom(res, 'create the token')
  return res.json()
}

export async function revokeToken(id) {
  const res = await fetch(`${TOKENS_URL}/${encodeURIComponent(id)}`, {
    method: 'DELETE',
    credentials: 'include',
  })
  if (!res.ok) throw await errorFrom(res, 'revoke the token')
  return res.json()
}

// listOwners returns the users who may own an operator token.
export async function listOwners() {
  const res = await fetch('/api/v1/users', { credentials: 'include' })
  if (!res.ok) throw await errorFrom(res, 'load users')
  const data = await res.json()
  const users = Array.isArray(data?.users) ? data.users : []
  return users.filter(u => u.role === 'operator' || u.role === 'admin')
}

export function tokenStatus(token, now = Date.now()) {
  if (token.revoked_at) return 'revoked'
  if (new Date(token.expires_at).getTime() <= now) return 'expired'
  return 'active'
}

export function parseDatabaseNames(text) {
  return text.split(',').map(s => s.trim()).filter(Boolean)
}

export function validDays(text) {
  const n = Number(text)
  return text !== '' && Number.isInteger(n) && n >= MIN_DAYS && n <= MAX_DAYS
}

export function setupCommand(origin, secret) {
  return `claude mcp add --transport http pg_sage ${origin}/api/v1/mcp`
    + ` --header "Authorization: Bearer ${secret}"`
}

// requestBody builds the POST body from the form, or returns null when the
// form is not complete. An agent token never carries approve or an owner.
export function requestBody(form) {
  const name = form.name.trim()
  const scopes = SCOPES.filter(s => form.scopes.includes(s))
  const databases = form.allDatabases ? ['*'] : parseDatabaseNames(form.databaseNames)
  if (!name || scopes.length === 0 || databases.length === 0) return null
  if (!validDays(form.expires)) return null
  const body = { name, kind: form.kind, scopes, databases,
    expires_in_days: Number(form.expires) }
  if (form.kind === 'operator') {
    if (!form.owner) return null
    body.owner_user_id = Number(form.owner)
  }
  return body
}
