/* eslint-disable react-refresh/only-export-components */
import { createContext, useContext, useState, useEffect } from 'react'
import { LogOut, Menu, X, ChevronDown } from 'lucide-react'
import { DatabasePicker } from './DatabasePicker'
import { EmergencyStopControl } from './EmergencyStopControl'
import { useAPI } from '../hooks/useAPI'
import { useLiveRefetch } from '../hooks/useLiveEvents'
import { useLiveStatus } from '../hooks/liveEventsContext'
import { FreshnessIndicator } from './FreshnessIndicator'
import { TimeRangePicker } from './TimeRangePicker'
import { TimeZoneToggle } from './TimeZoneToggle'
import { HeaderTrustBadge } from './HeaderTrustBadge'
import { PageIntro } from './PageIntro'
import { pageGuide } from './pageGuide'
import { MonitoringContext } from './MonitoringContext'
import { PageErrorBoundary } from './PageErrorBoundary'
import { SecretsPostureBanner } from './SecretsPostureBanner'
import { findRoute, navGroups, routeHref, routeLabel } from '../routes'
import { inboxBadge } from '../lib/inbox'
import { readPref, writePref } from '../lib/storage'

const PendingActionsContext = createContext({ refetch: () => {} })

export function usePendingActionsRefetch() {
  return useContext(PendingActionsContext).refetch
}

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

// inboxCountURL is the inbox count of the selected database, or of the
// fleet in the all-databases view (ux-01b: the one nav badge).
function inboxCountURL(selectedDB) {
  const base = '/api/v1/inbox/count'
  return selectedDB && selectedDB !== 'all'
    ? `${base}?database=${encodeURIComponent(selectedDB)}` : base
}

function NavLink({ route, active, pending }) {
  const Icon = route.icon
  return (
    <a
      href={routeHref(route)}
      aria-current={active ? 'page' : undefined}
      data-testid={route.testId}
      className={`sage-nav-link flex items-center gap-2 px-3 py-2 rounded text-sm${active ? ' sage-nav-link-active' : ''}`}
      style={{
        color: active
          ? 'var(--accent)' : 'var(--text-secondary)',
        background: active
          ? 'var(--bg-hover)' : 'transparent',
      }}>
      <Icon size={16} />
      {routeLabel(route)}
      {route.id === 'inbox' && pending && (
        <span data-testid="nav-badge"
          title={pending.title}
          className="ml-auto px-1.5 py-0.5 rounded-full text-xs"
          style={{
            background: 'var(--red-solid)',
            color: '#fff',
            fontSize: '0.65rem',
            lineHeight: 1,
          }}>
          {pending.text}
        </span>
      )}
    </a>
  )
}

// CollapsibleNav is a nav group the viewer can fold (ux-11: Evidence).
// Layout keeps the open state so the desktop sidebar and the mobile drawer
// share it.
function CollapsibleNav({ group, activeId, pending, open, onToggle }) {
  const listId = `${group.id}-navigation`
  return (
    <div>
      <button type="button" aria-expanded={open} aria-controls={listId}
        onClick={onToggle}
        className="mt-3 flex w-full items-center gap-2 rounded px-3 py-2 text-sm"
        style={{ color: 'var(--text-secondary)' }}>
        <ChevronDown size={16} aria-hidden="true"
          style={{ transform: open ? 'rotate(180deg)' : 'none' }} />
        {group.heading}
      </button>
      {open && (
        <div id={listId} className="ml-3 border-l pl-2" style={{ borderColor: 'var(--border)' }}>
          {group.routes.map(route => (
            <NavLink key={route.id} route={route} active={route.id === activeId}
              pending={pending} />
          ))}
        </div>
      )}
    </div>
  )
}

