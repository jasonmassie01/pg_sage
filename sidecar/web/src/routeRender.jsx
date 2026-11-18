/* eslint-disable react-refresh/only-export-components */
import { Dashboard } from './pages/Dashboard'
import { CasesPage } from './pages/CasesPage'
import { Actions } from './pages/Actions'
import { DatabasePage } from './pages/DatabasePage'
import { SettingsPage } from './pages/SettingsPage'
import { AlertsRedirect } from './pages/AlertsRedirect'
import { UsersPage } from './pages/UsersPage'
import { ProfilePage } from './pages/ProfilePage'
import { NotificationsPage } from './pages/NotificationsPage'
import { DatabasesPage } from './pages/DatabasesPage'
import { ValuePage } from './pages/ValuePage'
import { InboxPage } from './pages/InboxPage'
import { LedgerPage } from './pages/LedgerPage'
import { OnboardingPanel } from './components/onboarding/OnboardingPanel'
import { FirstLookPanel } from './components/onboarding/FirstLookPanel'
import { SLOsPage } from './pages/SLOsPage'
import { RunbooksPage } from './pages/RunbooksPage'
import { AutonomyHub } from './pages/autonomy/AutonomyHub'
import { FactsPage } from './pages/FactsPage'
import { AskPage } from './pages/AskPage'
import { MCPTokensPage } from './pages/MCPTokensPage'
import { AgentsPage } from './pages/AgentsPage'
import { QueriesPage } from './pages/QueriesPage'
import { QueryDetailPage } from './pages/QueryDetailPage'
import { allowsRole, findRoute } from './routes'

// routeRender.jsx renders the page for a route of the registry in routes.js
// (dashboard-06). RENDERERS has one entry per signed-in route id.

function AccessDenied() {
  return (
    <div data-testid="access-denied" className="rounded p-5"
      style={{
        background: 'var(--bg-card)',
        border: '1px solid var(--border)',
      }}>
      <h2 className="text-lg font-semibold mb-2"
        style={{ color: 'var(--text-primary)' }}>
        Access denied
      </h2>
      <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
        You do not have permission to open this admin page.
      </p>
    </div>
  )
}

function NotFound() {
  return (
    <div data-testid="not-found" className="rounded p-5"
      style={{
        background: 'var(--bg-card)',
        border: '1px solid var(--border)',
      }}>
      <h2 className="text-lg font-semibold mb-2"
        style={{ color: 'var(--text-primary)' }}>
        Page not found
      </h2>
      <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
        This pg_sage route does not exist.
      </p>
      <a href="#/" className="inline-block mt-4 text-sm underline"
        style={{ color: 'var(--accent)' }}>
        Go to Inbox
      </a>
    </div>
  )
}

function PlatformProtocolUnavailable({ agents = false }) {
  return (
    <div className="rounded p-5"
      data-testid={agents ? 'databricks-agents-unavailable' : 'databricks-mcp-unavailable'}
      style={{ background: 'var(--bg-card)', border: '1px solid var(--border)' }}>
      <h2 className="text-lg font-semibold mb-2" style={{ color: 'var(--text-primary)' }}>
        {agents ? 'Agent governance unavailable' : 'MCP tokens unavailable'}
      </h2>
      <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
        Agent governance, MCP connections and separate agent tokens are unavailable
        in this Databricks-managed deployment. Use Databricks sign-in to access the
        pg_sage dashboard and authenticated user APIs.
      </p>
      <a href="#/" className="inline-block mt-4 text-sm underline"
        style={{ color: 'var(--accent)' }}>
        Go to Inbox
      </a>
    </div>
  )
}

const cases = (ctx, initialSource) => (
  <CasesPage key={ctx.route} database={ctx.database} user={ctx.user}
    initialSource={initialSource} />
)
const actions = ctx => (
  <Actions database={ctx.database} user={ctx.user} fleetData={ctx.fleetData} />
)
const profile = ctx => <ProfilePage user={ctx.user} authMode={ctx.authMode} />

