import { useState } from 'react'
import { ChannelsTab } from './notifications/ChannelsTab'
import { RulesTab } from './notifications/RulesTab'
import { LogTab } from './notifications/LogTab'

const TABS = ['Channels', 'Rules', 'Log']

export function NotificationsPage() {
  const [tab, setTab] = useState('Channels')

  return (
    <div>
      <div className="flex gap-1 mb-4">
        {TABS.map(t => (
          <button key={t} onClick={() => setTab(t)}
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
      {tab === 'Log' && <LogTab />}
    </div>
  )
}
