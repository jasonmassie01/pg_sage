import { useState, useEffect, useCallback } from 'react'
import { Layout } from './components/Layout'
import { LoginPage } from './pages/LoginPage'
import { LinkSSOPage } from './pages/LinkSSOPage'
import { useAPI } from './hooks/useAPI'
import { useTimeZoneMode } from './lib/time'
import { resolveSelectedDB } from './lib/selectedDatabase'
import { readPref, writePref } from './lib/storage'
import { TimeRangeProvider } from './context/TimeRangeContext'
import { CommandPalette } from './components/CommandPalette'
import { AuthStoreBanner, AuthStoreRetry } from './components/AuthStoreStatus'
import { isAuthStoreUnavailable } from './lib/authExpiry'
import { LiveEventsProvider } from './hooks/useLiveEvents'
import { ToastProvider } from './components/Toast'
import { PostgresMeme } from './components/PostgresMeme'
import { renderRoute } from './routeRender'
import { ForcedPasswordChange } from './pages/users/ForcedPasswordChange'

function getRoute() {
  const hash = window.location.hash || '#/'
  return hash.replace('#', '') || '/'
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

export default function App({ reloadPage = () => window.location.reload() }) {
  const [route, setRoute] = useState(getRoute())
  const [selectedDB, setSelectedDB] = useState(() => readPref('pg_sage_db', 'all'))
  // ux-05d: the header local/UTC choice re-renders the whole page, so every
  // time printed through formatTime follows it.
  useTimeZoneMode()
  const [user, setUser] = useState(null)
  const [authChecked, setAuthChecked] = useState(false)
  const [authMode, setAuthMode] = useState(null)
  const [authAttempt, setAuthAttempt] = useState(0)
  // apiauth-02: the session store could not answer; the session may be fine.
  const [authStoreDown, setAuthStoreDown] = useState(false)
  const [authDegraded, setAuthDegraded] = useState(false)
  const retryAuth = useCallback(() => setAuthAttempt(attempt => attempt + 1), [])

  useEffect(() => {
    let cancelled = false
    setAuthChecked(false)
    async function checkAuth() {
      let mode = null
      let storeDown = false
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
        if (response.status === 503) storeDown = await isAuthStoreUnavailable(response)
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
          setAuthStoreDown(storeDown)
          setAuthChecked(true)
        }
      }
    }
    checkAuth()
    return () => { cancelled = true }
  }, [authAttempt])

  // A session whose password must change can reach nothing else (apiauth-03).
  const appUser = user && !user.must_change_password ? user : null
  const fleetQuery = useAPI(appUser ? '/api/v1/databases' : null, 30000)
  const { data: fleetData, loading: fleetLoading, error: fleetError,
    refetch: refetchFleet, updatedAt: fleetUpdatedAt } = fleetQuery

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
    const onExpired = () => { setAuthDegraded(false); setUser(null) }
    window.addEventListener('sage:auth-expired', onExpired)
    return () => window.removeEventListener('sage:auth-expired', onExpired)
  }, [])

  // A session-store outage keeps the user and shows a banner until the
  // next successful API response (apiauth-02).
  useEffect(() => {
    const onDegraded = () => setAuthDegraded(true)
    const onRestored = () => setAuthDegraded(false)
    window.addEventListener('sage:auth-degraded', onDegraded)
    window.addEventListener('sage:auth-restored', onRestored)
    return () => {
      window.removeEventListener('sage:auth-degraded', onDegraded)
      window.removeEventListener('sage:auth-restored', onRestored)
    }
  }, [])

  useEffect(() => {
    writePref('pg_sage_db', selectedDB)
  }, [selectedDB])

  if (!authChecked) return null

  // Hash routes may carry a query (e.g. #/profile?linked=1); match on path.
  const routePath = route.split('?')[0]
  const platformManaged = authMode === 'databricks_app'

  if (!user) {
    // The store could not say whether the session is valid: retry, do not
    // send the operator to a login that fails against the same database.
    if (authStoreDown) return <AuthStoreRetry onRetry={retryAuth} />
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

  if (user.must_change_password) {
    return <ForcedPasswordChange user={user} onLogout={handleLogout}
      onChanged={() => setUser({ ...user, must_change_password: false })} />
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

  const pageState = renderRoute(routePath, {
    route, database: effectiveDB, user, authMode, fleetQuery, fleetData, fleetLoading,
    databases, databaseInventory, inventoryError, refetchFleet, selectDatabase,
    setSelectedDB, selectedDatabase, linkedCase,
  })

  return (
    <LiveEventsProvider>
      <ToastProvider>
        <TimeRangeProvider>
          {authDegraded && <AuthStoreBanner />}
          <Layout data-testid="app-loaded" databases={databases}
            selectedDB={effectiveDB}
            onSelectDB={selectDatabase} user={user} authMode={authMode}
            fleetError={inventoryError}
            fleetData={fleetData ? { ...fleetData, databases } : fleetData}
            fleetUpdatedAt={fleetUpdatedAt}
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
            authMode={authMode}
          />
        </TimeRangeProvider>
      </ToastProvider>
    </LiveEventsProvider>
  )
}
