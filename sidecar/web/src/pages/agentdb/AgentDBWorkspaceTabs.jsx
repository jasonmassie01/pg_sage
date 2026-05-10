import {
  Activity, Blocks, CloudCog, Database, FileCode, Layers3, Settings2,
} from 'lucide-react'

const TABS = [
  { key: 'deployments', label: 'Deployments', icon: Database },
  { key: 'provision', label: 'Provision', icon: Layers3 },
  { key: 'profiles', label: 'Profiles', icon: Settings2 },
  { key: 'provider-settings', label: 'Provider Settings', icon: CloudCog },
  { key: 'terraform', label: 'Terraform', icon: FileCode },
  { key: 'blueprints', label: 'Blueprints', icon: Blocks },
  { key: 'activity', label: 'Activity', icon: Activity },
]

export function AgentDBWorkspaceTabs({ activeTab, onChange }) {
  return (
    <div className="rounded border p-2"
      style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
      <div role="tablist" aria-label="Agent DB workspace"
        className="flex flex-wrap gap-1">
        {TABS.map(tab => (
          <TabButton
            key={tab.key}
            tab={tab}
            active={activeTab === tab.key}
            onClick={() => onChange(tab.key)}
          />
        ))}
      </div>
    </div>
  )
}

function TabButton({ tab, active, onClick }) {
  const Icon = tab.icon
  return (
    <button type="button" role="tab" aria-selected={active}
      aria-controls={`agent-db-panel-${tab.key}`}
      id={`agent-db-tab-${tab.key}`}
      onClick={onClick}
      className="inline-flex items-center gap-2 rounded px-3 py-2 text-sm"
      style={{
        background: active ? 'var(--bg-hover)' : 'transparent',
        color: active ? 'var(--text-primary)' : 'var(--text-secondary)',
        border: `1px solid ${active ? 'var(--accent)' : 'transparent'}`,
      }}>
      <Icon size={15} />
      {tab.label}
    </button>
  )
}
