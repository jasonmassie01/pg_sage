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
export const killAgents = body => post(`${AGENTS_URL}/kill`, 'kill agents', body)
export const patchAgent = (id, body) => call(agentURL(id), 'change the agent', {
  method: 'PATCH',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify(body),
})
export const listFreezes = () => call(`${AGENTS_URL}/freezes`, 'load freezes')
export const releaseFreeze = body =>
  post(`${AGENTS_URL}/kill/release`, 'release the freeze', body)
// An agent's token is revoked on the MCP Tokens route, like any token.
export { revokeToken as revokeAgentToken } from '../mcptokens/tokenApi'
export const agentActivity = id => call(`${agentURL(id)}/activity`, 'load activity')

export async function listUsers() {
  const data = await call('/api/v1/users', 'load users')
  return Array.isArray(data?.users) ? data.users : []
}

export async function listAgentTokens(id) {
  const data = await call('/api/v1/mcp/tokens', 'load tokens')
  return tokensFor(data?.tokens, id)
}

const identitiesURL = id => `${agentURL(id)}/identities`

export const listAgentIdentities = id => call(identitiesURL(id), 'load OAuth identities')
export const bindAgentIdentity = (id, body) =>
  post(identitiesURL(id), 'bind the identity', body)
export const unbindAgentIdentity = (id, issuer, subject) => call(
  `${identitiesURL(id)}?issuer=${encodeURIComponent(issuer)}` +
  `&subject=${encodeURIComponent(subject)}`, 'unbind the identity', { method: 'DELETE' })
export const oauthIssuers = () => call('/api/v1/mcp/oauth/issuers', 'load OAuth issuers')

const MAX_SUBJECT_BYTES = 255

// identityBody builds POST /api/v1/agents/{id}/identities: an issuer from
// the configured list and a trimmed subject of 1 to 255 bytes, or null.
export function identityBody(form, issuers) {
  const subject = form.subject.trim()
  if (!Array.isArray(issuers) || !issuers.includes(form.issuer)) return null
  if (!subject || new TextEncoder().encode(subject).length > MAX_SUBJECT_BYTES) return null
  return { issuer: form.issuer, subject }
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

// profileNames are the names of the profiles GET /api/v1/agents serves
// (agentgov-06), skipping malformed entries; without a list, only the
// default profile is offered.
export function profileNames(profiles) {
  if (!Array.isArray(profiles)) return [DEFAULT_PROFILE]
  const names = profiles.filter(p => p && typeof p.name === 'string' && p.name)
    .map(p => p.name)
  return names.length ? names : [DEFAULT_PROFILE]
}

// unknownProfile reports an agent whose profile is not in the served list;
// without a list nothing is known to be wrong.
export function unknownProfile(agent, profiles) {
  if (!Array.isArray(profiles) || profiles.length === 0) return false
  return !profileNames(profiles).includes(agent?.profile)
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

export const KILL_SCOPES = ['all', 'principal', 'database']

// killReady: the typed phrase, a reason and, for one agent or one
// database, its target.
export function killReady(phrase, reason, scope = 'all', target = '') {
  if (phrase !== KILL_PHRASE || reason.trim() === '') return false
  if (!KILL_SCOPES.includes(scope)) return false
  return scope === 'all' || target !== ''
}

// killBody builds POST /api/v1/agents/kill: the target is the agent id or
// the database name, sent as id.
export function killBody({ scope, target, reason }) {
  const body = { scope, reason: reason.trim() }
  return scope === 'all' ? body : { scope, id: target, reason: body.reason }
}

// sponsorOptions are the users who may sponsor an agent.
export function sponsorOptions(users) {
  return Array.isArray(users) ? users.filter(u => u.role === 'operator' ||
    u.role === 'admin') : []
}

// editBody builds PATCH /api/v1/agents/{id} from the edit form with only
// the changed fields (and the reason), or null when nothing valid changed.
// An empty sponsor choice is no change: the API cannot clear a sponsor.
export function editBody(agent, form) {
  if (!agent || !form) return null
  const body = {}
  if (form.sponsor !== '') {
    const id = Number(form.sponsor)
    if (!Number.isInteger(id) || id <= 0) return null
    if (id !== agent.sponsor_user_id) body.sponsor_user_id = id
  }
  const profile = form.profile.trim()
  if (!profile || !ENVIRONMENTS.includes(form.ceiling)) return null
  if (profile !== agent.profile) body.profile = profile
  if (form.ceiling !== agent.env_ceiling) body.env_ceiling = form.ceiling
  if (Object.keys(body).length === 0) return null
  if (form.reason.trim()) body.reason = form.reason.trim()
  return body
}

// retireReady: a live agent, its exact name typed and a reason.
export function retireReady(agent, typed, reason) {
  return !!agent && agent.status !== 'retired' && typed === agent.name &&
    reason.trim() !== ''
}

const RELEASABLE = { fleet: 0, database: 1 }

// freezeItems keeps the fleet and database freezes of GET
// /api/v1/agents/freezes, fleet first; an agent's freeze is lifted from
// its drawer.
export function freezeItems(data) {
  const list = Array.isArray(data?.freezes) ? data.freezes : []
  return list.filter(f => f && f.scope in RELEASABLE)
    .sort((a, b) => RELEASABLE[a.scope] - RELEASABLE[b.scope])
}

// releaseBody builds POST /api/v1/agents/kill/release for a fleet or
// database freeze, or null without a reason.
export function releaseBody(freeze, reason) {
  const why = reason.trim()
  if (!why || !(freeze?.scope in RELEASABLE)) return null
  return freeze.scope === 'fleet' ? { scope: 'all', reason: why }
    : { scope: 'database', database: freeze.target, reason: why }
}

// releaseMessage explains a release result.
export function releaseMessage(r) {
  if (r.pending) {
    return `Pending second admin (request #${r.request_id}, started by ` +
      `${r.requested_by}). Another admin, not an agent's sponsor, releases it too.`
  }
  return r.single_operator ? 'Released by one admin in single-operator mode.' : 'Released.'
}
