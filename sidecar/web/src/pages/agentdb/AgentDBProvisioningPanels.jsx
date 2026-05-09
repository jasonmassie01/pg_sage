import { Cloud, Database, Save } from 'lucide-react'

const PROVIDERS = [
  { key: 'local_postgres', label: 'Local Postgres' },
  { key: 'aws_rds', label: 'AWS RDS' },
  { key: 'gcp_cloudsql', label: 'Cloud SQL' },
  { key: 'databricks_lakebase', label: 'Lakebase' },
]

export function ProfilePanel({ profiles, form, busy, onChange, onSubmit }) {
  function update(key, value) {
    const next = { ...form, [key]: value }
    if (key === 'provider' && value !== 'local_postgres') {
      next.provisioning_level = 'instance'
    }
    onChange(next)
  }
  const levels = form.provider === 'local_postgres'
    ? [
      { key: 'schema', label: 'Schema' },
      { key: 'database', label: 'Database' },
    ]
    : [{ key: 'instance', label: 'Instance' }]
  return (
    <section className="rounded border p-4 space-y-3"
      data-testid="agent-db-profiles"
      style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
      <div className="flex items-center gap-2">
        <Database size={16} style={{ color: 'var(--accent)' }} />
        <h2 className="text-sm font-semibold" style={{ color: 'var(--text-primary)' }}>
          Size profiles
        </h2>
      </div>
      <form onSubmit={onSubmit} className="grid gap-3 md:grid-cols-2">
        <TextField label="Profile ID" value={form.profile_id}
          onChange={value => update('profile_id', value)} />
        <TextField label="Name" value={form.name}
          onChange={value => update('name', value)} />
        <SelectField label="Provider" value={form.provider} options={PROVIDERS}
          onChange={value => update('provider', value)} />
        <SelectField label="Level" value={form.provisioning_level}
          options={levels}
          onChange={value => update('provisioning_level', value)} />
        <TextField label="CPU" value={form.cpu}
          onChange={value => update('cpu', value)} />
        <TextField label="Memory GB" value={form.memory_gb}
          onChange={value => update('memory_gb', value)} />
        <TextField label="Storage GB" value={form.storage_gb}
          onChange={value => update('storage_gb', value)} />
        <TextField label="Max connections" value={form.max_connections}
          onChange={value => update('max_connections', value)} />
        <TextField label="Monthly budget USD" value={form.monthly_budget_usd}
          onChange={value => update('monthly_budget_usd', value)} />
        <TextField label="Provider params JSON" value={form.provider_params_text}
          onChange={value => update('provider_params_text', value)} />
        <button type="submit" disabled={busy}
          data-testid="agent-db-profile-submit"
          className="inline-flex items-center gap-2 rounded px-3 py-2 text-sm"
          style={{ background: 'var(--accent)', color: '#fff' }}>
          <Save size={15} />
          Save profile
        </button>
      </form>
      <ProfileList profiles={profiles} />
    </section>
  )
}

function ProfileList({ profiles }) {
  return (
    <div className="grid gap-2 md:grid-cols-2">
      {profiles.slice(0, 6).map(profile => (
        <div key={profile.profile_id} className="rounded border p-2"
          style={{ borderColor: 'var(--border)' }}>
          <div className="text-sm" style={{ color: 'var(--text-primary)' }}>
            {profile.name}
          </div>
          <div className="text-xs" style={{ color: 'var(--text-secondary)' }}>
            {profile.provider} / {profile.provisioning_level}
          </div>
        </div>
      ))}
    </div>
  )
}

export function ProviderReadinessPanel({ providers }) {
  return (
    <section className="rounded border p-4" data-testid="agent-db-providers"
      style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
      <div className="mb-3 flex items-center gap-2">
        <Cloud size={16} style={{ color: 'var(--accent)' }} />
        <h2 className="text-sm font-semibold" style={{ color: 'var(--text-primary)' }}>
          Providers
        </h2>
      </div>
      <div className="space-y-2">
        {providers.map(provider => (
          <ProviderRow key={provider.provider} provider={provider} />
        ))}
      </div>
    </section>
  )
}

function ProviderRow({ provider }) {
  return (
    <div className="flex items-center justify-between gap-3 rounded border p-2"
      style={{ borderColor: 'var(--border)' }}>
      <div className="min-w-0">
        <div className="text-sm" style={{ color: 'var(--text-primary)' }}>
          {provider.label}
        </div>
        <div className="truncate text-xs" style={{ color: 'var(--text-secondary)' }}>
          {provider.version || provider.detail}
        </div>
      </div>
      <span className="text-xs font-medium"
        style={{ color: provider.found ? 'var(--green)' : 'var(--yellow)' }}>
        {provider.found ? 'ready' : 'missing'}
      </span>
    </div>
  )
}

function TextField({ label, value, onChange }) {
  return (
    <label className="block text-xs" style={{ color: 'var(--text-secondary)' }}>
      <span>{label}</span>
      <input value={value} onChange={e => onChange(e.target.value)}
        className="mt-1 w-full rounded border px-2 py-1.5 text-sm"
        style={{
          background: 'var(--bg-primary)',
          borderColor: 'var(--border)',
          color: 'var(--text-primary)',
        }} />
    </label>
  )
}

function SelectField({ label, value, options, onChange }) {
  return (
    <label className="block text-xs" style={{ color: 'var(--text-secondary)' }}>
      <span>{label}</span>
      <select value={value} onChange={e => onChange(e.target.value)}
        className="mt-1 w-full rounded border px-2 py-1.5 text-sm"
        style={{
          background: 'var(--bg-primary)',
          borderColor: 'var(--border)',
          color: 'var(--text-primary)',
        }}>
        {options.map(option => (
          <option key={option.key} value={option.key}>{option.label}</option>
        ))}
      </select>
    </label>
  )
}
