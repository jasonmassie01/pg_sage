import * as Tooltip from '@radix-ui/react-tooltip'
import { Cloud, Database, Info, Save } from 'lucide-react'

const PROVIDERS = [
  { key: 'local_postgres', label: 'Local Postgres' },
  { key: 'aws_rds', label: 'AWS RDS' },
  { key: 'gcp_cloudsql', label: 'Cloud SQL' },
  { key: 'databricks_lakebase', label: 'Lakebase' },
]

const FIELD_HELP = {
  profile_id: 'Stable profile key used by agents and the API. Use lowercase letters, numbers, and underscores.',
  name: 'Human readable label shown in the Agent DB UI.',
  provider: 'Where pg_sage should provision the agent database. Cloud providers are instance-level only.',
  provisioning_level: 'Isolation boundary. Local Postgres supports schema and database; cloud providers use instance.',
  cpu: 'Planning CPU count for budget and capacity comparison. Cloud APIs may map this to provider-specific classes.',
  memory_gb: 'Planning memory in GiB. Keep aligned with the provider class or tier you choose.',
  storage_gb: 'Initial storage size in GiB. For Cloud SQL live validation the minimum is 10 GiB.',
  max_connections: 'Connection budget pg_sage should assume for this agent-created database.',
  monthly_budget_usd: 'Expected monthly spend guardrail for this profile.',
  provider_params_text: 'Advanced JSON passed to provider-specific plan generation. Prefer the explicit cloud fields when available.',
  aws_region: 'AWS region for the RDS instance, for example us-east-1.',
  aws_class: 'RDS DB instance class such as db.t4g.micro.',
  aws_storage: 'Allocated RDS storage in GiB.',
  aws_backup_retention: 'Managed backup retention in days. Use at least 1 for disposable validation and 7+ for production.',
  gcp_project: 'GCP project ID used with the Cloud SQL Admin API.',
  gcp_region: 'Cloud SQL region, for example us-central1.',
  gcp_tier: 'Cloud SQL machine tier. Use db-f1-micro or db-custom-* for Enterprise edition.',
  gcp_version: 'Cloud SQL Postgres version, for example POSTGRES_16.',
  gcp_edition: 'Cloud SQL edition. ENTERPRISE supports low-cost shared-core tiers.',
  gcp_storage: 'Cloud SQL storage size in GiB. Minimum is 10 GiB.',
  gcp_ipv4: 'Enable public IPv4 connectivity. Use with empty authorized networks plus SSL for smoke tests.',
  gcp_ssl: 'Require SSL for Cloud SQL client connections.',
  lakebase_mode: 'Lakebase provisioning shape: autoscaling branch or provisioned instance.',
  lakebase_project: 'Lakebase project, database, or workspace grouping used by the Databricks API.',
  lakebase_source_instance: 'The existing Lakebase instance or branch path used as the source for a new branch.',
  lakebase_extensions: 'Comma-separated extension expectations to validate against the Lakebase allowlist.',
}

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
  const providerParams = parseProviderParams(form.provider_params_text)
  const updateParam = (key, value) => {
    const nextParams = { ...providerParams, [key]: value }
    update('provider_params_text', JSON.stringify(nextParams, null, 2))
  }
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
          tipKey="profile_id"
          onChange={value => update('profile_id', value)} />
        <TextField label="Name" value={form.name}
          tipKey="name"
          onChange={value => update('name', value)} />
        <SelectField label="Provider" value={form.provider} options={PROVIDERS}
          tipKey="provider"
          onChange={value => update('provider', value)} />
        <SelectField label="Level" value={form.provisioning_level}
          options={levels}
          tipKey="provisioning_level"
          onChange={value => update('provisioning_level', value)} />
        <TextField label="CPU" value={form.cpu}
          tipKey="cpu"
          onChange={value => update('cpu', value)} />
        <TextField label="Memory GB" value={form.memory_gb}
          tipKey="memory_gb"
          onChange={value => update('memory_gb', value)} />
        <TextField label="Storage GB" value={form.storage_gb}
          tipKey="storage_gb"
          onChange={value => update('storage_gb', value)} />
        <TextField label="Max connections" value={form.max_connections}
          tipKey="max_connections"
          onChange={value => update('max_connections', value)} />
        <TextField label="Monthly budget USD" value={form.monthly_budget_usd}
          tipKey="monthly_budget_usd"
          onChange={value => update('monthly_budget_usd', value)} />
        <CloudSettingsFields
          provider={form.provider}
          params={providerParams}
          onParamChange={updateParam}
        />
        <TextField label="Provider params JSON" value={form.provider_params_text}
          tipKey="provider_params_text"
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

function parseProviderParams(text) {
  try {
    const parsed = JSON.parse(text || '{}')
    return parsed && typeof parsed === 'object' && !Array.isArray(parsed)
      ? parsed
      : {}
  } catch {
    return {}
  }
}

