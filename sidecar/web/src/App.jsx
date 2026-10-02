import { useState, useEffect } from 'react'
import { Layout } from './components/Layout'
import { Dashboard } from './pages/Dashboard'
import { CasesPage } from './pages/CasesPage'
import { Actions } from './pages/Actions'
import { DatabasePage } from './pages/DatabasePage'
import { SettingsPage } from './pages/SettingsPage'
import { AlertLogPage } from './pages/AlertLogPage'
import { LoginPage } from './pages/LoginPage'
import { UsersPage } from './pages/UsersPage'
import { ProfilePage } from './pages/ProfilePage'
import { LinkSSOPage } from './pages/LinkSSOPage'
import { NotificationsPage } from './pages/NotificationsPage'
import { AgentDBsPage } from './pages/AgentDBsPage'
import { DatabasesPage } from './pages/DatabasesPage'
import { ValuePage } from './pages/ValuePage'
import { SLOsPage } from './pages/SLOsPage'
import { RunbooksPage } from './pages/RunbooksPage'
import { useAPI } from './hooks/useAPI'
import { resolveSelectedDB } from './lib/selectedDatabase'
import { TimeRangeProvider } from './context/TimeRangeContext'
import { CommandPalette } from './components/CommandPalette'
import { LiveEventsProvider } from './hooks/useLiveEvents'
import { ToastProvider } from './components/Toast'

function getRoute() {
  const hash = window.location.hash || '#/'
  return hash.replace('#', '') || '/'
}

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
        Go to Dashboard
      </a>
    </div>
  )
}

