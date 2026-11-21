import { useState } from 'react'
import { TabList, TabPanel } from '../../components/Tabs'
import { AgentsPage } from '../AgentsPage'
import { MCPTokensPage } from '../MCPTokensPage'

// AgentsHub is the one Agents nav entry (ux-11): the agent principals, and
// for admins a tab with the MCP tokens external tools connect with (the
// tokens API is admin only). #/mcp-tokens redirects to #/agents?tab=tokens.
// An operator never gets the tokens tab, whatever the link says.

const TABS = [
  { key: 'agents', label: 'Agents', testId: 'agents-tab-agents' },
  { key: 'tokens', label: 'MCP tokens', testId: 'agents-tab-tokens' },
]

export function AgentsHub({ user, databases, initialTab }) {
  const isAdmin = user?.role === 'admin'
  const [tab, setTab] = useState(isAdmin && initialTab === 'tokens' ? 'tokens' : 'agents')
  if (!isAdmin) return <AgentsPage user={user} databases={databases} />
  return (
    <div className="space-y-4">
      <TabList id="agents" label="Agents sections" tabs={TABS} active={tab} onSelect={setTab}
        className="flex gap-2" tabClassName="px-3 py-1 rounded text-sm"
        tabStyle={selected => ({
          background: selected ? 'var(--accent-solid)' : 'var(--bg-card)',
          color: selected ? '#fff' : 'var(--text-primary)',
          border: '1px solid var(--border)',
        })} />
      <TabPanel id="agents" active={tab}>
        {tab === 'tokens'
          ? <MCPTokensPage />
          : <AgentsPage user={user} databases={databases} />}
      </TabPanel>
    </div>
  )
}
