import { describe, expect, it } from 'vitest'
import {
  NAV_GROUPS, ROUTES, allowsRole, canSee, canonicalRoute, findRoute, navGroups,
  paletteRoutes, routeHref, routeGuideTitle, routeLabel, routeMemeLabel,
} from './routes'
import { pageGuide } from './components/pageGuide'

// dashboard-06: one route registry feeds App, the nav, the command palette,
// the tab guide and the meme label. ux-11: the nav is 14 entries in Operate,
// Admin and Evidence; every moved page keeps its old path as an alias.
// No concurrent access tests: the registry is a static module with no writers.

const admin = { email: 'a@x.test', role: 'admin' }
const operator = { email: 'o@x.test', role: 'operator' }
const viewer = { email: 'v@x.test', role: 'viewer' }
const byId = id => ROUTES.find(route => route.id === id)
const ids = routes => routes.map(route => route.id)
const ADMIN_ONLY = ['databases', 'users', 'settings']

// NAV_MAP is the planned nav (ux-11), group by group, in order.
const NAV_MAP = {
  operate: [['inbox', 'Inbox'], ['actions', 'Changes'], ['cases', 'Issues'], ['ask', 'Ask Sage']],
  admin: [['databases', 'Fleet'], ['autonomy', 'Autonomy'], ['notifications', 'Notifications'],
    ['agents', 'Agents'], ['users', 'Users'], ['settings', 'Settings']],
  evidence: [['queries', 'Workload'], ['slos', 'SLOs'], ['facts', 'Facts'],
    ['runbooks', 'Runbooks']],
}

describe('route registry integrity', () => {
  it('gives every route a non-empty title, label, guide and meme label', () => {
    expect(ROUTES.length).toBeGreaterThanOrEqual(20)
    for (const route of ROUTES) {
      expect(route.id, JSON.stringify(route)).toMatch(/^[a-z][a-z-]*$/)
      expect(route.path, route.id).toMatch(/^\/[a-z/-]*$/)
      for (const text of [route.title, route.guide, routeLabel(route),
        routeGuideTitle(route), routeMemeLabel(route)]) {
        expect(typeof text, route.id).toBe('string')
        expect(text.trim().length, route.id).toBeGreaterThan(0)
      }
    }
  })

  it('never lets two routes claim the same path or alias', () => {
    const claimed = ROUTES.flatMap(route => [route.path, ...(route.aliases || [])])
    expect(new Set(claimed).size).toBe(claimed.length)
    expect(new Set(ids(ROUTES)).size).toBe(ROUTES.length)
  })

  it('gives every nav route a unique nav-* test id in a known group', () => {
    const groups = new Set(NAV_GROUPS.map(group => group.id))
    const nav = ROUTES.filter(route => route.navGroup)
    for (const route of nav) {
      expect(groups.has(route.navGroup), route.id).toBe(true)
      expect(route.testId, route.id).toMatch(/^nav-[a-z-]+$/)
      expect(route.hint?.trim().length, route.id).toBeGreaterThan(0)
    }
    const testIds = nav.map(route => route.testId)
    expect(new Set(testIds).size).toBe(testIds.length)
  })

  it('keeps the nav test ids and moves Changes and Issues to their new URLs', () => {
    const expected = {
      'nav-inbox': '#/', 'nav-cases': '#/issues', 'nav-actions': '#/changes',
      'nav-slos': '#/slos', 'nav-autonomy': '#/autonomy', 'nav-facts': '#/facts',
      'nav-ask': '#/ask', 'nav-databases': '#/manage-databases',
      'nav-settings': '#/settings', 'nav-agents': '#/agents',
      'nav-queries': '#/queries', 'nav-runbooks': '#/advanced/runbooks',
    }
    for (const [testId, href] of Object.entries(expected)) {
      const route = ROUTES.find(r => r.testId === testId)
      expect(route, testId).toBeDefined()
      expect(routeHref(route)).toBe(href)
    }
  })
})

