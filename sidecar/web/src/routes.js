// routes.js: the one route registry (dashboard-06). App renders the page a
// route names (routeRender.jsx), and the nav, the command palette, the tab
// guide and the meme label are all derived from this list.
//
// Fields: path and aliases are hash paths without '#'. title is the page
// header; label names the route in the nav and palette (default title);
// guideTitle and memeLabel default to the label. roles limits the route to
// those user roles (absent: every signed-in user). standaloneOnly routes are
// unavailable when the platform manages sign-in (Databricks Apps).
// signedOutOnly routes render before sign-in and are never matched for a
// signed-in user. The UI gate is cosmetic: the API enforces every role.
// usesTimeRange routes are the pages the header time picker drives; the
// header shows the picker only there, captioned with timeRangeLabel
// (dashboard-07). aliases are old paths the route still answers: App
// redirects each to the route's path (canonicalRoute), adding the view the
// old path named (ALIAS_QUERY, ux-11).
import {
  Activity, AlertTriangle, BarChart3, Bell, BookCheck, Bot, Gauge, Inbox, MessageSquare,
  ScrollText, Server, Settings, ShieldCheck, Users,
} from 'lucide-react'

// NAV_GROUPS (ux-11): Operate is the daily work, Admin the setup and
// governance, Evidence the pages issues and plans link to.
export const NAV_GROUPS = [
  { id: 'operate', heading: 'Operate' },
  { id: 'admin', heading: 'Admin' },
  { id: 'evidence', heading: 'Evidence', collapsible: true },
]

const ADMIN = ['admin']
const REVIEWERS = ['admin', 'operator']
const CASES_GUIDE = 'See database issues, the evidence behind them and what remains unknown. '
  + 'Open an issue to investigate it or review a proposed next step.'
const ACCOUNT_GUIDE = 'Review your signed-in identity and account preferences. '
  + 'Your pg_sage role determines which controls you can use.'
const ACCOUNT_PLATFORM_GUIDE = 'Review your Databricks identity and pg_sage role. '
  + 'Databricks manages sign-in for this App.'
const PROTOCOL_PLATFORM_GUIDE = 'This App uses Databricks sign-in. '
  + 'Agent and MCP token access are unavailable in this deployment.'

