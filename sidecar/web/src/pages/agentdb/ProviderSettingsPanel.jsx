import { useMemo, useState } from 'react'
import { CloudCog, Save } from 'lucide-react'
import { FieldTip } from './AgentDBFormControls'

const PROVIDERS = [
  { key: 'aws_rds', label: 'AWS RDS' },
  { key: 'gcp_cloudsql', label: 'Cloud SQL' },
  { key: 'databricks_lakebase', label: 'Lakebase' },
]

const SENSITIVE_KEY = /(secret|token|password|credential|private_key|access_key)/i

export function ProviderSettingsPanel({
  configs,
  providers,
  busy,
  onSave,
}) {
  const [provider, setProvider] = useState('aws_rds')
  const [draft, setDraft] = useState({
    dirty: false,
    enabled: true,
    settingsText: defaultSettingsText(),
  })

  const sanitizedConfigs = useMemo(
    () => configs.map(config => ({
      ...config,
      settings: sanitizeSettings(config.settings || {}),
    })),
    [configs],
  )

  const selectedConfig = sanitizedConfigs.find(config => config.provider === provider)
  const enabled = draft.dirty
    ? draft.enabled
    : Boolean(selectedConfig?.enabled ?? true)
  const settingsText = draft.dirty
    ? draft.settingsText
    : JSON.stringify(selectedConfig?.settings || {}, null, 2)

  async function submit(event) {
    event.preventDefault()
    const settings = JSON.parse(settingsText || '{}')
    await onSave(provider, {
      enabled,
      settings: sanitizeSettings(settings),
    })
  }

  return (
    <section className="rounded border p-4 space-y-4"
      data-testid="agent-db-provider-settings"
      style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
      <div className="flex items-center gap-2">
        <CloudCog size={16} style={{ color: 'var(--accent)' }} />
        <h2 className="text-sm font-semibold"
          style={{ color: 'var(--text-primary)' }}>
          Provider Settings
        </h2>
      </div>
      <form onSubmit={submit} className="grid gap-3 md:grid-cols-2">
        <label className="block text-xs" style={{ color: 'var(--text-secondary)' }}>
          <FieldTip tipKey="provider_settings_provider">
            <span>Settings provider</span>
          </FieldTip>
          <select value={provider} onChange={e => {
            setProvider(e.target.value)
            setDraft({ dirty: false, enabled: true, settingsText: '' })
          }}
            className="mt-1 w-full rounded border px-2 py-1.5 text-sm"
            style={inputStyle()}>
            {PROVIDERS.map(option => (
              <option key={option.key} value={option.key}>
                {option.label}
              </option>
            ))}
          </select>
        </label>
        <label className="inline-flex items-center gap-2 text-xs"
          style={{ color: 'var(--text-secondary)' }}>
          <input type="checkbox" checked={enabled}
            onChange={e => setDraft({
              dirty: true,
              enabled: e.target.checked,
              settingsText,
            })} />
          <FieldTip tipKey="provider_enabled">
            <span>Enabled</span>
          </FieldTip>
        </label>
        <label className="block text-xs md:col-span-2"
          style={{ color: 'var(--text-secondary)' }}>
          <FieldTip tipKey="provider_settings_json">
            <span>Provider settings JSON</span>
          </FieldTip>
          <textarea value={settingsText}
            onChange={e => setDraft({
              dirty: true,
              enabled,
              settingsText: e.target.value,
            })}
            rows={7}
            className="mt-1 w-full rounded border px-2 py-1.5 font-mono text-xs"
            style={inputStyle()} />
        </label>
        <button type="submit" disabled={busy}
          data-testid="agent-db-provider-settings-save"
          className="inline-flex w-fit items-center gap-2 rounded px-3 py-2 text-sm"
          style={{ background: 'var(--accent)', color: '#fff' }}>
          <Save size={15} />
          Save settings
        </button>
      </form>
      <div className="grid gap-2 md:grid-cols-2">
        {sanitizedConfigs.map(config => (
          <div key={config.provider} className="rounded border p-3"
            style={{ borderColor: 'var(--border)' }}>
            <div className="flex items-center justify-between gap-2">
              <span className="text-sm" style={{ color: 'var(--text-primary)' }}>
                {config.provider}
              </span>
              <span className="text-xs"
                style={{ color: config.enabled ? 'var(--green)' : 'var(--yellow)' }}>
                {config.enabled ? 'enabled' : 'disabled'}
              </span>
            </div>
            <pre className="mt-2 overflow-auto rounded p-2 text-xs"
              style={{ background: 'var(--bg-primary)' }}>
              {JSON.stringify(config.settings || {}, null, 2)}
            </pre>
          </div>
        ))}
      </div>
      <ProviderReadiness providers={providers} />
    </section>
  )
}

function ProviderReadiness({ providers }) {
  if (!providers.length) return null
  return (
    <div className="grid gap-2 md:grid-cols-3">
      {providers.map(provider => (
        <div key={provider.provider} className="rounded border p-2"
          style={{ borderColor: 'var(--border)' }}>
          <div className="text-xs" style={{ color: 'var(--text-primary)' }}>
            {provider.label || provider.provider}
          </div>
          <div className="truncate text-xs"
            style={{ color: 'var(--text-secondary)' }}>
            {provider.interface || provider.detail || 'provider'}
          </div>
        </div>
      ))}
    </div>
  )
}

function sanitizeSettings(value) {
  if (Array.isArray(value)) {
    return value.map(item => sanitizeSettings(item))
  }
  if (value && typeof value === 'object') {
    return Object.fromEntries(
      Object.entries(value)
        .filter(([key]) => !SENSITIVE_KEY.test(key))
        .map(([key, val]) => [key, sanitizeSettings(val)]),
    )
  }
  return value
}

function defaultSettingsText() {
  return '{\n  "allowed_regions": []\n}'
}

function inputStyle() {
  return {
    background: 'var(--bg-primary)',
    borderColor: 'var(--border)',
    color: 'var(--text-primary)',
  }
}
