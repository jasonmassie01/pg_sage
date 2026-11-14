// mocks/agents.ts — a stateful mock of the agent kill, freeze, unfreeze
// and release routes for the Agents page e2e spec (agentgov-07b).
//
// One AgentsMock holds the agents, the open freezes and the pending
// requests; every page (one browser context per admin) routes
// /api/v1/agents* to it as its own session user, so two admins act on one
// shared state. The codes and bodies follow the real routes:
//   - internal/api/agent_kill_routes.go: kill answers 200 with the
//     KillReport, release and unfreeze answer 202 whether applied or
//     pending (writeUnfreeze), errors are {error, code} (writeAgentKillError
//     through sreErrorCode, sre_handlers.go), a malformed or unknown-field
//     body is 422 invalid_arguments (decode, DisallowUnknownFields);
//   - internal/agentguard/unfreeze.go: UnfreezeResult; after a kill the
//     first admin's call is pending, the same admin again stays pending on
//     the same request, a different admin applies it, and for an agent's
//     own freeze the agent's sponsor may not be that second admin (403
//     sponsor_cannot_approve); a fleet or database flag has no sponsor;
//   - internal/agentguard/kill_freezes.go: {freezes, truncated}, each with
//     pending_release;
//   - internal/agentguard/kill_types.go, principal.go: a kill or freeze
//     needs a 1 to 2000 character reason (an unfreeze or release may have
//     none), a principal id is agp_ and 20 base32 characters, and the
//     stored reason is prefixed "kill: " or "freeze: " (kill_switch.go).
// The Go side of these rules is pinned by internal/api/agent_kill_routes_test.go,
// internal/api/agent_freezes_routes_test.go and internal/agentguard/unfreeze_db_test.go.
import { type Page, type Route } from '@playwright/test'
import { mockDatabases } from '../fixtures'

export interface MockUser {
  id: number
  email: string
  role: 'admin' | 'operator'
}

export const ADMIN1: MockUser = { id: 1, email: 'admin1@example.com', role: 'admin' }
export const ADMIN2: MockUser = { id: 2, email: 'admin2@example.com', role: 'admin' }

export const AT = '2026-11-13T12:00:00Z'
const EXPIRES = '2026-11-13T13:00:00Z'

interface Pending {
  request_id: number
  requested_by: string
  user_id: number
}

interface Freeze {
  id: number
  scope: 'fleet' | 'database'
  target: string
  reason: string
  set_by: string
  after_kill: boolean
  pending: Pending | null
}

export interface Agent {
  id: string
  name: string
  sponsor_user_id: number | null
  tenant: string
  profile: string
  env_ceiling: string
  status: 'active' | 'frozen' | 'retired'
  frozen_reason?: string
  created_by: string
  created_at: string
  updated_at: string
  sponsor_active: boolean
  tainted: boolean
}

// AgentLock is an agent's own freeze: after a kill it needs two admins.
interface AgentLock {
  afterKill: boolean
  pending: Pending | null
}

// Sent is one call the page made, with its session user.
export interface Sent {
  method: string
  path: string
  body: unknown
  user: string
}

type Body = Record<string, unknown>

interface Reply {
  status: number
  json: unknown
}

const ok = (json: unknown): Reply => ({ status: 200, json })
const accepted = (json: unknown): Reply => ({ status: 202, json })
const fail = (status: number, code: string, error: string): Reply =>
  ({ status, json: { error, code } })

export const DATABASES = ['primary', 'orders']

// Principal ids have the real shape (agentguard idPattern).
export const ALPHA_ID = 'agp_alpha234567abcdefghi'
export const BETA_ID = 'agp_beta234567abcdefghij'
const ID_PATTERN = /^agp_[a-z2-7]{20}$/
const MAX_REASON = 2000

const PROFILES = [
  { name: 'readonly-analyst', classes: ['read'], env_ceiling: 'prod', direct_lane: false },
  { name: 'app-writer', classes: ['read', 'write_insert', 'write_update'],
    env_ceiling: 'prod', direct_lane: true },
]

