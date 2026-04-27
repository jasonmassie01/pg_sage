import { createContext, useContext, useState, useEffect } from 'react'
import {
  AlertTriangle, Activity, Settings,
  Home, LogOut, Server, ShieldAlert, Menu, X,
} from 'lucide-react'
import { DatabasePicker } from './DatabasePicker'
import { useAPI } from '../hooks/useAPI'
import { useLiveRefetch } from '../hooks/useLiveEvents'
import { TimeRangePicker } from './TimeRangePicker'
import { TrustBadge } from './TrustBadge'

const PendingActionsContext = createContext({ refetch: () => {} })

export function usePendingActionsRefetch() {
  return useContext(PendingActionsContext).refetch
}

const NAV_GROUPS = [
  {
    heading: 'Operate',
    items: [
      { path: '#/', icon: Home, label: 'Overview',
        tid: 'nav-dashboard' },
      { path: '#/cases', icon: AlertTriangle,
        label: 'Cases', tid: 'nav-cases',
        aliases: ['#/findings'] },
      { path: '#/actions', icon: Activity, label: 'Actions',
        tid: 'nav-actions' },
      { path: '#/manage-databases', icon: Server,
        label: 'Fleet', admin: true,
        tid: 'nav-databases' },
      { path: '#/settings', icon: Settings, label: 'Settings',
        admin: true, tid: 'nav-settings' },
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
  user, fleetData: fleetDataProp, onLogout, pageTitle, ...rest
}) {
  const hash = window.location.hash || '#/'
  const canReviewActions =
    user?.role === 'admin' || user?.role === 'operator'
  const { data: countData, refetch: refetchPending } = useAPI(
    canReviewActions ? '/api/v1/actions/pending/count' : null, 30000,
  )
  useLiveRefetch(['actions'], refetchPending)
  const pendingCount = countData?.count || 0

  // Reuse the fleet data App.jsx already fetches to avoid a
  // duplicate 30s poll. Fall back to our own useAPI when the
  // prop is absent (kept for older callers/tests).
  const { data: fleetDataOwn } = useAPI(
    user && !fleetDataProp ? '/api/v1/databases' : null, 30000,
  )
  const fleetData = fleetDataProp || fleetDataOwn
  const emergencyStopped =
    fleetData?.summary?.emergency_stopped === true

  // TrustBadge reflects the selected database's trust_level.
  // "all" collapses to the worst (least autonomous) across the
  // fleet so a single observation-mode instance still shows up
  // on the overview.
  const trustLevel = (() => {
    if (!fleetData?.databases?.length) return null
    if (selectedDB && selectedDB !== 'all') {
      const d = fleetData.databases.find(x => x.name === selectedDB)
      return d?.status?.trust_level || null
    }
    const order = { observation: 0, advisory: 1, autonomous: 2 }
    let worst = null
    for (const d of fleetData.databases) {
      const t = d.status?.trust_level
      if (!t) continue
      if (worst == null || order[t] < order[worst]) worst = t
    }
    return worst
  })()

  const isAdmin = user?.role === 'admin'
  const [drawerOpen, setDrawerOpen] = useState(false)

  // Close the drawer on navigation. Listen for hashchange so that
  // clicking a link inside the drawer (which is a plain anchor)
  // doesn't leave the overlay stuck over the content.
  useEffect(() => {
    const close = () => setDrawerOpen(false)
    window.addEventListener('hashchange', close)
    return () => window.removeEventListener('hashchange', close)
  }, [])

  // Lock body scroll while the drawer is open on mobile.
  useEffect(() => {
    if (!drawerOpen) return
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => { document.body.style.overflow = prev }
  }, [drawerOpen])

  const navContent = (
    <>
      <div
        className="text-lg font-bold mb-4 flex items-center
          justify-between"
        style={{ color: 'var(--accent)' }}>
        <span>pg_sage</span>
        <button
          type="button"
          aria-label="Close navigation"
          data-testid="sidebar-close"
          onClick={() => setDrawerOpen(false)}
          className="md:hidden p-1 rounded"
          style={{ color: 'var(--text-secondary)' }}>
          <X size={18} />
        </button>
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
                active={hash === n.path || n.aliases?.includes(hash)}
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
    </>
  )

  return (
    <div className="flex h-screen min-w-0 overflow-hidden" {...rest}>
      {/* Desktop sidebar */}
      <nav
        data-testid="sidebar-desktop"
        className="hidden md:flex w-56 flex-shrink-0 border-r
          flex-col p-4 gap-1"
        style={{
          background: 'var(--bg-card)',
          borderColor: 'var(--border)',
        }}>
        {navContent}
      </nav>

      {/* Mobile drawer (shown only when open) */}
      {drawerOpen && (
        <>
          <div
            data-testid="sidebar-overlay"
            className="md:hidden fixed inset-0 z-40"
            style={{ background: 'rgba(0,0,0,0.5)' }}
            onClick={() => setDrawerOpen(false)}
          />
          <nav
            data-testid="sidebar-drawer"
            className="md:hidden fixed top-0 left-0 bottom-0 z-50
              w-64 border-r flex flex-col p-4 gap-1 overflow-y-auto"
            style={{
              background: 'var(--bg-card)',
              borderColor: 'var(--border)',
            }}>
            {navContent}
          </nav>
        </>
      )}

      <main className="flex-1 overflow-auto min-w-0">
        <header
          className="flex items-center justify-between gap-2
            px-3 md:px-4 py-3 md:py-4 border-b"
          style={{ borderColor: 'var(--border)' }}>
          <div className="flex items-center gap-2 min-w-0">
            <button
              type="button"
              aria-label="Open navigation"
              data-testid="sidebar-open"
              onClick={() => setDrawerOpen(true)}
              className="md:hidden p-1.5 rounded flex-shrink-0"
              style={{
                color: 'var(--text-primary)',
                border: '1px solid var(--border)',
              }}>
              <Menu size={18} />
            </button>
            <h1
              className="text-base md:text-lg font-semibold truncate"
              style={{ color: 'var(--text-primary)' }}>
              {pageTitle || ALL_NAV_ITEMS.find(n => n.path === hash)?.label
                || 'pg_sage'}
            </h1>
          </div>
          <div className="flex items-center gap-1.5 md:gap-3
            flex-wrap justify-end min-w-0">
            {emergencyStopped && <EmergencyBadge />}
            {trustLevel && <TrustBadge level={trustLevel} />}
            <TimeRangePicker />
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
        <div className="p-3 md:p-6">
          <PendingActionsContext.Provider
            value={{ refetch: refetchPending }}>
            {children}
          </PendingActionsContext.Provider>
        </div>
      </main>
    </div>
  )
}
