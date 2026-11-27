// MCP v2: REST helpers and pure rules for the admin MCP tokens page.

export const TOKENS_URL = '/api/v1/mcp/tokens'
export const SCOPES = ['read', 'propose', 'approve']
export const MIN_DAYS = 1
export const MAX_DAYS = 90

// DEFAULT_FORM is the create form before the admin types anything. This
// page issues operator tokens only (a person's own token, bound to an
// operator or admin owner); a coding agent's token is minted per agent on
// the Agents page, and the server refuses kind "agent" here (agentgov-01).
export const DEFAULT_FORM = Object.freeze({
  name: '', scopes: ['read'], allDatabases: true, databaseNames: '',
  expires: '30', owner: '',
})

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

// PRINCIPAL_STATUS names an agent token whose principal is not active: a
// retired agent's token is refused, a frozen agent's gets agent_frozen.
const PRINCIPAL_STATUS = { retired: 'agent retired', frozen: 'agent frozen' }

export function tokenStatus(token, now = Date.now()) {
  if (token.revoked_at) return 'revoked'
  if (new Date(token.expires_at).getTime() <= now) return 'expired'
  return PRINCIPAL_STATUS[token.principal_status] || 'active'
}

// revocable: a token that still exists for its holder, whatever its agent's
// state; revoking a retired or frozen agent's token is a safe cleanup.
export function revocable(status) {
  return status !== 'revoked' && status !== 'expired'
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

function ownerID(text) {
  const n = Number(text)
  return text !== '' && Number.isInteger(n) && n > 0 ? n : null
}

// requestBody builds the POST body of an operator token from the form, or
// returns null when the form is not complete. The owner is required.
export function requestBody(form) {
  const name = form.name.trim()
  const scopes = SCOPES.filter(s => form.scopes.includes(s))
  const databases = form.allDatabases ? ['*'] : parseDatabaseNames(form.databaseNames)
  const owner = ownerID(form.owner)
  if (!name || scopes.length === 0 || databases.length === 0) return null
  if (!validDays(form.expires) || owner === null) return null
  return { name, kind: 'operator', scopes, databases,
    expires_in_days: Number(form.expires), owner_user_id: owner }
}
