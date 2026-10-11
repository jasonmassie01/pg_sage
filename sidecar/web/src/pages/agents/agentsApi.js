// Agents page (spec §8.3, §8.4): REST helpers and pure rules.

export const AGENTS_URL = '/api/v1/agents'
export const ENVIRONMENTS = ['branch', 'dev', 'stage', 'prod']
export const KILL_PHRASE = 'KILL AGENTS'
export const DEFAULT_PROFILE = 'readonly-analyst'
const MAX_TOKEN_DAYS = 90

// errorFrom turns a failed response into an Error with the server's
// message; reason is the blocked verdict's reason_code or the error code,
// fix the statement a person runs, status the HTTP status.
export async function errorFrom(res, what) {
  let body = null
  try {
    body = await res.json()
  } catch (err) {
    if (!(err instanceof SyntaxError)) throw err
  }
  const status = `${res.status} ${res.statusText || ''}`.trim()
  const message = typeof body?.error === 'string' && body.error
    ? body.error : `Could not ${what} (${status})`
  const err = new Error(message)
  err.status = res.status
  err.reason = body?.reason_code || body?.code || ''
  err.fix = body?.fix || ''
  return err
}

async function call(url, what, init) {
  const res = await fetch(url, { credentials: 'include', ...init })
  if (!res.ok) throw await errorFrom(res, what)
  return res.json()
}

function post(url, what, body) {
  return call(url, what, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
}

const agentURL = id => `${AGENTS_URL}/${encodeURIComponent(id)}`

export function listAgents(cursor = '') {
  const q = cursor ? `?cursor=${encodeURIComponent(cursor)}` : ''
  return call(`${AGENTS_URL}${q}`, 'load agents')
}

export const getAgent = id => call(agentURL(id), 'load the agent')
export const createAgent = body => post(AGENTS_URL, 'create the agent', body)
export const mintAgentToken = (id, body) =>
  post(`${agentURL(id)}/tokens`, 'issue the token', body)
export const freezeAgent = (id, reason) =>
  post(`${agentURL(id)}/freeze`, 'freeze the agent', { reason })
export const unfreezeAgent = (id, reason) =>
  post(`${agentURL(id)}/unfreeze`, 'unfreeze the agent', { reason })
export const killAgents = reason =>
  post(`${AGENTS_URL}/kill`, 'kill agents', { scope: 'all', reason })
export const agentActivity = id => call(`${agentURL(id)}/activity`, 'load activity')

export async function listUsers() {
  const data = await call('/api/v1/users', 'load users')
  return Array.isArray(data?.users) ? data.users : []
}

export async function listAgentTokens(id) {
  const data = await call('/api/v1/mcp/tokens', 'load tokens')
  return tokensFor(data?.tokens, id)
}

const dbQuery = database => `?database=${encodeURIComponent(database)}`

export const agentGrants = (id, database) =>
  call(`${agentURL(id)}/grants${dbQuery(database)}`, 'load grants')
export const grantRequests = (id, database) =>
  call(`${agentURL(id)}/grant-requests${dbQuery(database)}`, 'load grant requests')
export const revokeGrant = (id, database, grantID) =>
  post(`${agentURL(id)}/grants/${grantID}/revoke`, 'revoke the grant', { database })
export const decideRequest = (id, database, requestID, verb) =>
  post(`${agentURL(id)}/grant-requests/${requestID}/${verb}`, `${verb} the request`,
    { database })

// tokensFor keeps the MCP tokens bound to the agent principal.
export function tokensFor(tokens, principalID) {
  return Array.isArray(tokens) ? tokens.filter(t => t.principal_id === principalID) : []
}

// sponsorLabel names an agent's accountable sponsor.
export function sponsorLabel(agent, users) {
  if (agent.sponsor_user_id == null) return 'none (unsponsored)'
  if (agent.sponsor_active === false) return `user #${agent.sponsor_user_id} (inactive)`
  const u = (users || []).find(x => x.id === agent.sponsor_user_id)
  return u ? u.email : `user #${agent.sponsor_user_id}`
}

// createBody builds POST /api/v1/agents from the form, or null.
export function createBody(form) {
  const name = form.name.trim()
  const sponsor = Number(form.sponsor)
  const profile = form.profile.trim()
  if (!name || !Number.isInteger(sponsor) || sponsor <= 0 || !profile) return null
  if (!ENVIRONMENTS.includes(form.ceiling)) return null
  const body = { name, sponsor_user_id: sponsor, profile, env_ceiling: form.ceiling }
  if (form.tenant.trim()) body.tenant = form.tenant.trim()
  return body
}

// tokenBody builds an agent token request: read, plus propose if chosen,
// never approve; no databases listed means every database the agent may use.
export function tokenBody(form) {
  const name = form.name.trim()
  const days = Number(form.days)
  if (!name || form.days === '' || !Number.isInteger(days) || days < 1 ||
    days > MAX_TOKEN_DAYS) {
    return null
  }
  const dbs = form.databases.split(',').map(s => s.trim()).filter(Boolean)
  return { name, scopes: form.propose ? ['read', 'propose'] : ['read'],
    databases: dbs.length ? dbs : ['*'], expires_in_days: days }
}

// unfreezeMessage explains an unfreeze result.
export function unfreezeMessage(r) {
  if (r.pending) {
    return `Waiting for a second admin (request #${r.request_id}, started by ` +
      `${r.requested_by}). After a kill, unfreezing needs two admins, and the ` +
      "agent's sponsor cannot be the second."
  }
  const rotated = (r.clusters || []).filter(c => c.rotated).length
  let msg = 'Unfrozen.'
  if (rotated) msg += ` Broker credentials rotated on ${rotated} cluster(s).`
  if (r.single_operator) msg += ' Applied by one admin in single-operator mode.'
  return msg
}

export function killReady(phrase, reason) {
  return phrase === KILL_PHRASE && reason.trim() !== ''
}
