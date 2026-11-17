import { useState } from 'react'
import { TabList, TabPanel } from '../components/Tabs'
import { ChannelsTab } from './notifications/ChannelsTab'
import { RulesTab } from './notifications/RulesTab'
import { LogTab } from './notifications/LogTab'

// alerting-06c: the delivery log is open to admins and operators (it
// replaced the Alert log); channels and rules stay admin-only, as the API
// enforces.
const ADMIN_TABS = ['Channels', 'Rules', 'Log']
const REVIEWER_TABS = ['Log']

// tabsFor returns the tabs a role may open; no user fails closed to Log.
function tabsFor(user) {
  return user?.role === 'admin' ? ADMIN_TABS : REVIEWER_TABS
}

export function NotificationsPage({ user, database, initialTab }) {
  const tabs = tabsFor(user)
  const [tab, setTab] = useState(
    () => tabs.find(t => t.toLowerCase() === initialTab) || tabs[0])

  const tabList = tabs.map(t => ({
    key: t, label: t, testId: `notifications-tab-${t.toLowerCase()}`,
  }))

  return (
    <div>
      <TabList id="notifications" label="Notification sections" tabs={tabList}
        active={tab} onSelect={setTab} className="flex gap-1 mb-4"
        tabClassName="px-4 py-2 rounded-t text-sm font-medium"
        tabStyle={selected => ({
          background: selected ? 'var(--bg-card)' : 'transparent',
          color: selected ? 'var(--accent)' : 'var(--text-secondary)',
          borderBottom: selected ? '2px solid var(--accent)' : 'none',
        })} />
      <TabPanel id="notifications" active={tab}>
        {tab === 'Channels' && <ChannelsTab />}
        {tab === 'Rules' && <RulesTab />}
        {tab === 'Log' && <LogTab database={database} />}
      </TabPanel>
    </div>
  )
}