// SidebarNav renders the nav groups this user can see, from the route
// registry (dashboard-06).
function SidebarNav({ user, platformManaged, activeId, pending, collapsed }) {
  return navGroups(user, platformManaged).map(group => (group.collapsible
    ? <CollapsibleNav key={group.id} group={group} activeId={activeId} pending={pending}
      open={collapsed.isOpen(group)} onToggle={() => collapsed.toggle(group)} />
    : (
      <div key={group.id}>
        <NavHeading>{group.heading}</NavHeading>
        {group.routes.map(route => (
          <NavLink key={route.id} route={route} active={route.id === activeId}
            pending={pending} />
        ))}
      </div>
    )))
}

const navPrefKey = group => `pg_sage_nav_${group.id}`

// storedOpen is the viewer's last choice for a collapsible group: true,
// false, or null when they made none (or storage is unavailable).
function storedOpen(group) {
  const value = readPref(navPrefKey(group), '')
  if (value === 'open') return true
  if (value === 'closed') return false
  return null
}

// useCollapsedGroups: a collapsible group is open when the viewer last
// opened it, or, before any choice, when the current page is in it. The
// choice persists per browser (lib/storage.js); blocked storage keeps it
// for this page view only.
function useCollapsedGroups(activeRoute) {
  const [choices, setChoices] = useState({})
  const choice = group => (group.id in choices ? choices[group.id] : storedOpen(group))
  const isOpen = group => choice(group) ?? activeRoute?.navGroup === group.id
  const toggle = group => {
    const next = !isOpen(group)
    writePref(navPrefKey(group), next ? 'open' : 'closed')
    setChoices(prev => ({ ...prev, [group.id]: next }))
  }
  return { isOpen, toggle }
}

// ScopeEmpty stands in for the scope picker before any database exists;
// only an admin can open the Fleet page to add one.
function ScopeEmpty({ isAdmin }) {
  return (
    <span data-testid="scope-empty" className="text-sm"
      style={{ color: 'var(--text-secondary)' }}>
      No databases yet.{' '}
      {isAdmin
        ? <a href="#/manage-databases" className="underline"
          style={{ color: 'var(--accent)' }}>Add a database</a>
        : 'Ask an admin to add one.'}
    </span>
  )
}