function agent(id: string, name: string, sponsor: number): Agent {
  return { id, name, sponsor_user_id: sponsor, tenant: '', profile: 'readonly-analyst',
    env_ceiling: 'prod', status: 'active', created_by: ADMIN1.email, created_at: AT,
    updated_at: AT, sponsor_active: true, tainted: false }
}

// strict mirrors the routes' DisallowUnknownFields decode.
function strict(body: unknown, allowed: string[]): Body | null {
  if (typeof body !== 'object' || body === null || Array.isArray(body)) return null
  const b = body as Body
  return Object.keys(b).every(k => allowed.includes(k)) ? b : null
}

const text = (v: unknown): string => (typeof v === 'string' ? v : '')

const malformed = (): Reply =>
  fail(422, 'invalid_arguments', 'malformed body: unknown field or not an object')

const invalid = (msg: string): Reply =>
  fail(422, 'invalid_arguments', `agentguard: invalid request: ${msg}`)

// badReason is checkText("reason", reason, min, 2000): a kill or freeze
// needs one, an unfreeze or release may have none; blanks are not trimmed.
function badReason(reason: string, min: number): Reply | null {
  const n = [...reason].length
  return n < min || n > MAX_REASON
    ? invalid(`reason must be ${min} to ${MAX_REASON} characters`) : null
}

const badID = (id: string): Reply | null =>
  (ID_PATTERN.test(id) ? null : invalid(`principal id "${id}" is not valid`))

export class AgentsMock {
  agents: Agent[] = [agent(ALPHA_ID, 'alpha', ADMIN1.id),
    agent(BETA_ID, 'beta', ADMIN2.id)]
  freezes: Freeze[] = []
  truncated = false
  failKill = false
  sent: Sent[] = []
  private locks = new Map<string, AgentLock>()
  private nextID = 40

  private id(): number {
    this.nextID += 1
    return this.nextID
  }

  // seedFreeze opens a fleet or database flag as a kill would.
  seedFreeze(scope: 'fleet' | 'database', target = '', reason = 'seeded'): void {
    this.freezes.push({ id: this.id(), scope, target, reason, set_by: ADMIN1.email,
      after_kill: true, pending: null })
  }

  // calls are the recorded calls to one method and path.
  calls(method: string, path: string): Sent[] {
    return this.sent.filter(s => s.method === method && s.path === path)
  }

  freezeList(): Reply {
    const freezes = this.freezes.map(f => ({ id: f.id, scope: f.scope, target: f.target,
      reason: f.reason, set_by: f.set_by, set_at: AT, after_kill: f.after_kill,
      pending_release: f.pending && { request_id: f.pending.request_id,
        requested_by: f.pending.requested_by, requested_at: AT, expires_at: EXPIRES } }))
    return ok({ freezes, truncated: this.truncated })
  }

  private report(scope: string, id: string, reason: string, principals: string[]): Reply {
    const dbs = scope === 'database' ? [id] : DATABASES
    return ok({ kill_id: this.id(), scope, ...(id ? { id } : {}), reason, principals,
      tokens_revoked: principals.length, approvals_cancelled: 0,
      databases: dbs.map(name => ({ name, roles_disabled: principals.length,
        backends_terminated: 1, statements_cancelled: 0, approvals_cancelled: 0,
        replicas: [], verified: true })),
      verified: true, started_at: AT, finished_at: AT })
  }

  private freezeAgent(a: Agent, reason: string, afterKill: boolean): void {
    a.status = 'frozen'
    a.frozen_reason = reason
    this.locks.set(a.id, { afterKill, pending: null })
  }

