import { createContext, useContext } from 'react'
import {
  AlertTriangle, Activity, Bell, Settings,
  Home, TrendingUp, Zap, Users, LogOut, Mail, Server,
  ShieldAlert, Search,
} from 'lucide-react'
import { DatabasePicker } from './DatabasePicker'
import { useAPI } from '../hooks/useAPI'

const PendingActionsContext = createContext({ refetch: () => {} })

export function usePendingActionsRefetch() {
  return useContext(PendingActionsContext).refetch
}

const NAV_GROUPS = [
  {
    heading: 'Monitor',
    items: [
      { path: '#/', icon: Home, label: 'Dashboard',
        tid: 'nav-dashboard' },
      { path: '#/findings', icon: AlertTriangle,
        label: 'Recommendations', tid: 'nav-findings' },
      { path: '#/actions', icon: Activity, label: 'Actions',
        tid: 'nav-actions' },
      { path: '#/incidents', icon: ShieldAlert,
        label: 'Incidents', tid: 'nav-incidents' },
    ],
  },
  {
    heading: 'Analyze',
    items: [
      { path: '#/forecasts', icon: TrendingUp,
        label: 'Forecasts', tid: 'nav-forecasts' },
      { path: '#/query-hints', icon: Zap, label: 'Performance',
        tid: 'nav-query-hints' },
      { path: '#/schema-health', icon: Search,
        label: 'Schema Health', tid: 'nav-schema-health' },
      { path: '#/alerts', icon: Bell, label: 'Alerts',
        tid: 'nav-alerts' },
    ],
  },
  {
    heading: 'Configure',
    items: [
      { path: '#/settings', icon: Settings, label: 'Settings',
        tid: 'nav-settings' },
      { path: '#/manage-databases', icon: Server,
        label: 'Databases', admin: true,
        tid: 'nav-databases' },
      { path: '#/notifications', icon: Mail,
        label: 'Notifications', admin: true,
        tid: 'nav-notifications' },
      { path: '#/users', icon: Users, label: 'Users',
        admin: true, tid: 'nav-users' },
    ],
  },
]

/* Flat list of all nav items for header label lookup */
const ALL_NAV_ITEMS = NAV_GROUPS.flatMap(g => g.items)

function NavHeading({ children }) {
  return (
    <div
      className="px-3 mt-4 mb-1"
      style={{
        fontSize: '10px',
        textTransform: 'uppercase',
        letterSpacing: '0.08em',
        color: 'var(--text-secondary)',
      }}>
      {children}
    </div>
  )
}

function NavLink({ item, active, pendingCount }) {
  return (
    <a
      href={item.path}
      data-testid={item.tid}
      className="flex items-center gap-2 px-3 py-2 rounded text-sm"
      style={{
        color: active
          ? 'var(--accent)' : 'var(--text-secondary)',
        background: active
          ? 'var(--bg-hover)' : 'transparent',
      }}>
      <item.icon size={16} />
      {item.label}
      {item.path === '#/actions' && pendingCount > 0 && (
        <span
          className="ml-auto px-1.5 py-0.5 rounded-full text-xs"
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
  )
}

function EmergencyBadge() {
  return (
    <span
      data-testid="emergency-stop-badge"
      className="flex items-center gap-1.5 px-2.5 py-1 rounded text-xs font-semibold"
      style={{
        background: 'var(--red, #e53e3e)',
        color: '#fff',
        animation: 'pulse 2s cubic-bezier(0.4, 0, 0.6, 1) infinite',
      }}>
      <ShieldAlert size={14} />
      EMERGENCY STOP ACTIVE
    </span>
  )
}

export function Layout({
  children, databases, selectedDB, onSelectDB,
  user, onLogout, ...rest
}) {
  const hash = window.location.hash || '#/'
  const { data: countData, refetch: refetchPending } = useAPI(
    user ? '/api/v1/actions/pending/count' : null, 30000,
  )
  const pendingCount = countData?.count || 0

  const { data: fleetData } = useAPI(
    user ? '/api/v1/databases' : null, 30000,
  )
  const emergencyStopped =
    fleetData?.summary?.emergency_stopped === true

  const isAdmin = user?.role === 'admin'

  return (
    <div className="flex h-screen" {...rest}>
      <nav
        className="w-56 flex-shrink-0 border-r flex flex-col p-4 gap-1"
        style={{
          background: 'var(--bg-card)',
          borderColor: 'var(--border)',
        }}>
        <div
          className="text-lg font-bold mb-4"
          style={{ color: 'var(--accent)' }}>
          pg_sage
        </div>

        {NAV_GROUPS.map(group => {
          const visible = group.items.filter(
            n => !n.admin || isAdmin,
          )
          if (visible.length === 0) return null
          return (
            <div key={group.heading}>
              <NavHeading>{group.heading}</NavHeading>
              {visible.map(n => (
                <NavLink
                  key={n.path}
                  item={n}
                  active={hash === n.path}
                  pendingCount={pendingCount}
                />
              ))}
            </div>
          )
        })}

        <div
          className="mt-auto pt-4"
          style={{ borderTop: '1px solid var(--border)' }}>
          {user && (
            <div
              className="px-3 py-1 text-xs mb-2"
              data-testid="user-email"
              style={{ color: 'var(--text-secondary)' }}>
              {user.email}
              <span className="ml-1 opacity-60">
                ({user.role})
              </span>
            </div>
          )}
          {onLogout && (
            <button
              onClick={onLogout}
              data-testid="sign-out-button"
              className="flex items-center gap-2 px-3 py-2 rounded text-sm w-full"
              style={{ color: 'var(--text-secondary)' }}>
              <LogOut size={16} />
              Sign Out
            </button>
          )}
        </div>
      </nav>

      <main className="flex-1 overflow-auto">
        <header
          className="flex items-center justify-between p-4 border-b"
          style={{ borderColor: 'var(--border)' }}>
          <h1
            className="text-lg font-semibold"
            style={{ color: 'var(--text-primary)' }}>
            {ALL_NAV_ITEMS.find(n => n.path === hash)?.label
              || 'pg_sage'}
          </h1>
          <div className="flex items-center gap-3">
            {emergencyStopped && <EmergencyBadge />}
            {databases && databases.length > 1 && (
              <DatabasePicker
                data-testid="database-picker"
                databases={databases}
                selected={selectedDB}
                onSelect={onSelectDB}
              />
            )}
          </div>
        </header>
        <div className="p-6">
          <PendingActionsContext.Provider
            value={{ refetch: refetchPending }}>
            {children}
          </PendingActionsContext.Provider>
        </div>
      </main>
    </div>
  )
}