// App owns the one /api/v1/databases poll: fleetData is its result (null
// until the first load) and onFleetChanged refetches it.
export function Layout({
  children, databases, selectedDB, onSelectDB, selectedTag = '', onSelectTag, scopeNotice,
  user, authMode = user?.auth_mode, fleetData, fleetUpdatedAt, fleetError,
  onFleetChanged, onLogout, pageTitle,
  ...rest
}) {
  const hash = (window.location.hash || '#/').split('?')[0]
  const canReviewActions =
    user?.role === 'admin' || user?.role === 'operator'
  const { data: countData, refetch: refetchPending } = useAPI(
    canReviewActions ? inboxCountURL(selectedDB) : null, 30000,
  )
  // The Inbox badge counts every decision waiting on a person in the
  // selected database, or in the fleet (ux-01b); Actions keeps no badge.
  // It refetches on any database's action events and when a page decides
  // something; a hidden tab refreshes it when shown.
  useLiveRefetch(['actions'], refetchPending)
  const pending = inboxBadge(countData)
  const liveStatus = useLiveStatus()
  const emergencyStopped =
    fleetData?.summary?.emergency_stopped === true

  const isAdmin = user?.role === 'admin'
  const platformManaged = authMode === 'databricks_app'
  const activeRoute = findRoute(hash)
  const headerTitle = pageTitle || activeRoute?.title || 'pg_sage'
  // Each tab names its page for the tab list and screen readers (dashboard-10).
  useEffect(() => {
    document.title = headerTitle === 'pg_sage' ? 'pg_sage' : `${headerTitle} · pg_sage`
  }, [headerTitle])
  const [drawerOpen, setDrawerOpen] = useState(false)
  const collapsed = useCollapsedGroups(activeRoute)

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
      <div className="sage-brand">
        <a href="#/" className="sage-brand-home" aria-label="pg_sage home">
          <span className="sage-brand-mark" aria-hidden="true">pg<span>_</span></span>
          <span><span className="sage-brand-name">pg_sage</span><span className="sage-brand-caption">POSTGRES, WITH RECEIPTS.</span></span>
        </a>
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

      <div className="sage-sidebar-label"><span className="sage-terminal-prompt" aria-hidden="true">❯</span> database control room</div>

      <SidebarNav user={user} platformManaged={platformManaged}
        activeId={activeRoute?.id} pending={pending} collapsed={collapsed} />

      <div
        className="sage-account mt-auto pt-4"
        style={{ borderTop: '1px solid var(--border)' }}>
        {user && (
          <a
            href="#/profile"
            title="Account and SSO"
            className="block px-3 py-1 text-xs mb-2"
            data-testid="user-email"
            style={{ color: 'var(--text-secondary)' }}>
            {user.email}
            <span className="ml-1 opacity-60">
              ({user.role})
            </span>
          </a>
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
    <div className="sage-shell flex h-screen min-w-0 overflow-hidden" {...rest}>
      {/* Desktop sidebar */}
      <nav
        data-testid="sidebar-desktop"
        aria-label="Main navigation"
        className="sage-sidebar hidden md:flex w-56 flex-shrink-0 border-r
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
            aria-label="Mobile navigation"
            data-testid="sidebar-drawer"
            className="sage-sidebar md:hidden fixed top-0 left-0 bottom-0 z-50
              w-64 border-r flex flex-col p-4 gap-1 overflow-y-auto"
            style={{
              background: 'var(--bg-card)',
              borderColor: 'var(--border)',
            }}>
            {navContent}
          </nav>
        </>
      )}

      <main className="sage-main flex-1 overflow-auto min-w-0" id="main-content">
        <header
          className="sage-header flex items-center justify-between gap-2
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
            <div className="min-w-0">
              <div className="sage-header-eyebrow">POSTGRES OPERATIONS <span aria-hidden="true">/</span> CONTROL ROOM</div>
              <h1
              className="text-base md:text-lg font-semibold truncate"
              style={{ color: 'var(--text-primary)' }}>
              {headerTitle}
              </h1>
            </div>
          </div>
          <div className="flex items-center gap-1.5 md:gap-3
            flex-wrap justify-end min-w-0">
            <FreshnessIndicator status={liveStatus} updatedAt={fleetUpdatedAt} />
            <EmergencyStopControl
              databases={fleetData?.databases || []}
              selectedDB={selectedDB}
              canControl={canReviewActions}
              summaryStopped={emergencyStopped}
              fleetStop={fleetData?.summary}
              onChanged={onFleetChanged}
            />
            <HeaderTrustBadge rows={fleetData?.databases} selectedDB={selectedDB} />
            {activeRoute?.usesTimeRange && (
              <TimeRangePicker label={activeRoute.timeRangeLabel} />
            )}
            <TimeZoneToggle />
            <DatabasePicker
              data-testid="database-picker"
              databases={databases}
              selected={selectedDB}
              selectedTag={selectedTag}
              onSelect={onSelectDB}
              onSelectTag={onSelectTag}
            />
            {fleetData && !fleetError && databases?.length === 0 && (
              <ScopeEmpty isAdmin={isAdmin} />
            )}
          </div>
        </header>
        <div className="sage-content p-3 md:p-6">
          {/* A page render error stays here; the header and nav survive. */}
          <PageErrorBoundary resetKey={window.location.hash || '#/'}>
            <SecretsPostureBanner isAdmin={isAdmin} />
            {scopeNotice && (
              <p role="status" data-testid="scope-notice" className="mb-4 text-sm"
                style={{ color: 'var(--amber, #f0a54a)' }}>{scopeNotice}</p>
            )}
            <MonitoringContext database={selectedDB} fleetData={fleetData}
              error={fleetError} user={user} />
            <PageIntro route={hash} guide={pageGuide(hash, { pageTitle, authMode })} />
            <PendingActionsContext.Provider
              value={{ refetch: refetchPending }}>
              {children}
            </PendingActionsContext.Provider>
          </PageErrorBoundary>
        </div>
      </main>
    </div>
  )
}