  kill(body: unknown, user: MockUser): Reply {
    const b = strict(body, ['scope', 'id', 'reason'])
    if (!b) return malformed()
    const scope = text(b.scope)
    const id = text(b.id)
    const bad = badReason(text(b.reason), 1)
    if (bad) return bad
    const reason = `kill: ${text(b.reason)}`
    if (this.failKill) return { status: 500, json: { error: 'internal error' } }
    const live = this.agents.filter(a => a.status !== 'retired')
    if (scope === 'all' && id === '') {
      live.forEach(a => this.freezeAgent(a, reason, true))
      this.freezes.push({ id: this.id(), scope: 'fleet', target: '', reason,
        set_by: user.email, after_kill: true, pending: null })
      return this.report(scope, '', reason, live.map(a => a.id))
    }
    if (scope === 'database') {
      if (!DATABASES.includes(id)) {
        return fail(404, 'not_found', `agentguard: not found: database "${id}"`)
      }
      this.freezes.push({ id: this.id(), scope: 'database', target: id, reason,
        set_by: user.email, after_kill: true, pending: null })
      return this.report(scope, id, reason, live.map(a => a.id))
    }
    if (scope !== 'principal') {
      return invalid(`scope "${scope}" must be all, principal or database`)
    }
    if (badID(id)) return invalid('scope principal needs a principal id (agp_…)')
    const one = live.find(a => a.id === id)
    if (!one) return fail(404, 'not_found', `agentguard: not found: principal "${id}"`)
    this.freezeAgent(one, reason, true)
    return this.report(scope, id, reason, [one.id])
  }

  // signOff is unfreeze.go signOff: pending for the first admin and for
  // the same admin again; null when this admin completes the quorum.
  private signOff(holder: { pending: Pending | null }, user: MockUser): Reply | null {
    const p = holder.pending
    if (p === null || p.user_id === user.id) {
      const pend = p ?? { request_id: this.id(), requested_by: user.email, user_id: user.id }
      holder.pending = pend
      return accepted({ applied: false, pending: true, request_id: pend.request_id,
        requested_by: pend.requested_by, quorum: 2 })
    }
    return null
  }

  release(body: unknown, user: MockUser): Reply {
    const b = strict(body, ['scope', 'database', 'reason'])
    if (!b) return malformed()
    // ReleaseRequest.Validate: scope all takes no database, scope
    // database needs one; anything else is refused.
    const target = text(b.database)
    if (b.scope === 'all' && target !== '') return invalid('scope all takes no database')
    if (b.scope === 'database' && target === '') {
      return invalid('database must be 1 to 200 characters')
    }
    if (b.scope !== 'all' && b.scope !== 'database') {
      return invalid(`scope "${text(b.scope)}" must be all or database`)
    }
    const bad = badReason(text(b.reason), 0)
    if (bad) return bad
    const scope = b.scope === 'all' ? 'fleet' : 'database'
    const f = this.freezes.find(x => x.scope === scope && x.target === target)
    if (!f) {
      return fail(409, 'not_frozen',
        `agentguard: not frozen: no open ${scope} freeze "${target}"`)
    }
    const pending = this.signOff(f, user)
    if (pending) return pending
    this.freezes = this.freezes.filter(x => x !== f)
    return accepted({ applied: true, pending: false, request_id: f.pending?.request_id,
      requested_by: f.pending?.requested_by, quorum: 2 })
  }

  freeze(a: Agent, body: unknown): Reply {
    const b = strict(body, ['reason'])
    if (!b) return malformed()
    const bad = badReason(text(b.reason), 1)
    if (bad) return bad
    const reason = `freeze: ${text(b.reason)}`
    this.freezeAgent(a, reason, false)
    return this.report('principal', a.id, reason, [a.id])
  }

  unfreeze(a: Agent, body: unknown, user: MockUser): Reply {
    const b = strict(body, ['reason'])
    if (!b) return malformed()
    const bad = badReason(text(b.reason), 0)
    if (bad) return bad
    const lock = this.locks.get(a.id)
    if (a.status !== 'frozen' || !lock) {
      return fail(409, 'not_frozen', `agentguard: not frozen: principal ${a.id} is ${a.status}`)
    }
    if (lock.afterKill) {
      const pending = this.signOff(lock, user)
      if (pending) return pending
      if (a.sponsor_user_id === user.id) {
        return fail(403, 'sponsor_cannot_approve',
          "agentguard: the principal's sponsor cannot be the second approver")
      }
    }
    a.status = 'active'
    delete a.frozen_reason
    this.locks.delete(a.id)
    return accepted({ applied: true, pending: false, request_id: lock.pending?.request_id,
      requested_by: lock.pending?.requested_by, quorum: lock.afterKill ? 2 : 1,
      clusters: [{ cluster_key: 'primary', action_id: this.id(), rotated: true,
        restored: {} }] })
  }

