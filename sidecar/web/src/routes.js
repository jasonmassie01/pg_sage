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
// (dashboard-07).
import {
  Activity, AlertTriangle, BarChart3, Bell, BookCheck, Bot, ClipboardCheck, Gauge, Home, Inbox,
  KeyRound, MessageSquare, ScrollText, Server, Settings, ShieldCheck, Users,
} from 'lucide-react'

export const NAV_GROUPS = [
  { id: 'operate', heading: 'Operate' },
  { id: 'admin', heading: 'Admin' },
  { id: 'advanced', heading: 'Advanced', collapsible: true },
]

const ADMIN = ['admin']
const REVIEWERS = ['admin', 'operator']
const CASES_GUIDE = 'See database issues, the evidence behind them and what remains unknown. '
  + 'Open a case to investigate it or review a proposed next step.'
const ACCOUNT_GUIDE = 'Review your signed-in identity and account preferences. '
  + 'Your pg_sage role determines which controls you can use.'
const ACCOUNT_PLATFORM_GUIDE = 'Review your Databricks identity and pg_sage role. '
  + 'Databricks manages sign-in for this App.'
const PROTOCOL_PLATFORM_GUIDE = 'This App uses Databricks sign-in. '
  + 'Agent and MCP token access are unavailable in this deployment.'

function caseView(id, path, label, guide) {
  return { id, path, title: 'Cases', guideTitle: `Cases · ${label}`, memeLabel: label, guide }
}