describe('the ux-11 nav map', () => {
  it('has exactly 14 entries in Operate, Admin and Evidence, in the planned order', () => {
    expect(NAV_GROUPS.map(group => group.heading)).toEqual(['Operate', 'Admin', 'Evidence'])
    expect(NAV_GROUPS.filter(group => group.collapsible).map(group => group.id))
      .toEqual(['evidence'])
    const groups = navGroups(admin, false)
    expect(groups.map(group => group.id)).toEqual(['operate', 'admin', 'evidence'])
    for (const group of groups) {
      expect(group.routes.map(r => [r.id, routeLabel(r)]), group.id)
        .toEqual(NAV_MAP[group.id])
    }
    expect(ROUTES.filter(route => route.navGroup)).toHaveLength(14)
    expect(paletteRoutes(admin, false)).toHaveLength(14)
  })

  it('lists no Value, Ledger, Alert log, MCP tokens or Snapshot entry', () => {
    const labels = ROUTES.filter(r => r.navGroup).map(routeLabel)
    for (const gone of ['Value', 'Ledger', 'Alert log', 'MCP tokens', 'Snapshot & metrics',
      'Actions', 'Findings explorer', 'Action history']) {
      expect(labels, gone).not.toContain(gone)
    }
    expect(new Set(labels).size).toBe(labels.length)
    for (const gone of ['value', 'snapshot', 'mcp-tokens', 'alerts', 'forecasts',
      'query-hints', 'schema-health', 'incidents']) {
      expect(byId(gone), gone).toBeUndefined()
    }
  })

  it('keeps the ledger a routed page outside the nav', () => {
    expect(byId('ledger').navGroup).toBeUndefined()
    expect(findRoute('#/ledger?evidence=e1')).toBe(byId('ledger'))
  })

  it('resolves every palette route back to itself', () => {
    for (const route of paletteRoutes(admin, false)) {
      expect(findRoute(routeHref(route))).toBe(route)
      expect(canonicalRoute(routeHref(route))).toBe(route.path)
    }
  })
})

// Every old path keeps working: it resolves to its new home, and the
// redirect keeps ?database= and ?case=.
const ALIASES = [
  ['/', 'inbox', '/'],
  ['/value', 'ledger', '/ledger?section=value'],
  ['/actions', 'actions', '/changes'],
  ['/advanced/actions', 'actions', '/changes'],
  ['/cases', 'cases', '/issues'],
  ['/findings', 'cases', '/issues'],
  ['/advanced/findings', 'cases', '/issues'],
  ['/forecasts', 'cases', '/issues?source=forecast'],
  ['/query-hints', 'cases', '/issues?source=query_hint'],
  ['/schema-health', 'cases', '/issues?source=schema_health'],
  ['/incidents', 'cases', '/issues?source=incident'],
  ['/trust', 'autonomy', '/autonomy'],
  ['/advanced/autonomy', 'autonomy', '/autonomy?section=history'],
  ['/advanced', 'databases', '/manage-databases?tab=readiness'],
  ['/alerts', 'notifications', '/notifications?tab=log'],
  ['/mcp-tokens', 'agents', '/agents?tab=tokens'],
]

function withQuery(path, query) {
  const [base, view] = path.split('?')
  return `${base}?${view ? `${view}&` : ''}${query}`
}

describe('old paths and aliases', () => {
  it.each(ALIASES)('%s resolves to %s at %s', (old, id, target) => {
    expect(findRoute(`#${old}`)).toBe(byId(id))
    expect(canonicalRoute(`#${old}`)).toBe(target)
    expect(canonicalRoute(old)).toBe(target)
  })

  it.each(ALIASES)('%s keeps ?database= and ?case= on the redirect', (old, id, target) => {
    const query = 'database=orders&case=42'
    expect(canonicalRoute(`#${old}?${query}`)).toBe(withQuery(target, query))
    expect(findRoute(canonicalRoute(`#${old}?${query}`))).toBe(byId(id))
  })

  it('keeps a query value the link already chose over the alias default', () => {
    expect(canonicalRoute('#/forecasts?source=incident')).toBe('/issues?source=incident')
    expect(canonicalRoute('#/alerts?tab=channels&database=a'))
      .toBe('/notifications?tab=channels&database=a')
  })

  it('keeps encoded query values intact', () => {
    expect(canonicalRoute('#/cases?database=a%26b&case=1'))
      .toBe('/issues?database=a%26b&case=1')
  })

  it('leaves canonical, unknown and empty routes as they are', () => {
    expect(canonicalRoute('#/issues?case=3')).toBe('/issues?case=3')
    expect(canonicalRoute('#/no-such-page?x=1')).toBe('/no-such-page?x=1')
    expect(canonicalRoute('')).toBe('/')
    expect(canonicalRoute(undefined)).toBe('/')
    expect(canonicalRoute(null)).toBe('/')
    expect(canonicalRoute('#')).toBe('/')
  })
})

describe('findRoute', () => {
  it('matches a hash, a bare path and a hash with a query', () => {
    expect(findRoute('#/issues').id).toBe('cases')
    expect(findRoute('/issues').id).toBe('cases')
    expect(findRoute('#/issues?case=12&database=orders').id).toBe('cases')
    expect(findRoute('#/').id).toBe('inbox')
  })

  it('returns undefined for unrouted, prefixed or differently cased paths', () => {
    expect(findRoute('#/alert-log')).toBeUndefined()
    expect(findRoute('#/issues/12')).toBeUndefined()
    expect(findRoute('#/advanced/unknown')).toBeUndefined()
    expect(findRoute('#/Issues')).toBeUndefined()
    expect(findRoute('#/users/')).toBeUndefined()
  })

  it('treats a missing or empty route as the landing page', () => {
    expect(findRoute(undefined).id).toBe('inbox')
    expect(findRoute(null).id).toBe('inbox')
    expect(findRoute('').id).toBe('inbox')
    expect(findRoute('#').id).toBe('inbox')
  })

  it('finds signed-out pages only when asked to', () => {
    expect(findRoute('#/link-sso')).toBeUndefined()
    expect(findRoute('#/link-sso?grant=x', { includeSignedOut: true }).id).toBe('link-sso')
    expect(findRoute('#/login').id).toBe('login')
  })
})