  // patch is agentgov-06: any profile change widens and waits for a
  // second admin (agent_change_routes.go requestChange).
  patch(a: Agent, body: unknown, user: MockUser): Reply {
    const b = strict(body, ['sponsor_user_id', 'profile', 'env_ceiling', 'reason'])
    if (!b) return malformed()
    if (b.profile === undefined) return ok({ ...a, ...b })
    return accepted({ status: 'pending', change_id: this.id(), expires_at: EXPIRES,
      requested_by: user.email, patch: b, superseded_change_id: 0,
      message: 'this change widens what the agent may do; another admin, not its ' +
        'sponsor, confirms it with the same fields' })
  }

  // agentRoute answers /api/v1/agents/{id}[/sub].
  private agentRoute(method: string, rest: string, body: unknown, user: MockUser): Reply {
    const [raw, sub = ''] = rest.split('/')
    const id = decodeURIComponent(raw)
    const bad = method === 'POST' ? badID(id) : null
    if (bad) return bad
    const a = this.agents.find(x => x.id === id)
    if (!a) return fail(404, 'not_found', `agentguard: not found: principal "${id}"`)
    if (method === 'GET' && sub === '') return ok(a)
    if (method === 'PATCH' && sub === '') return this.patch(a, body, user)
    if (method === 'POST' && sub === 'freeze') return this.freeze(a, body)
    if (method === 'POST' && sub === 'unfreeze') return this.unfreeze(a, body, user)
    return ok({ items: [] })
  }

  answer(method: string, path: string, body: unknown, user: MockUser): Reply {
    const rest = path.replace(/^\/api\/v1\/agents\/?/, '')
    if (method === 'GET' && rest === '') {
      return ok({ items: this.agents, next_cursor: '', profiles: PROFILES,
        retire_grace_days: 7 })
    }
    if (method === 'GET' && rest === 'freezes') return this.freezeList()
    if (method === 'POST' && rest === 'kill') return this.kill(body, user)
    if (method === 'POST' && rest === 'kill/release') return this.release(body, user)
    if (rest !== '') return this.agentRoute(method, rest, body, user)
    return fail(405, 'method_not_allowed', `${method} ${path}`)
  }
}

function bodyOf(route: Route): unknown {
  const raw = route.request().postData()
  if (raw === null) return null
  try {
    return JSON.parse(raw)
  } catch (err) {
    if (err instanceof SyntaxError) return raw
    throw err
  }
}

// installAgentsMock routes the Agents page's calls on page to mock as
// user; call it after mockAllAPIs so these routes win.
export async function installAgentsMock(page: Page, mock: AgentsMock,
  user: MockUser): Promise<void> {
  await page.route('**/api/v1/auth/me', route =>
    route.fulfill({ json: { id: user.id, user: user.email, email: user.email,
      role: user.role } }))
  await page.route('**/api/v1/users', route =>
    route.fulfill({ json: { users: [ADMIN1, ADMIN2] } }))
  // The fleet is primary and orders, so a database-scope kill can pick
  // orders; /databases/managed keeps the fixtures' answer.
  await page.route('**/api/v1/databases**', route => {
    if (route.request().url().includes('/managed')) return route.fallback()
    return route.fulfill({ json: { ...mockDatabases, databases: DATABASES.map((name, i) =>
      ({ ...mockDatabases.databases[i], name })) } })
  })
  await page.route('**/api/v1/agents**', route => {
    const req = route.request()
    const path = new URL(req.url()).pathname
    const body = bodyOf(route)
    mock.sent.push({ method: req.method(), path, body, user: user.email })
    const reply = mock.answer(req.method(), path, body, user)
    return route.fulfill({ status: reply.status, json: reply.json })
  })
}