export const ROUTES = Object.freeze([
  // The Inbox is home (ux-01b): what needs a person, what changed since
  // they were last here, and one Value line linking to the Value page.
  { id: 'inbox', path: '/', title: 'Inbox', navGroup: 'operate', testId: 'nav-inbox',
    icon: Inbox, hint: 'Decisions waiting on you, and what changed',
    guide: 'Decide what waits on you, from any page that owns it, and review the '
      + 'changes made since your last visit. Undo asks why and runs the stored rollback.' },
  // The full Value page stays at #/value, linked from the Inbox.
  { id: 'value', path: '/value', title: 'Value', icon: Home, hint: 'Verified DBA hours saved',
    guide: 'Compare verified outcomes and estimated DBA effort saved, and see '
      + 'recommendations still waiting for review.' },
  { id: 'cases', path: '/cases', aliases: ['/findings', '/advanced/findings'], title: 'Cases',
    navGroup: 'operate', testId: 'nav-cases', icon: AlertTriangle,
    hint: 'Open DBA work queue', guide: CASES_GUIDE },
  { id: 'actions', path: '/actions', aliases: ['/advanced/actions'], title: 'Actions',
    navGroup: 'operate', testId: 'nav-actions', icon: Activity,
    hint: 'Executed + pending timeline',
    usesTimeRange: true, timeRangeLabel: 'Ledger window',
    guide: 'Follow proposed database changes through review, approval and outcome '
      + 'checks. The execution mode determines whether a change can run.' },
  { id: 'ledger', path: '/ledger', title: 'Outcome ledger', label: 'Ledger',
    navGroup: 'operate', testId: 'nav-ledger', icon: ClipboardCheck,
    hint: 'What changed, and what it did',
    guide: 'See every change pg_sage made in a window, what it expected, what was '
      + 'measured and whether it can still be rolled back.' },
  { id: 'queries', path: '/queries', title: 'Workload', navGroup: 'operate',
    testId: 'nav-queries', icon: BarChart3, hint: 'Top statements and wait-class load',
    guide: 'See which statements carry the load and what active sessions wait on. '
      + 'Open a statement for its history, plan changes and related cases.' },
  { id: 'slos', path: '/slos', title: 'SLOs', navGroup: 'operate', testId: 'nav-slos',
    icon: Gauge, hint: 'Reliability targets and error budgets',
    guide: 'Check reliability targets, remaining error budgets and how quickly failures '
      + 'consume them. Missing evidence appears as unknown.' },
  { id: 'trust', path: '/trust', title: 'Trust', navGroup: 'operate', testId: 'nav-trust',
    icon: ShieldCheck, hint: 'What pg_sage may do, and why',
    guide: 'See what pg_sage is allowed to do and the evidence behind each autonomy '
      + 'level. Review proposed increases before granting more control.' },
  { id: 'facts', path: '/facts', title: 'Facts', navGroup: 'operate', testId: 'nav-facts',
    icon: BookCheck, hint: 'Context pg_sage cannot discover',
    guide: 'Record context pg_sage cannot discover, such as application-owned indexes. '
      + 'Proposed facts need confirmation before they influence decisions.' },
  { id: 'ask', path: '/ask', title: 'Ask Sage', navGroup: 'operate', testId: 'nav-ask',
    icon: MessageSquare, hint: 'Questions answered with cited evidence',
    guide: 'Ask questions about the selected database and inspect the cited evidence. '
      + 'Answers never execute or approve database changes.' },
  { id: 'databases', path: '/manage-databases', title: 'Databases', label: 'Fleet',
    navGroup: 'admin', testId: 'nav-databases', icon: Server, roles: ADMIN,
    hint: 'Manage database connections',
    guide: 'See the databases pg_sage monitors and their connection health. Open a '
      + 'database to inspect its collected snapshot.' },
  { id: 'users', path: '/users', title: 'Users', navGroup: 'admin', testId: 'nav-users',
    icon: Users, roles: ADMIN, hint: 'Who can use pg_sage, and with which role',
    guide: 'Manage who can use pg_sage and what each role can do. App access and '
      + 'database permissions are separate.' },
  { id: 'notifications', path: '/notifications', title: 'Notifications', navGroup: 'admin',
    testId: 'nav-notifications', icon: Bell, roles: REVIEWERS,
    hint: 'Alert channels, rules and delivery history',
    guide: 'Inspect delivery history for the selected database to see what was sent or '
      + 'failed. Administrators also configure the delivery channels and rules.' },
  // The Alert log is folded into Notifications (alerting-06c): /alerts
  // redirects to its Log tab, keeping ?database=.
  { id: 'alerts', path: '/alerts', title: 'Alerts', label: 'Alert log', guideTitle: 'Alerts',
    navGroup: 'admin', testId: 'nav-alerts', icon: ScrollText, roles: REVIEWERS,
    hint: 'Delivery history in Notifications',
    guide: 'The Alert log moved to Notifications · Log, which lists every delivery with '
      + 'its database, status and error.' },
  { id: 'settings', path: '/settings', title: 'Settings', navGroup: 'admin',
    testId: 'nav-settings', icon: Settings, roles: ADMIN, hint: 'Configure pg_sage',
    guide: 'Inspect monitoring, AI and safety settings. Scope labels show whether changes '
      + 'affect the pg_sage instance or one database.' },
  { id: 'mcp-tokens', path: '/mcp-tokens', title: 'MCP tokens', navGroup: 'admin',
    testId: 'nav-mcp-tokens', icon: KeyRound, roles: ADMIN, standaloneOnly: true,
    hint: 'Credentials for MCP clients', platformGuide: PROTOCOL_PLATFORM_GUIDE,
    guide: 'Manage credentials for external tools that connect through MCP. Token access '
      + 'is limited by its scope and the account’s permissions.' },
  { id: 'agents', path: '/agents', title: 'Agents', navGroup: 'admin', testId: 'nav-agents',
    icon: Bot, roles: REVIEWERS, standaloneOnly: true,
    hint: 'Registered agents and their permissions', platformGuide: PROTOCOL_PLATFORM_GUIDE,
    guide: 'Review registered agents, their permissions and activity. Access depends on '
      + 'your role and the deployment’s authentication mode.' },
  { id: 'runbooks', path: '/advanced/runbooks', title: 'Runbooks', navGroup: 'advanced',
    testId: 'nav-runbooks', hint: 'Signed investigation guides',
    guide: 'Review versioned investigation guides. An administrator must sign each version '
      + 'before use; runbooks propose changes without executing them.' },
  { id: 'autonomy', path: '/advanced/autonomy', title: 'Earned autonomy',
    navGroup: 'advanced', testId: 'nav-autonomy', hint: 'Evidence for more independence',
    guide: 'Review evidence for giving pg_sage more independence on specific tasks. '
      + 'Administrators approve proposed increases in autonomy.' },
  { id: 'snapshot', path: '/advanced', title: 'Snapshot & metrics', navGroup: 'advanced',
    testId: 'nav-snapshot', hint: 'Collected health and performance metrics',
    usesTimeRange: true, timeRangeLabel: 'Health chart window',
    guide: 'Survey collected database health, performance metrics and recent '
      + 'recommendations. Provider readiness shows supported capabilities and blockers.' },
  { id: 'database', path: '/database', title: 'Database',
    guide: 'Inspect the latest collected snapshot for one database, including the '
      + 'measurements and raw details used for analysis.' },
  { id: 'query', path: '/query', title: 'Query',
    guide: 'Inspect one statement: its history from the query store, plan changes, the '
      + 'newest captured plan, and the cases, hints and actions about it.' },
  caseView('forecasts', '/forecasts', 'Forecasts', 'Review cases based on forecasts, '
    + 'including predicted capacity risks and the evidence behind them.'),
  caseView('query-hints', '/query-hints', 'Query hints', 'Review cases about query-plan '
    + 'hints and their evidence. Provider support determines which proposals are available.'),
  caseView('schema-health', '/schema-health', 'Schema health', 'Review cases from schema '
    + 'checks, such as missing or duplicate indexes, and inspect the supporting evidence.'),
  caseView('incidents', '/incidents', 'Incidents', 'Review incident investigations, '
    + 'collected evidence and proposed next steps. Open a case to follow its progress.'),
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

export const routeLabel = route => route.label || route.title
export const routeGuideTitle = route => route.guideTitle || routeLabel(route)
export const routeMemeLabel = route => route.memeLabel || routeLabel(route)
export const routeHref = route => `#${route.path}`

// normalizePath turns '#/cases?case=1', '/cases' or '' into '/cases' or '/'.
function normalizePath(route) {
  const path = String(route ?? '').replace(/^#/, '').split('?')[0]
  return path || '/'
}

// findRoute returns the route whose path or alias is exactly the given hash
// path, ignoring any query. Signed-out pages match only when asked for.
export function findRoute(route, { includeSignedOut = false } = {}) {
  const path = normalizePath(route)
  return ROUTES.find(r => (includeSignedOut || !r.signedOutOnly)
    && (r.path === path || r.aliases?.includes(path)))
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