describe('canSee and allowsRole', () => {
  it('shows admins every route of a standalone deployment', () => {
    for (const route of ROUTES.filter(r => !r.signedOutOnly)) {
      expect(canSee(route, admin, false), route.id).toBe(true)
    }
  })

  it('hides the admin pages, Notifications and Agents from viewers', () => {
    for (const id of [...ADMIN_ONLY, 'notifications', 'agents']) {
      expect(canSee(byId(id), viewer, false), id).toBe(false)
      expect(allowsRole(byId(id), viewer), id).toBe(false)
    }
    for (const id of ['inbox', 'cases', 'actions', 'ask', 'ledger', 'autonomy', 'queries',
      'slos', 'facts', 'runbooks']) {
      expect(canSee(byId(id), viewer, false), id).toBe(true)
    }
  })

  it('gives operators Notifications and Agents but not the admin pages', () => {
    for (const id of ['notifications', 'agents', 'autonomy']) {
      expect(canSee(byId(id), operator, false), id).toBe(true)
    }
    for (const id of ADMIN_ONLY) {
      expect(canSee(byId(id), operator, false), id).toBe(false)
    }
  })

  it('shows each role only its entries and never an empty group', () => {
    const expected = {
      admin: { operate: 4, admin: 6, evidence: 4 },
      operator: { operate: 4, admin: 3, evidence: 4 },
      viewer: { operate: 4, admin: 1, evidence: 4 },
    }
    for (const [role, counts] of Object.entries(expected)) {
      const groups = navGroups({ role }, false)
      expect(Object.fromEntries(groups.map(g => [g.id, g.routes.length])), role)
        .toEqual(counts)
      for (const group of groups) expect(group.routes.length, group.id).toBeGreaterThan(0)
    }
    expect(ids(navGroups(viewer, false).find(g => g.id === 'admin').routes))
      .toEqual(['autonomy'])
  })

  it('hides Agents in a platform-managed deployment', () => {
    expect(canSee(byId('agents'), admin, true)).toBe(false)
    expect(canSee(byId('users'), admin, true)).toBe(true)
    // The role still allows the page; only the deployment hides it.
    expect(allowsRole(byId('agents'), admin)).toBe(true)
    expect(paletteRoutes(admin, true)).toHaveLength(13)
  })

  it('fails closed without a user or with an unknown role', () => {
    for (const route of ROUTES) {
      expect(canSee(route, null, false), route.id).toBe(false)
      expect(canSee(route, undefined, false), route.id).toBe(false)
      expect(allowsRole(route, {}), route.id).toBe(!route.roles)
    }
    expect(canSee(byId('users'), { role: 'superuser' }, false)).toBe(false)
    expect(canSee(byId('cases'), { role: 'superuser' }, false)).toBe(true)
  })

  it('never shows a signed-out page to a signed-in user', () => {
    expect(canSee(byId('link-sso'), admin, false)).toBe(false)
  })

  it('builds the palette from exactly the nav routes the user can see', () => {
    const navIds = navGroups(viewer, false).flatMap(group => ids(group.routes))
    expect(ids(paletteRoutes(viewer, false))).toEqual(navIds)
    expect(ids(paletteRoutes(viewer, false))).not.toContain('users')
    expect(paletteRoutes(null, false)).toEqual([])
    expect(navGroups(null, false)).toEqual([])
  })
})

describe('pageGuide from the registry', () => {
  it('uses the route guide, or the platform guide when the platform signs in', () => {
    expect(pageGuide('#/advanced/findings')).toEqual(
      { title: 'Issues', description: byId('cases').guide })
    expect(pageGuide('#/issues?source=forecast&database=a').title).toBe('Issues')
    expect(pageGuide('#/agents', { authMode: 'databricks_app' }).description)
      .toMatch(/Databricks sign-in/)
    expect(pageGuide('#/agents', { authMode: 'session' }).description)
      .toBe(byId('agents').guide)
    expect(pageGuide('#/issues', { authMode: 'databricks_app' }).description)
      .toBe(byId('cases').guide)
  })

  it('explains Access denied and unknown routes', () => {
    expect(pageGuide('#/users', { pageTitle: 'Access denied' }).title).toBe('Access denied')
    expect(pageGuide('#/alert-log').title).toBe('Page not found')
    expect(pageGuide('#/issues', { pageTitle: 'Not found' }).title).toBe('Page not found')
    expect(pageGuide('#/link-sso').title).toBe('Page not found')
  })
})
