import { describe, expect, it } from 'vitest'
import {
  NAV_GROUPS, ROUTES, allowsRole, canSee, findRoute, navGroups, paletteRoutes,
  routeHref, routeGuideTitle, routeLabel, routeMemeLabel,
} from './routes'
import { pageGuide } from './components/pageGuide'

// dashboard-06: one route registry feeds App, the nav, the command palette,
// the tab guide and the meme label.
// No concurrent access tests: the registry is a static module with no writers.

const admin = { email: 'a@x.test', role: 'admin' }
const operator = { email: 'o@x.test', role: 'operator' }
const viewer = { email: 'v@x.test', role: 'viewer' }
const byId = id => ROUTES.find(route => route.id === id)
const ids = routes => routes.map(route => route.id)
const ADMIN_ONLY = ['databases', 'users', 'notifications', 'settings', 'mcp-tokens']

describe('route registry integrity', () => {
  it('gives every route a non-empty title, label, guide and meme label', () => {
    expect(ROUTES.length).toBeGreaterThan(20)
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

  it('keeps the existing nav test ids and hash URLs', () => {
    const expected = {
      'nav-inbox': '#/', 'nav-cases': '#/cases', 'nav-actions': '#/actions',
      'nav-slos': '#/slos', 'nav-autonomy': '#/autonomy', 'nav-facts': '#/facts',
      'nav-ask': '#/ask', 'nav-databases': '#/manage-databases',
      'nav-settings': '#/settings', 'nav-mcp-tokens': '#/mcp-tokens',
      'nav-agents': '#/agents',
    }
    for (const [testId, href] of Object.entries(expected)) {
      const route = ROUTES.find(r => r.testId === testId)
      expect(route, testId).toBeDefined()
      expect(routeHref(route)).toBe(href)
    }
  })

  it('puts the admin pages in an Admin group in the planned order', () => {
    const groups = navGroups(admin, false)
    expect(NAV_GROUPS.map(group => group.heading)).toEqual(['Operate', 'Admin', 'Advanced'])
    expect(ids(groups.find(g => g.id === 'admin').routes)).toEqual([
      'databases', 'users', 'notifications', 'alerts', 'settings', 'mcp-tokens', 'agents'])
    expect(ids(groups.find(g => g.id === 'advanced').routes))
      .toEqual(['runbooks', 'snapshot'])
  })

  it('lists no duplicate Advanced entry for Cases or Actions', () => {
    const labels = ROUTES.filter(r => r.navGroup).map(routeLabel)
    expect(labels).not.toContain('Findings explorer')
    expect(labels).not.toContain('Action history')
    expect(new Set(labels).size).toBe(labels.length)
  })

  it('resolves every palette route back to itself', () => {
    for (const route of paletteRoutes(admin, false)) {
      expect(findRoute(routeHref(route))).toBe(route)
    }
  })
})

describe('findRoute', () => {
  it('matches a hash, a bare path and a hash with a query', () => {
    expect(findRoute('#/cases').id).toBe('cases')
    expect(findRoute('/cases').id).toBe('cases')
    expect(findRoute('#/cases?case=12&database=orders').id).toBe('cases')
    expect(findRoute('#/').id).toBe('inbox')
    // ux-01b: the Inbox is home; the Value page stays at #/value.
    expect(findRoute('#/value').id).toBe('value')
  })

  it('resolves the old Advanced and findings URLs as aliases', () => {
    expect(findRoute('#/advanced/findings')).toBe(byId('cases'))
    expect(findRoute('#/findings')).toBe(byId('cases'))
    expect(findRoute('#/advanced/actions')).toBe(byId('actions'))
    expect(findRoute('#/advanced').id).toBe('snapshot')
  })

  it('returns undefined for unrouted, prefixed or differently cased paths', () => {
    expect(findRoute('#/alert-log')).toBeUndefined()
    expect(findRoute('#/cases/12')).toBeUndefined()
    expect(findRoute('#/advanced/unknown')).toBeUndefined()
    expect(findRoute('#/Cases')).toBeUndefined()
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

  it('hides the admin pages, Alert log and Agents from viewers', () => {
    for (const id of [...ADMIN_ONLY, 'alerts', 'agents']) {
      expect(canSee(byId(id), viewer, false), id).toBe(false)
      expect(allowsRole(byId(id), viewer), id).toBe(false)
    }
    for (const id of ['value', 'cases', 'actions', 'runbooks', 'snapshot']) {
      expect(canSee(byId(id), viewer, false), id).toBe(true)
    }
  })

  it('gives operators Alert log, Notifications and Agents but not the admin pages', () => {
    for (const id of ['alerts', 'notifications', 'agents']) {
      expect(canSee(byId(id), operator, false), id).toBe(true)
    }
    for (const id of ADMIN_ONLY.filter(id => id !== 'notifications')) {
      expect(canSee(byId(id), operator, false), id).toBe(false)
    }
  })

  it('hides MCP tokens and Agents in a platform-managed deployment', () => {
    expect(canSee(byId('mcp-tokens'), admin, true)).toBe(false)
    expect(canSee(byId('agents'), admin, true)).toBe(false)
    expect(canSee(byId('users'), admin, true)).toBe(true)
    // The role still allows the page; only the deployment hides it.
    expect(allowsRole(byId('mcp-tokens'), admin)).toBe(true)
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
    expect(navGroups(viewer, false).map(g => g.id)).toEqual(['operate', 'advanced'])
    expect(ids(paletteRoutes(admin, true))).not.toContain('mcp-tokens')
    expect(paletteRoutes(null, false)).toEqual([])
    expect(navGroups(null, false)).toEqual([])
  })
})

describe('pageGuide from the registry', () => {
  it('uses the route guide, or the platform guide when the platform signs in', () => {
    expect(pageGuide('#/advanced/findings')).toEqual(
      { title: 'Issues', description: byId('cases').guide })
    expect(pageGuide('#/forecasts?database=a').title).toBe('Issues · Forecasts')
    expect(pageGuide('#/mcp-tokens', { authMode: 'databricks_app' }).description)
      .toMatch(/Databricks sign-in/)
    expect(pageGuide('#/mcp-tokens', { authMode: 'session' }).description)
      .toBe(byId('mcp-tokens').guide)
    expect(pageGuide('#/cases', { authMode: 'databricks_app' }).description)
      .toBe(byId('cases').guide)
  })

  it('explains Access denied and unknown routes', () => {
    expect(pageGuide('#/users', { pageTitle: 'Access denied' }).title).toBe('Access denied')
    expect(pageGuide('#/alert-log').title).toBe('Page not found')
    expect(pageGuide('#/cases', { pageTitle: 'Not found' }).title).toBe('Page not found')
    expect(pageGuide('#/link-sso').title).toBe('Page not found')
  })
})