function CloudSettingsFields({ provider, params, onParamChange }) {
  if (provider === 'aws_rds') {
    return (
      <>
        <TextField label="AWS region" value={params.region || ''}
          tipKey="aws_region"
          onChange={value => onParamChange('region', value)} />
        <TextField label="RDS instance class"
          value={params.db_instance_class || ''}
          tipKey="aws_class"
          onChange={value => onParamChange('db_instance_class', value)} />
        <TextField label="Allocated storage"
          value={params.allocated_storage || ''}
          tipKey="aws_storage"
          onChange={value => onParamChange('allocated_storage', value)} />
        <TextField label="Backup retention days"
          value={params.backup_retention_days || ''}
          tipKey="aws_backup_retention"
          onChange={value => onParamChange('backup_retention_days', value)} />
      </>
    )
  }
  if (provider === 'gcp_cloudsql') {
    return (
      <>
        <TextField label="GCP project" value={params.project || ''}
          tipKey="gcp_project"
          onChange={value => onParamChange('project', value)} />
        <TextField label="Cloud SQL region" value={params.region || ''}
          tipKey="gcp_region"
          onChange={value => onParamChange('region', value)} />
        <TextField label="Cloud SQL tier" value={params.tier || ''}
          tipKey="gcp_tier"
          onChange={value => onParamChange('tier', value)} />
        <TextField label="Postgres version"
          value={params.database_version || ''}
          tipKey="gcp_version"
          onChange={value => onParamChange('database_version', value)} />
        <SelectField label="Cloud SQL edition"
          value={params.edition || 'ENTERPRISE'}
          tipKey="gcp_edition"
          options={[
            { key: 'ENTERPRISE', label: 'Enterprise' },
            { key: 'ENTERPRISE_PLUS', label: 'Enterprise Plus' },
          ]}
          onChange={value => onParamChange('edition', value)} />
        <TextField label="Cloud SQL storage"
          value={params.storage_size || ''}
          tipKey="gcp_storage"
          onChange={value => onParamChange('storage_size', value)} />
        <SelectField label="Public IPv4"
          value={String(params.ipv4_enabled ?? true)}
          tipKey="gcp_ipv4"
          options={[
            { key: 'true', label: 'Enabled' },
            { key: 'false', label: 'Disabled' },
          ]}
          onChange={value => onParamChange('ipv4_enabled', value === 'true')} />
        <SelectField label="Require SSL"
          value={String(params.require_ssl ?? true)}
          tipKey="gcp_ssl"
          options={[
            { key: 'true', label: 'Required' },
            { key: 'false', label: 'Not required' },
          ]}
          onChange={value => onParamChange('require_ssl', value === 'true')} />
      </>
    )
  }
  if (provider === 'databricks_lakebase') {
    return (
      <>
        <SelectField label="Lakebase mode"
          value={params.mode || 'autoscaling_branch'}
          tipKey="lakebase_mode"
          options={[
            { key: 'autoscaling_branch', label: 'Autoscaling branch' },
            { key: 'provisioned_instance', label: 'Provisioned instance' },
          ]}
          onChange={value => onParamChange('mode', value)} />
        <TextField label="Lakebase project" value={params.project || ''}
          tipKey="lakebase_project"
          onChange={value => onParamChange('project', value)} />
        <TextField label="Lakebase source instance"
          value={params.source_instance || params.source_branch || ''}
          tipKey="lakebase_source_instance"
          onChange={value => onParamChange('source_instance', value)} />
        <TextField label="Extension expectations"
          value={params.extension_allowlist || ''}
          tipKey="lakebase_extensions"
          onChange={value => onParamChange('extension_allowlist', value)} />
      </>
    )
  }
  return null
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
          {provider.interface || provider.version || provider.detail}
        </div>
        {provider.detail && (
          <div className="truncate text-xs" style={{ color: 'var(--text-secondary)' }}>
            {provider.detail}
          </div>
        )}
      </div>
      <span className="text-xs font-medium"
        style={{ color: provider.found ? 'var(--green)' : 'var(--yellow)' }}>
        {provider.found ? 'ready' : 'missing'}
      </span>
    </div>
  )
}

function AgentDBTip({ tipKey, children }) {
  const help = FIELD_HELP[tipKey]
  if (!help) return children
  return (
    <Tooltip.Provider delayDuration={200}>
      <Tooltip.Root>
        <Tooltip.Trigger asChild>
          <span className="inline-flex items-center gap-1 cursor-help"
            data-testid={`agent-db-tip-${tipKey}`}
            title={help}>
            {children}
            <Info size={12} />
          </span>
        </Tooltip.Trigger>
        <Tooltip.Portal>
          <Tooltip.Content side="top" sideOffset={6}
            className="z-50 max-w-sm rounded-md border border-gray-700 bg-gray-900 px-3 py-2 text-xs text-gray-50 shadow-lg">
            {help}
            <Tooltip.Arrow className="fill-gray-900" />
          </Tooltip.Content>
        </Tooltip.Portal>
      </Tooltip.Root>
    </Tooltip.Provider>
  )
}

function TextField({ label, value, onChange, tipKey }) {
  return (
    <label className="block text-xs" style={{ color: 'var(--text-secondary)' }}>
      <AgentDBTip tipKey={tipKey}>
        <span>{label}</span>
      </AgentDBTip>
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

function SelectField({ label, value, options, onChange, tipKey }) {
  return (
    <label className="block text-xs" style={{ color: 'var(--text-secondary)' }}>
      <AgentDBTip tipKey={tipKey}>
        <span>{label}</span>
      </AgentDBTip>
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