export default function App() {
  const [route, setRoute] = useState(getRoute())
  const [selectedDB, setSelectedDB] = useState(
    localStorage.getItem('pg_sage_db') || 'all'
  )
  const [user, setUser] = useState(null)
  const [authChecked, setAuthChecked] = useState(false)

  useEffect(() => {
    fetch('/api/v1/auth/me', { credentials: 'include' })
      .then(res => {
        if (res.ok) return res.json()
        throw new Error('not authenticated')
      })
      .then(data => setUser(data))
      .catch(() => setUser(null))
      .finally(() => setAuthChecked(true))
  }, [])

  const { data: fleetData, refetch: refetchFleet } = useAPI(
    user ? '/api/v1/databases' : null, 30000
  )

  useEffect(() => {
    const handler = () => setRoute(getRoute())
    window.addEventListener('hashchange', handler)
    return () => window.removeEventListener('hashchange', handler)
  }, [])

  // Return to the login screen when any polling request reports the
  // session has expired, instead of leaving stale pages showing 401s.
  useEffect(() => {
    const onExpired = () => setUser(null)
    window.addEventListener('sage:auth-expired', onExpired)
    return () => window.removeEventListener('sage:auth-expired', onExpired)
  }, [])

  useEffect(() => {
    localStorage.setItem('pg_sage_db', selectedDB)
  }, [selectedDB])

  if (!authChecked) return null

  // Hash routes may carry a query (e.g. #/profile?linked=1); match on path.
  const routePath = route.split('?')[0]

  if (!user) {
    // An admin-issued SSO link is used before the holder can sign in (D7).
    if (routePath === '/link-sso') return <LinkSSOPage />
    return <LoginPage onLogin={setUser} />
  }

  async function handleLogout() {
    // requireJSONMiddleware rejects POSTs without Content-Type:
    // application/json with 415 (see internal/api/middleware.go:97).
    // The body-less logout still needs the header.
    await fetch('/api/v1/auth/logout', {
      method: 'POST',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
    })
    setUser(null)
  }

  const databases = fleetData?.databases || []
  // A stored selection for a deleted or renamed database would hide the
  // picker and 404 every page; fall back to all databases (G9-B09).
  const effectiveDB = resolveSelectedDB(selectedDB, fleetData?.databases)
  const selectedDatabase = effectiveDB === 'all'
    ? null
    : databases.find(db => db.name === effectiveDB)

  // TODO(fleet-ctx): Audit every fetch() call site and ensure
  // per-database context is forwarded via ?database= where the
  // endpoint accepts it. Current direct fetch() call sites:
  //   - Actions.jsx approve/reject: id-scoped, no ?database= needed
  //   - Findings.jsx suppress/execute: id-scoped, no ?database=
  //   - ChannelsTab, RulesTab, LogTab: global notifications config
  //   - DatabaseTable/DatabaseForm: address the managed DB by id
  //   - SettingsPage config/global, emergency-stop, resume: pass dbParam
  //   - UsersPage, LoginPage, DatabasesPage: global resources
  // Page-level useAPI calls already pass dbParam where applicable
  // (Findings, Actions, AlertLog, Dashboard, DatabasePage, etc).
  // Revisit when new per-DB endpoints are added.

  const isAdmin = user.role === 'admin'
  const denied = { title: 'Access denied', node: <AccessDenied /> }
  const pageState = (() => {
    switch (routePath) {
      case '/':
        return { title: 'Value', node: <ValuePage database={effectiveDB} /> }
      case '/advanced':
        return { title: 'Snapshot & metrics',
          node: <Dashboard database={effectiveDB}
            onSelectDB={setSelectedDB} user={user} /> }
      case '/advanced/findings':
        return { title: 'Findings explorer',
          node: <CasesPage key={route} database={effectiveDB} user={user} /> }
      case '/advanced/actions':
        return { title: 'Action history',
          node: <Actions database={effectiveDB} user={user} /> }
      case '/advanced/runbooks':
        return { title: 'Runbooks',
          node: <RunbooksPage database={effectiveDB} user={user} /> }
      case '/manage-databases':
        return isAdmin ? { title: 'Databases', node: <DatabasesPage /> }
          : denied
      case '/agent-dbs':
        return { title: 'Agent DBs', node: <AgentDBsPage /> }
      case '/findings':
      case '/cases':
        return { title: 'Cases',
          node: <CasesPage key={route} database={effectiveDB} user={user} /> }
      case '/actions':
        return { title: 'Actions',
          node: <Actions database={effectiveDB} user={user} /> }
      case '/database':
        return { title: 'Database',
          node: <DatabasePage database={effectiveDB} /> }
      case '/forecasts':
        return { title: 'Cases',
          node: <CasesPage key={route} database={effectiveDB} user={user}
            initialSource="forecast" /> }
      case '/query-hints':
        return { title: 'Cases',
          node: <CasesPage key={route} database={effectiveDB} user={user}
            initialSource="query_hint" /> }
      case '/schema-health':
        return { title: 'Cases',
          node: <CasesPage key={route} database={effectiveDB} user={user}
            initialSource="schema_health" /> }
      case '/alerts':
        return { title: 'Alerts',
          node: <AlertLogPage database={effectiveDB} /> }
      case '/slos':
        return { title: 'SLOs', node: <SLOsPage database={effectiveDB} /> }
      case '/incidents':
        return { title: 'Cases',
          node: <CasesPage key={route} database={effectiveDB} user={user}
            initialSource="incident" /> }
      case '/settings':
        return isAdmin ? { title: 'Settings',
          node: <SettingsPage
            database={effectiveDB}
            databaseId={selectedDatabase?.id || selectedDatabase?.database_id}
          /> } : denied
      case '/notifications':
        return isAdmin ? { title: 'Notifications',
          node: <NotificationsPage /> } : denied
      // A signed-in user can land on /login from an SSO callback error; the
      // account page explains the sso_error (D7).
      case '/login':
      case '/profile':
        return { title: 'Account', node: <ProfilePage user={user} /> }
      case '/users':
        return isAdmin ? { title: 'Users',
          node: <UsersPage currentUser={user} /> } : denied
      default:
        return { title: 'Not found', node: <NotFound /> }
    }
  })()

  return (
    <LiveEventsProvider>
      <ToastProvider>
        <TimeRangeProvider>
          <Layout data-testid="app-loaded" databases={databases}
            selectedDB={effectiveDB}
            onSelectDB={setSelectedDB} user={user}
            fleetData={fleetData}
            onFleetChanged={refetchFleet}
            onLogout={handleLogout}
            pageTitle={pageState.title}>
            {pageState.node}
          </Layout>
          <CommandPalette
            databases={databases}
            selectedDB={effectiveDB}
            onSelectDB={setSelectedDB}
            user={user}
          />
        </TimeRangeProvider>
      </ToastProvider>
    </LiveEventsProvider>
  )
}
