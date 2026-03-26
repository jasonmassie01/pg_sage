import { Database, AlertTriangle, Activity, Settings, Home } from 'lucide-react'
import { DatabasePicker } from './DatabasePicker'

const NAV = [
  { path: '#/', icon: Home, label: 'Dashboard' },
  { path: '#/findings', icon: AlertTriangle, label: 'Findings' },
  { path: '#/actions', icon: Activity, label: 'Actions' },
  { path: '#/database', icon: Database, label: 'Database' },
  { path: '#/settings', icon: Settings, label: 'Settings' },
]

export function Layout({ children, databases, selectedDB, onSelectDB }) {
  const hash = window.location.hash || '#/'
  return (
    <div className="flex h-screen">
      <nav className="w-56 flex-shrink-0 border-r flex flex-col p-4 gap-1"
        style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
        <div className="text-lg font-bold mb-4"
          style={{ color: 'var(--accent)' }}>
          pg_sage
        </div>
        {NAV.map(n => (
          <a key={n.path} href={n.path}
            className="flex items-center gap-2 px-3 py-2 rounded text-sm"
            style={{
              color: hash === n.path
                ? 'var(--accent)' : 'var(--text-secondary)',
              background: hash === n.path
                ? 'var(--bg-hover)' : 'transparent',
            }}>
            <n.icon size={16} />
            {n.label}
          </a>
        ))}
      </nav>
      <main className="flex-1 overflow-auto">
        <header className="flex items-center justify-between p-4 border-b"
          style={{ borderColor: 'var(--border)' }}>
          <h1 className="text-lg font-semibold"
            style={{ color: 'var(--text-primary)' }}>
            {NAV.find(n => n.path === hash)?.label || 'pg_sage'}
          </h1>
          {databases && databases.length > 1 && (
            <DatabasePicker databases={databases}
              selected={selectedDB} onSelect={onSelectDB} />
          )}
        </header>
        <div className="p-6">{children}</div>
      </main>
    </div>
  )
}
