import {
  Database, AlertTriangle, Activity, Bell, Settings,
  Home, TrendingUp, Zap, Users, LogOut, Mail, Server,
} from 'lucide-react'
import { DatabasePicker } from './DatabasePicker'
import { useAPI } from '../hooks/useAPI'

const NAV = [
  { path: '#/', icon: Home, label: 'Dashboard' },
  { path: '#/manage-databases', icon: Server, label: 'Databases',
    admin: true },
  { path: '#/findings', icon: AlertTriangle, label: 'Findings' },
  { path: '#/actions', icon: Activity, label: 'Actions' },
  { path: '#/forecasts', icon: TrendingUp, label: 'Forecasts' },
  { path: '#/query-hints', icon: Zap, label: 'Query Hints' },
  { path: '#/alerts', icon: Bell, label: 'Alert Log' },
  { path: '#/database', icon: Database, label: 'Database' },
  { path: '#/settings', icon: Settings, label: 'Settings' },
]

export function Layout({
  children, databases, selectedDB, onSelectDB,
  user, onLogout,
}) {
  const hash = window.location.hash || '#/'
  const { data: countData } = useAPI(
    user ? '/api/v1/actions/pending/count' : null, 30000,
  )
  const pendingCount = countData?.count || 0

  const isAdmin = user?.role === 'admin'
  const baseNav = isAdmin
    ? NAV
    : NAV.filter(n => !n.admin)
  const navItems = isAdmin
    ? [...baseNav,
        { path: '#/notifications', icon: Mail,
          label: 'Notifications' },
        { path: '#/users', icon: Users, label: 'Users' }]
    : baseNav

  return (
    <div className="flex h-screen">
      <nav className="w-56 flex-shrink-0 border-r flex flex-col p-4 gap-1"
        style={{
          background: 'var(--bg-card)',
          borderColor: 'var(--border)',
        }}>
        <div className="text-lg font-bold mb-4"
          style={{ color: 'var(--accent)' }}>
          pg_sage
        </div>
        {navItems.map(n => (
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
            {n.path === '#/actions' && pendingCount > 0 && (
              <span className="ml-auto px-1.5 py-0.5 rounded-full text-xs"
                style={{
                  background: 'var(--red, #e53e3e)',
                  color: '#fff',
                  fontSize: '0.65rem',
                  lineHeight: 1,
                }}>
                {pendingCount}
              </span>
            )}
          </a>
        ))}
        <div className="mt-auto pt-4"
          style={{ borderTop: '1px solid var(--border)' }}>
          {user && (
            <div className="px-3 py-1 text-xs mb-2"
              style={{ color: 'var(--text-secondary)' }}>
              {user.email}
              <span className="ml-1 opacity-60">
                ({user.role})
              </span>
            </div>
          )}
          {onLogout && (
            <button onClick={onLogout}
              className="flex items-center gap-2 px-3 py-2 rounded text-sm w-full"
              style={{ color: 'var(--text-secondary)' }}>
              <LogOut size={16} />
              Sign Out
            </button>
          )}
        </div>
      </nav>
      <main className="flex-1 overflow-auto">
        <header className="flex items-center justify-between p-4 border-b"
          style={{ borderColor: 'var(--border)' }}>
          <h1 className="text-lg font-semibold"
            style={{ color: 'var(--text-primary)' }}>
            {navItems.find(n => n.path === hash)?.label
              || 'pg_sage'}
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
