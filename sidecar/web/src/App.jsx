import { useState, useEffect } from 'react'
import { Layout } from './components/Layout'
import { Dashboard } from './pages/Dashboard'
import { Findings } from './pages/Findings'
import { Actions } from './pages/Actions'
import { DatabasePage } from './pages/DatabasePage'
import { SettingsPage } from './pages/SettingsPage'
import { ForecastsPage } from './pages/ForecastsPage'
import { QueryHintsPage } from './pages/QueryHintsPage'
import { AlertLogPage } from './pages/AlertLogPage'
import { IncidentsPage } from './pages/IncidentsPage'
import { LoginPage } from './pages/LoginPage'
import { UsersPage } from './pages/UsersPage'
import { NotificationsPage } from './pages/NotificationsPage'
import { DatabasesPage } from './pages/DatabasesPage'
import { SchemaHealthPage } from './pages/SchemaHealthPage'
import { useAPI } from './hooks/useAPI'

function getRoute() {
  const hash = window.location.hash || '#/'
  return hash.replace('#', '') || '/'
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

  const { data: fleetData } = useAPI(
    user ? '/api/v1/databases' : null, 30000
  )

  useEffect(() => {
    const handler = () => setRoute(getRoute())
    window.addEventListener('hashchange', handler)
    return () => window.removeEventListener('hashchange', handler)
  }, [])

  useEffect(() => {
    localStorage.setItem('pg_sage_db', selectedDB)
  }, [selectedDB])

  if (!authChecked) return null

  if (!user) {
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

  const page = (() => {
    switch (route) {
      case '/manage-databases':
        return user.role === 'admin'
          ? <DatabasesPage />
          : <Dashboard database={selectedDB} />
      case '/findings': return <Findings database={selectedDB} user={user} />
      case '/actions': return <Actions database={selectedDB} user={user} />
      case '/database': return <DatabasePage database={selectedDB} />
      case '/forecasts':
        return <ForecastsPage database={selectedDB} />
      case '/query-hints':
        return <QueryHintsPage database={selectedDB} />
      case '/schema-health':
        return <SchemaHealthPage database={selectedDB} />
      case '/alerts': return <AlertLogPage database={selectedDB} />
      case '/incidents':
        return <IncidentsPage database={selectedDB} user={user} />
      case '/settings': return <SettingsPage database={selectedDB} />
      case '/notifications':
        return user.role === 'admin'
          ? <NotificationsPage />
          : <Dashboard database={selectedDB} />
      case '/users':
        return user.role === 'admin'
          ? <UsersPage />
          : <Dashboard database={selectedDB} />
      default: return <Dashboard database={selectedDB} />
    }
  })()

  return (
    <Layout data-testid="app-loaded" databases={databases}
      selectedDB={selectedDB}
      onSelectDB={setSelectedDB} user={user}
      onLogout={handleLogout}>
      {page}
    </Layout>
  )
}