// ctx: route (the full hash route), database (the effective scope), user,
// authMode, fleetQuery, fleetData, databases, the Ask inventory fields,
// selectDatabase, setSelectedDB, selectedDatabase and linkedCase.
export const RENDERERS = Object.freeze({
  // Onboarding and first look stay above the Inbox until ux-04b's wizard.
  inbox: ctx => (
    <div className="space-y-6">
      <OnboardingPanel database={ctx.database} authMode={ctx.authMode} user={ctx.user} />
      <FirstLookPanel database={ctx.database} user={ctx.user} />
      <InboxPage key={ctx.database} database={ctx.database} user={ctx.user} />
    </div>
  ),
  value: ctx => <ValuePage database={ctx.database} />,
  cases: ctx => cases(ctx),
  actions,
  ledger: ctx => <LedgerPage key={ctx.route} database={ctx.database} />,
  queries: ctx => <QueriesPage database={ctx.database} />,
  query: ctx => <QueryDetailPage route={ctx.route} database={ctx.database} />,
  slos: ctx => <SLOsPage database={ctx.database} user={ctx.user} />,
  facts: ctx => <FactsPage database={ctx.database} user={ctx.user} />,
  ask: ctx => (
    <AskPage database={ctx.database} user={ctx.user} caseId={ctx.linkedCase}
      databases={ctx.databaseInventory} databaseLoading={ctx.fleetLoading}
      databaseError={ctx.inventoryError} onRetryDatabase={ctx.refetchFleet}
      onSelectDatabase={ctx.selectDatabase} />
  ),
  databases: ctx => <DatabasesPage onSelectDatabase={ctx.setSelectedDB} />,
  users: ctx => <UsersPage currentUser={ctx.user} authMode={ctx.authMode} />,
  notifications: ctx => {
    const tab = new URLSearchParams(ctx.route.split('?')[1] || '').get('tab') || ''
    return <NotificationsPage key={tab} user={ctx.user} database={ctx.database}
      initialTab={tab} />
  },
  alerts: ctx => <AlertsRedirect route={ctx.route} />,
  settings: ctx => (
    <SettingsPage database={ctx.database}
      databaseId={ctx.selectedDatabase?.id || ctx.selectedDatabase?.database_id} />
  ),
  'mcp-tokens': () => <MCPTokensPage />,
  agents: ctx => <AgentsPage user={ctx.user} databases={ctx.databases.map(d => d.name)} />,
  runbooks: ctx => <RunbooksPage database={ctx.database} user={ctx.user} />,
  autonomy: ctx => (
    <AutonomyHub key={ctx.route} database={ctx.database} user={ctx.user} route={ctx.route} />
  ),
  snapshot: ctx => (
    <Dashboard database={ctx.database} onSelectDB={ctx.setSelectedDB} user={ctx.user}
      fleet={ctx.fleetQuery} />
  ),
  database: ctx => <DatabasePage database={ctx.database} user={ctx.user} />,
  forecasts: ctx => cases(ctx, 'forecast'),
  'query-hints': ctx => cases(ctx, 'query_hint'),
  'schema-health': ctx => cases(ctx, 'schema_health'),
  incidents: ctx => cases(ctx, 'incident'),
  profile,
  login: profile,
})

// renderRoute returns the header title and page for a hash path. Unknown
// paths render Not found; a standalone-only page in a platform-managed
// deployment explains why it is unavailable; a role the route does not
// allow renders Access denied (the API enforces the same role).
export function renderRoute(path, ctx) {
  const route = findRoute(path)
  if (!route) return { title: 'Not found', node: <NotFound /> }
  if (route.standaloneOnly && ctx.authMode === 'databricks_app') {
    return { title: route.title,
      node: <PlatformProtocolUnavailable agents={route.id === 'agents'} /> }
  }
  if (!allowsRole(route, ctx.user)) return { title: 'Access denied', node: <AccessDenied /> }
  return { title: route.title, node: RENDERERS[route.id](ctx) }
}
