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
import { DatabasesPage } from './pages/DatabasesPage'
import { ValuePage } from './pages/ValuePage'
import { OnboardingPanel } from './components/onboarding/OnboardingPanel'
import { FirstLookPanel } from './components/onboarding/FirstLookPanel'
import { SLOsPage } from './pages/SLOsPage'
import { RunbooksPage } from './pages/RunbooksPage'
import { AutonomyPage } from './pages/AutonomyPage'
import { TrustPage } from './pages/TrustPage'
import { FactsPage } from './pages/FactsPage'
import { AskPage } from './pages/AskPage'
import { MCPTokensPage } from './pages/MCPTokensPage'
import { AgentsPage } from './pages/AgentsPage'
import { useAPI } from './hooks/useAPI'
import { resolveSelectedDB } from './lib/selectedDatabase'
import { TimeRangeProvider } from './context/TimeRangeContext'
import { CommandPalette } from './components/CommandPalette'
import { LiveEventsProvider } from './hooks/useLiveEvents'
import { ToastProvider } from './components/Toast'
import { PostgresMeme } from './components/PostgresMeme'

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

function PlatformSignInError({ mode, onReload }) {
  return (
    <div role="alert" data-testid="platform-auth-error" className="p-6">
      <h1 className="text-lg font-semibold mb-2">
        {mode === 'databricks_app' ? 'Databricks sign-in required' : 'Unable to verify sign-in'}
      </h1>
      <p className="mb-4">
        {mode === 'databricks_app'
          ? 'Open this app from your Databricks workspace and check that you have access. Reload to try again.'
          : 'Sign-in could not be verified. Reload to try again.'}
      </p>
      <button type="button" onClick={onReload} className="px-4 py-2 rounded"
        style={{ background: 'var(--accent)', color: '#fff' }}>
        Reload
      </button>
      <PostgresMeme route="#/login" compact />
    </div>
  )
}

function PlatformProtocolUnavailable({ agents = false }) {
  return (
    <div data-testid={agents ? 'databricks-agents-unavailable' : 'databricks-mcp-unavailable'} className="rounded p-5"
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
        Go to Dashboard
      </a>
    </div>
  )
}