export const ROUTES = Object.freeze([
  // The Inbox is home (ux-01b): what needs a person, what changed since
  // they were last here, and one Value line linking to the ledger.
  { id: 'inbox', path: '/', title: 'Inbox', navGroup: 'operate', testId: 'nav-inbox',
    icon: Inbox, hint: 'Decisions waiting on you, and what changed',
    guide: 'Decide what waits on you, from any page that owns it, and review the '
      + 'changes made since your last visit. Undo asks why and runs the stored rollback.' },
  // Changes (was Actions, ux-11): plans, approvals and history. The id and
  // the test id stay "actions" so automation keeps its hooks.
  { id: 'actions', path: '/changes', aliases: ['/actions', '/advanced/actions'],
    title: 'Changes', navGroup: 'operate', testId: 'nav-actions', icon: Activity,
    hint: 'Plans, approvals and the change history',
    usesTimeRange: true, timeRangeLabel: 'Ledger window',
    guide: 'Follow proposed database changes through review, approval and outcome '
      + 'checks. The database’s autonomy decides whether a change can run.' },
  // Issues (was Cases): the four filtered views are aliases that open the
  // list on their source (ALIAS_QUERY).
  { id: 'cases', path: '/issues',
    aliases: ['/cases', '/findings', '/advanced/findings', '/forecasts', '/query-hints',
      '/schema-health', '/incidents'],
    title: 'Issues', navGroup: 'operate', testId: 'nav-cases', icon: AlertTriangle,
    hint: 'Open DBA work queue', guide: CASES_GUIDE },
  { id: 'ask', path: '/ask', title: 'Ask Sage', navGroup: 'operate', testId: 'nav-ask',
    icon: MessageSquare, hint: 'Questions answered with cited evidence',
    guide: 'Ask questions about the selected database and inspect the cited evidence. '
      + 'Answers never execute or approve database changes.' },
  // The ledger is linked from the Inbox and Changes, not the nav. Value is
  // its summary (#/value opens it), and it keeps every external agent
  // request (ux-11).
  { id: 'ledger', path: '/ledger', aliases: ['/value'], title: 'Outcome ledger',
    label: 'Ledger',
    guide: 'See every change pg_sage made in a window, what it expected, what was '
      + 'measured and whether it can still be rolled back, and the value it delivered.' },
  // Fleet holds the old Snapshot & metrics page as its Health & readiness
  // tab; /advanced opens that tab.
  { id: 'databases', path: '/manage-databases', aliases: ['/advanced'], title: 'Databases',
    label: 'Fleet', navGroup: 'admin', testId: 'nav-databases', icon: Server, roles: ADMIN,
    hint: 'Database connections, fleet health and provider readiness',
    guide: 'See the databases pg_sage monitors, their connection health and provider '
      + 'readiness. Open a database to inspect its collected snapshot.' },
  // One Autonomy page (ux-06b, decision D6): one scale per class, the
  // waiting promotions, the ledger's history and the shadow ledger. The
  // old Trust and Earned autonomy paths stay as aliases.
  { id: 'autonomy', path: '/autonomy', aliases: ['/trust', '/advanced/autonomy'],
    title: 'Autonomy', navGroup: 'admin', testId: 'nav-autonomy', icon: ShieldCheck,
    hint: 'What pg_sage may do per class, and why',
    guide: 'See, per class, whether pg_sage watches only, asks first, acts then tells '
      + 'you, or acts in the window, and why. Approve waiting promotions here.' },
  // The Alert log is folded into Notifications (alerting-06c): /alerts
  // opens its Log tab.
  { id: 'notifications', path: '/notifications', aliases: ['/alerts'],
    title: 'Notifications', navGroup: 'admin', testId: 'nav-notifications', icon: Bell,
    roles: REVIEWERS, hint: 'Alert channels, rules and delivery history',
    guide: 'Inspect delivery history for the selected database to see what was sent or '
      + 'failed. Administrators also configure the delivery channels and rules.' },
  // MCP tokens are a tab of Agents for admins (ux-11); /mcp-tokens opens it.
  { id: 'agents', path: '/agents', aliases: ['/mcp-tokens'], title: 'Agents',
    navGroup: 'admin', testId: 'nav-agents', icon: Bot, roles: REVIEWERS,
    standaloneOnly: true, hint: 'Registered agents, their permissions and MCP tokens',
    platformGuide: PROTOCOL_PLATFORM_GUIDE,
    guide: 'Review registered agents, their permissions and activity. Administrators '
      + 'also manage the MCP tokens external tools connect with.' },
  { id: 'users', path: '/users', title: 'Users', navGroup: 'admin', testId: 'nav-users',
    icon: Users, roles: ADMIN, hint: 'Who can use pg_sage, and with which role',
    guide: 'Manage who can use pg_sage and what each role can do. App access and '
      + 'database permissions are separate.' },
  { id: 'settings', path: '/settings', title: 'Settings', navGroup: 'admin',
    testId: 'nav-settings', icon: Settings, roles: ADMIN, hint: 'Configure pg_sage',
    guide: 'Inspect monitoring, AI and safety settings. Scope labels show whether changes '
      + 'affect the pg_sage instance or one database.' },
  { id: 'queries', path: '/queries', title: 'Workload', navGroup: 'evidence',
    testId: 'nav-queries', icon: BarChart3, hint: 'Top statements and wait-class load',
    guide: 'See which statements carry the load and what active sessions wait on. '
      + 'Open a statement for its history, plan changes and related issues.' },
  { id: 'slos', path: '/slos', title: 'SLOs', navGroup: 'evidence', testId: 'nav-slos',
    icon: Gauge, hint: 'Reliability targets and error budgets',
    guide: 'Check reliability targets, remaining error budgets and how quickly failures '
      + 'consume them. Missing evidence appears as unknown.' },
  { id: 'facts', path: '/facts', title: 'Facts', navGroup: 'evidence', testId: 'nav-facts',
    icon: BookCheck, hint: 'Context pg_sage cannot discover',
    guide: 'Record context pg_sage cannot discover, such as application-owned indexes. '
      + 'Proposed facts need confirmation before they influence decisions.' },
  { id: 'runbooks', path: '/advanced/runbooks', title: 'Runbooks', navGroup: 'evidence',
    testId: 'nav-runbooks', icon: ScrollText, hint: 'Signed investigation guides',
    guide: 'Review versioned investigation guides. An administrator must sign each version '
      + 'before use; runbooks propose changes without executing them.' },
  // The health chart moved here from Snapshot & metrics (ux-11).
  { id: 'database', path: '/database', title: 'Database',
    usesTimeRange: true, timeRangeLabel: 'Health chart window',
    guide: 'Inspect the latest collected snapshot for one database, including its '
      + 'health chart, the measurements and the raw details used for analysis.' },
  { id: 'query', path: '/query', title: 'Query',
    guide: 'Inspect one statement: its history from the query store, plan changes, the '
      + 'newest captured plan, and the issues, hints and actions about it.' },
  { id: 'profile', path: '/profile', title: 'Account', memeLabel: 'Profile',
    guide: ACCOUNT_GUIDE, platformGuide: ACCOUNT_PLATFORM_GUIDE },
  // A signed-in user can land on /login from an SSO callback error; the
  // account page explains the sso_error (D7).
  { id: 'login', path: '/login', title: 'Account', memeLabel: 'Sign in',
    guide: ACCOUNT_GUIDE, platformGuide: ACCOUNT_PLATFORM_GUIDE },
  { id: 'link-sso', path: '/link-sso', title: 'Identity', signedOutOnly: true,
    guide: 'Link your single sign-on identity with the one-time link an administrator '
      + 'sent you.' },
].map(route => Object.freeze(route)))

