import { useState } from 'react'
import { ChannelsTab } from './notifications/ChannelsTab'
import { RulesTab } from './notifications/RulesTab'
import { LogTab } from './notifications/LogTab'
import { MutesPanel } from './notifications/MutesPanel'
import { WeeklyLine } from './notifications/WeeklyLine'

// alerting-06c: the delivery log is open to admins and operators (it
// replaced the Alert log); channels and rules stay admin-only, as the API
// enforces.
const ADMIN_TABS = ['Channels', 'Rules', 'Log']
const REVIEWER_TABS = ['Log']

// tabsFor returns the tabs a role may open; no user fails closed to Log.
function tabsFor(user) {
  return user?.role === 'admin' ? ADMIN_TABS : REVIEWER_TABS
}

export function NotificationsPage({ user, database, initialTab, muteLink }) {
  const tabs = tabsFor(user)
  const [tab, setTab] = useState(
    () => tabs.find(t => t.toLowerCase() === initialTab) || tabs[0])

  return (
    <div>
      <WeeklyLine />
      {user?.role === 'admin' && <MutesPanel link={muteLink} />}
      <div className="flex gap-1 mb-4">
        {tabs.map(t => (
          <button key={t} onClick={() => setTab(t)}
            data-testid={`notifications-tab-${t.toLowerCase()}`}
            className="px-4 py-2 rounded-t text-sm font-medium"
            style={{
              background: tab === t
                ? 'var(--bg-card)' : 'transparent',
              color: tab === t
                ? 'var(--accent)' : 'var(--text-secondary)',
              borderBottom: tab === t
                ? '2px solid var(--accent)' : 'none',
            }}>
            {t}
          </button>
        ))}
      </div>
      {tab === 'Channels' && <ChannelsTab />}
      {tab === 'Rules' && <RulesTab />}
      {tab === 'Log' && <LogTab database={database} />}
    </div>
  )
}