export default function App({ reloadPage = () => window.location.reload() }) {
  const [route, setRoute] = useState(getRoute())
  const [selectedDB, setSelectedDB] = useState(
    localStorage.getItem('pg_sage_db') || 'all'
  )
  const [user, setUser] = useState(null)
  const [authChecked, setAuthChecked] = useState(false)
  const [authMode, setAuthMode] = useState(null)
  const [authAttempt, setAuthAttempt] = useState(0)

  useEffect(() => {
    let cancelled = false
    setAuthChecked(false)
    async function checkAuth() {
      let mode = null
      // The public mode endpoint lets an identity failure keep the platform
      // sign-in flow. Browser-supplied identity headers never choose the mode.
      try {
        const response = await fetch('/api/v1/auth/mode', { credentials: 'include' })
        if (response.ok) {
          const data = await response.json()
          if (data.mode === 'session' || data.mode === 'databricks_app') mode = data.mode
        }
      } catch { /* An unknown mode fails closed if identity also fails. */ }
      try {
        const response = await fetch('/api/v1/auth/me', { credentials: 'include' })
        if (!response.ok) throw new Error('not authenticated')
        const data = await response.json()
        if (data.auth_mode === 'databricks_app') mode = 'databricks_app'
        if (!cancelled) setUser(data)
        // An authenticated legacy standalone response has no auth_mode.
        if (mode === null) mode = 'session'
      } catch {
        if (!cancelled) setUser(null)
      } finally {
        if (!cancelled) {
          setAuthMode(mode)
          setAuthChecked(true)
        }
      }
    }
    checkAuth()
    return () => { cancelled = true }
  }, [authAttempt])

  const { data: fleetData, loading: fleetLoading, error: fleetError,
    refetch: refetchFleet } = useAPI(
    user ? '/api/v1/databases' : null, 30000
  )

  useEffect(() => {
    const linked = new URLSearchParams(route.split('?')[1] || '').get('database')
    if (linked && fleetData?.databases?.some(db => db.name === linked)) setSelectedDB(linked)
  }, [route, fleetData])

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
  const platformManaged = authMode === 'databricks_app'

  if (!user) {
    if (authMode !== 'session') return (
      <PlatformSignInError mode={authMode}
        onReload={platformManaged
          // A full navigation lets the Apps ingress renew an expired platform
          // session; retrying fetch alone can remain stuck behind CORS redirects.
          ? reloadPage
          : () => setAuthAttempt(attempt => attempt + 1)} />
    )
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

  // Go may encode an empty slice as null; keep that distinct from an inventory
  // that has not loaded yet so a stale stored scope cannot open a missing DB.
  const databaseInventory = fleetData?.databases === null ? []
    : Array.isArray(fleetData?.databases)
      && fleetData.databases.every(db => typeof db?.name === 'string' && db.name)
      ? fleetData.databases : undefined
  const databases = databaseInventory || []
  const inventoryError = fleetError || (fleetData && databaseInventory === undefined
    ? new Error('Invalid database inventory response') : null)
  const routeParams = new URLSearchParams(route.split('?')[1] || '')
  const linkedDatabase = routeParams.get('database')
  const linkedCase = routeParams.get('case')
  // Only inventory-known links may change scope. Resolve it before mounting
  // a page so a case never opens an Ask session against the previous DB.
  const effectiveDB = resolveSelectedDB(
    databases.some(db => db.name === linkedDatabase) ? linkedDatabase : selectedDB,
    databaseInventory,
  )
  function selectDatabase(name) {
    setSelectedDB(name)
    const params = new URLSearchParams(route.split('?')[1] || '')
    params.delete('database')
    params.delete('case')
    const query = params.toString()
    if (linkedDatabase || linkedCase) window.location.hash = `#${routePath}${query ? `?${query}` : ''}`
  }
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
        return { title: 'Value', node: (
          <div className="space-y-6">
            <OnboardingPanel database={effectiveDB} authMode={authMode} />
            <FirstLookPanel database={effectiveDB} />
            <ValuePage database={effectiveDB} />
          </div>
        ) }
      case '/advanced':
        return { title: 'Snapshot & metrics',
          node: <Dashboard database={effectiveDB}
            onSelectDB={setSelectedDB} user={user} /> }
      case '/advanced/findings':
        return { title: 'Findings explorer',
          node: <CasesPage key={route} database={effectiveDB} user={user} /> }
      case '/advanced/actions':
        return { title: 'Action history',
          node: <Actions database={effectiveDB} user={user} fleetData={fleetData} /> }
      case '/advanced/runbooks':
        return { title: 'Runbooks',
          node: <RunbooksPage database={effectiveDB} user={user} /> }
      case '/trust':
        return { title: 'Trust',
          node: <TrustPage database={effectiveDB} user={user} /> }
      case '/facts':
        return { title: 'Facts',
          node: <FactsPage database={effectiveDB} user={user} /> }
      case '/ask':
        return { title: 'Ask Sage',
          node: <AskPage database={effectiveDB} user={user} caseId={linkedCase}
            databases={databaseInventory} databaseLoading={fleetLoading}
            databaseError={inventoryError} onRetryDatabase={refetchFleet}
            onSelectDatabase={selectDatabase} /> }
      case '/advanced/autonomy':
        return { title: 'Earned autonomy',
          node: <AutonomyPage database={effectiveDB} user={user} /> }
      case '/manage-databases':
        return isAdmin ? { title: 'Databases', node: <DatabasesPage onSelectDatabase={setSelectedDB} /> }
          : denied
      case '/findings':
      case '/cases':
        return { title: 'Cases',
          node: <CasesPage key={route} database={effectiveDB} user={user} /> }
      case '/actions':
        return { title: 'Actions',
          node: <Actions database={effectiveDB} user={user} fleetData={fleetData} /> }
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
        return { title: 'Account', node: <ProfilePage user={user} authMode={authMode} /> }
      case '/users':
        return isAdmin ? { title: 'Users',
          node: <UsersPage currentUser={user} authMode={authMode} /> } : denied
      case '/agents':
        if (platformManaged) return { title: 'Agents', node: <PlatformProtocolUnavailable agents /> }
        return isAdmin || user.role === 'operator' ? { title: 'Agents',
          node: <AgentsPage user={user}
            databases={databases.map(d => d.name)} /> } : denied
      case '/mcp-tokens':
        if (platformManaged) return { title: 'MCP tokens', node: <PlatformProtocolUnavailable /> }
        return isAdmin ? { title: 'MCP tokens', node: <MCPTokensPage /> }
          : denied
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
            onSelectDB={selectDatabase} user={user} authMode={authMode}
            fleetError={inventoryError}
            fleetData={fleetData ? { ...fleetData, databases } : fleetData}
            onFleetChanged={refetchFleet}
            onLogout={platformManaged ? undefined : handleLogout}
            pageTitle={pageState.title}>
            {pageState.node}
          </Layout>
          <CommandPalette
            databases={databases}
            selectedDB={effectiveDB}
            onSelectDB={selectDatabase}
            user={user}
          />
        </TimeRangeProvider>
      </ToastProvider>
    </LiveEventsProvider>
  )
}
