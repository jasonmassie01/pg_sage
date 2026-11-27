// Policy set payloads (fleetpol-01c), copied from the Go routes' golden
// responses: GET /api/v1/policy-sets, /api/v1/policy-sets/prod/members and
// /api/v1/policy-exceptions as TestPolicySetRoutes_MatchTheDashboardGolden
// answers them (internal/api/testdata/policy_sets_golden.json). The
// golden's members fake answers every set; here default has none.

import { type Page, type Route } from '@playwright/test'

const at = '2026-11-10T09:00:00Z'
const doc = { profile: 'staffed', lock_duration_ceiling_ms: 30000 }

export function policyException(over: Record<string, unknown> = {}) {
  return {
    database_id: 9, set_name: 'prod',
    diff: [{ field: 'lock_duration_ceiling_ms', set: 30000, exception: 120000 }],
    set_by: 'alice@example.com', reason: 'month-end batch needs longer locks',
    set_at: at, expires_at: '2026-12-01T00:00:00Z', policy_id: 12, policy_version: 3,
    ...over,
  }
}

export function policySets() {
  return [
    { name: 'default', priority: 0, selectors: [{ all: true }], requirements: {},
      created_by: 'system-bootstrap', updated_at: at, policy_id: 1, version: 1,
      profile: 'staffed' },
    { name: 'prod', priority: 10, selectors: [{ tags: { tier: 'prod' } }],
      requirements: { owner_required: true, trust_ceiling: { index: 'advisory' } },
      created_by: 'system-bootstrap', updated_at: at, policy_id: 3, version: 2,
      profile: 'staffed' },
  ]
}

export function prodMembers() {
  return [
    { member: { database_id: 4, name: 'orders', tags: { tier: 'prod' } }, set: 'prod',
      policy: { id: 3, scope: {}, set: 'prod', version: 2, profile: 'staffed', document: doc,
        updated_by: 'system-bootstrap', updated_at: at } },
    { member: { database_id: 9, name: 'billing', tags: { tier: 'prod' } }, set: 'prod',
      policy: { id: 12, scope: {}, version: 3, profile: 'staffed',
        document: { ...doc, lock_duration_ceiling_ms: 120000 },
        updated_by: 'alice@example.com', updated_at: at },
      exception: policyException() },
  ]
}

// PolicyState is what the mocked server holds; tests change it and read
// the requests it saw.
export interface PolicyState {
  sets: ReturnType<typeof policySets>
  members: Record<string, unknown[]>
  exceptions: Record<string, unknown>[]
  setsStatus: number
  proposeStatus: number
  proposeError: string
  sent: { method: string, path: string, body: unknown }[]
}

export function policyState(over: Partial<PolicyState> = {}): PolicyState {
  return { sets: policySets(), members: { default: [], prod: prodMembers() },
    exceptions: [policyException()], setsStatus: 200, proposeStatus: 201,
    proposeError: '', sent: [], ...over }
}

function setPolicy(state: PolicyState, name: string) {
  const set = state.sets.find(s => s.name === name)
  return set && { id: set.policy_id, scope: {}, set: name, version: set.version,
    profile: set.profile, document: doc, updated_by: 'system-bootstrap', updated_at: at }
}

// ratify is the server side of a ratified set proposal: the set's version
// grows and the exception now matches the set's document.
function ratify(state: PolicyState) {
  state.sets[1].version += 1
  state.exceptions = state.exceptions.map(e => ({ ...e, diff: [] }))
}

function post(state: PolicyState, route: Route, path: string) {
  const body = route.request().postDataJSON()
  state.sent.push({ method: 'POST', path, body })
  if (path === '/api/v1/policy/proposals') {
    if (state.proposeStatus !== 201) {
      return route.fulfill({ status: state.proposeStatus, json: { error: state.proposeError } })
    }
    return route.fulfill({ status: 201, json: { id: 77, base_version: body.expected_version } })
  }
  if (path === '/api/v1/policy/proposals/77/ratify') {
    ratify(state)
    return route.fulfill({ json: { id: 13, version: state.sets[1].version } })
  }
  return route.fulfill({ status: 404, json: { error: `no mock for ${path}` } })
}

function get(state: PolicyState, route: Route, url: URL) {
  const path = url.pathname
  if (path === '/api/v1/policy-sets') {
    if (state.setsStatus !== 200) {
      return route.fulfill({ status: state.setsStatus,
        json: { error: 'read policy sets: connection refused' } })
    }
    return route.fulfill({ json: { sets: state.sets } })
  }
  if (path === '/api/v1/policy-exceptions') {
    return route.fulfill({ json: { exceptions: state.exceptions } })
  }
  const members = path.match(/^\/api\/v1\/policy-sets\/([^/]+)\/members$/)
  if (members) {
    return route.fulfill({ json: { set: members[1], members: state.members[members[1]] || [] } })
  }
  const exported = path.match(/^\/api\/v1\/policy-sets\/([^/]+)\/export$/)
  if (exported) {
    return route.fulfill({ contentType: 'application/yaml',
      body: `name: ${exported[1]}\npriority: 10\nselectors:\n  - tags:\n      tier: prod\n` })
  }
  const name = url.searchParams.get('set_name')
  if (name) return route.fulfill({ json: setPolicy(state, name) })
  const db = Number(url.searchParams.get('database_id'))
  if (db === 9) {
    return route.fulfill({ json: { id: 12, scope: { database_id: 9 }, version: 3,
      profile: 'staffed', document: doc, updated_by: 'alice@example.com', updated_at: at } })
  }
  return route.fulfill({ json: { ...setPolicy(state, 'prod'), scope: { database_id: db } } })
}

const POLICY_PATH = /^\/api\/v1\/(policy|policy-sets|policy-exceptions)(\/|$)/

// routePolicy answers every policy route from state.
export async function routePolicy(page: Page, state: PolicyState) {
  await page.route(url => POLICY_PATH.test(url.pathname) && !url.pathname.endsWith('/history'),
    route => {
      const url = new URL(route.request().url())
      if (route.request().method() === 'POST') return post(state, route, url.pathname)
      return get(state, route, url)
    })
}