// ALIAS_QUERY is the view an old path named, added to its new home's query
// unless the link already chose that parameter (ux-11).
const ALIAS_QUERY = Object.freeze({
  '/forecasts': { source: 'forecast' },
  '/query-hints': { source: 'query_hint' },
  '/schema-health': { source: 'schema_health' },
  '/incidents': { source: 'incident' },
  '/advanced/autonomy': { section: 'history' },
  '/advanced': { tab: 'readiness' },
  '/alerts': { tab: 'log' },
  '/mcp-tokens': { tab: 'tokens' },
  '/value': { section: 'value' },
})

export const routeLabel = route => route.label || route.title
export const routeGuideTitle = route => route.guideTitle || routeLabel(route)
export const routeMemeLabel = route => route.memeLabel || routeLabel(route)
export const routeHref = route => `#${route.path}`

// splitRoute turns '#/cases?case=1', '/cases' or '' into its path ('/'
// when empty) and its raw query.
function splitRoute(route) {
  const raw = String(route ?? '').replace(/^#/, '')
  const at = raw.indexOf('?')
  if (at < 0) return { path: raw || '/', query: '' }
  return { path: raw.slice(0, at) || '/', query: raw.slice(at + 1) }
}

// findRoute returns the route whose path or alias is exactly the given hash
// path, ignoring any query. Signed-out pages match only when asked for.
export function findRoute(route, { includeSignedOut = false } = {}) {
  const { path } = splitRoute(route)
  return ROUTES.find(r => (includeSignedOut || !r.signedOutOnly)
    && (r.path === path || r.aliases?.includes(path)))
}

// canonicalRoute returns the hash route, without '#', a route should show:
// an alias becomes its route's path, the alias's view first and then the
// query the link carried (?database=, ?case=) as written. Any other route,
// an unknown one too, comes back unchanged.
export function canonicalRoute(route) {
  const { path, query } = splitRoute(route)
  const match = ROUTES.find(r => r.aliases?.includes(path))
  if (!match) return query ? `${path}?${query}` : path
  const params = new URLSearchParams(query)
  const extra = new URLSearchParams()
  for (const [key, value] of Object.entries(ALIAS_QUERY[path] || {})) {
    if (!params.has(key)) extra.set(key, value)
  }
  const joined = [extra.toString(), query].filter(Boolean).join('&')
  return joined ? `${match.path}?${joined}` : match.path
}

// routeParam reads one query parameter of a hash route ('' when absent).
export function routeParam(route, key) {
  return new URLSearchParams(splitRoute(route).query).get(key) || ''
}

// allowsRole reports whether the user's role may open the route.
export function allowsRole(route, user) {
  return !route.roles || route.roles.includes(user?.role)
}

// canSee reports whether the nav and palette show the route to this user.
// No user sees nothing: the shell only renders signed in, so fail closed.
export function canSee(route, user, platformManaged) {
  if (!user || route.signedOutOnly) return false
  if (route.standaloneOnly && platformManaged) return false
  return allowsRole(route, user)
}

// navGroups returns the nav groups with the routes this user can see, in
// registry order, leaving out empty groups.
export function navGroups(user, platformManaged) {
  return NAV_GROUPS
    .map(group => ({
      ...group,
      routes: ROUTES.filter(r => r.navGroup === group.id && canSee(r, user, platformManaged)),
    }))
    .filter(group => group.routes.length > 0)
}

// paletteRoutes is every nav route the user can see, in nav order.
export function paletteRoutes(user, platformManaged) {
  return navGroups(user, platformManaged).flatMap(group => group.routes)
}
