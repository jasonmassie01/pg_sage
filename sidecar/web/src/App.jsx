import { useState, useEffect } from 'react'
import { Layout } from './components/Layout'
import { Dashboard } from './pages/Dashboard'
import { Findings } from './pages/Findings'
import { Actions } from './pages/Actions'
import { DatabasePage } from './pages/DatabasePage'
import { SettingsPage } from './pages/SettingsPage'
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
  const { data: fleetData } = useAPI('/api/v1/databases', 30000)

  useEffect(() => {
    const handler = () => setRoute(getRoute())
    window.addEventListener('hashchange', handler)
    return () => window.removeEventListener('hashchange', handler)
  }, [])

  useEffect(() => {
    localStorage.setItem('pg_sage_db', selectedDB)
  }, [selectedDB])

  const databases = fleetData?.databases || []

  const page = (() => {
    switch (route) {
      case '/findings': return <Findings database={selectedDB} />
      case '/actions': return <Actions database={selectedDB} />
      case '/database': return <DatabasePage database={selectedDB} />
      case '/settings': return <SettingsPage database={selectedDB} />
      default: return <Dashboard database={selectedDB} />
    }
  })()

  return (
    <Layout databases={databases} selectedDB={selectedDB}
      onSelectDB={setSelectedDB}>
      {page}
    </Layout>
  )
}
