import { useState } from 'react'
import { Blocks, Sparkles } from 'lucide-react'
import { FieldTip } from './AgentDBFormControls'

const PROVIDERS = [
  { key: 'aws_rds', label: 'AWS RDS' },
  { key: 'gcp_cloudsql', label: 'Cloud SQL' },
  { key: 'databricks_lakebase', label: 'Lakebase' },
]

const initialForm = {
  blueprint_id: '',
  name: '',
  intent: '',
  provider: 'aws_rds',
  created_by: '',
}

export function BlueprintBuilderPanel({
  blueprints,
  busy,
  onGenerate,
  onApprove,
  onProvision,
}) {
  const [form, setForm] = useState(initialForm)

  async function submit(event) {
    event.preventDefault()
    await onGenerate({
      blueprint_id: form.blueprint_id,
      name: form.name,
      intent: form.intent,
      provider: form.provider,
      created_by: form.created_by,
    })
  }

  return (
    <section className="rounded border p-4 space-y-4"
      data-testid="agent-db-blueprints"
      style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
      <div className="flex items-center gap-2">
        <Blocks size={16} style={{ color: 'var(--accent)' }} />
        <h2 className="text-sm font-semibold"
          style={{ color: 'var(--text-primary)' }}>
          Blueprint Builder
        </h2>
      </div>

      <form onSubmit={submit} className="grid gap-3 md:grid-cols-2">
        <label className="block text-xs md:col-span-2"
          style={{ color: 'var(--text-secondary)' }}>
          <FieldTip tipKey="blueprint_intent">
            <span>Intent</span>
          </FieldTip>
          <textarea value={form.intent}
            onChange={e => setForm({ ...form, intent: e.target.value })}
            rows={5}
            className="mt-1 w-full rounded border px-2 py-1.5 text-sm"
            style={inputStyle()} />
        </label>
        <label className="block text-xs" style={{ color: 'var(--text-secondary)' }}>
          <FieldTip tipKey="blueprint_provider">
            <span>Blueprint provider</span>
          </FieldTip>
          <select value={form.provider}
            onChange={e => setForm({ ...form, provider: e.target.value })}
            className="mt-1 w-full rounded border px-2 py-1.5 text-sm"
            style={inputStyle()}>
            {PROVIDERS.map(option => (
              <option key={option.key} value={option.key}>
                {option.label}
              </option>
            ))}
          </select>
        </label>
        <TextField label="Blueprint ID" value={form.blueprint_id}
          tipKey="blueprint_id"
          onChange={value => setForm({ ...form, blueprint_id: value })} />
        <TextField label="Blueprint name" value={form.name}
          tipKey="blueprint_name"
          onChange={value => setForm({ ...form, name: value })} />
        <TextField label="Created by" value={form.created_by}
          tipKey="blueprint_created_by"
          onChange={value => setForm({ ...form, created_by: value })} />
        <button type="submit" disabled={busy}
          data-testid="agent-db-blueprint-submit"
          className="inline-flex w-fit items-center gap-2 rounded px-3 py-2 text-sm"
          style={{ background: 'var(--accent)', color: '#fff' }}>
          <Sparkles size={15} />
          Generate blueprint
        </button>
      </form>

      <div className="grid gap-2 md:grid-cols-2">
        {blueprints.map(blueprint => (
          <BlueprintCard key={blueprint.blueprint_id}
            blueprint={blueprint}
            busy={busy}
            onApprove={onApprove}
            onProvision={onProvision} />
        ))}
      </div>
    </section>
  )
}

function BlueprintCard({ blueprint, busy, onApprove, onProvision }) {
  const spec = blueprint.blueprint || {}
  const extensions = spec.extensions || []
  const hint = generatedHint(blueprint)

  return (
    <article className="rounded border p-3 space-y-2"
      style={{ borderColor: 'var(--border)' }}>
      <div className="flex items-start justify-between gap-3">
        <div>
          <h3 className="text-sm font-medium"
            style={{ color: 'var(--text-primary)' }}>
            {blueprint.name || blueprint.blueprint_id}
          </h3>
          <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
            {blueprint.blueprint_id}
          </p>
        </div>
        <span className="text-xs" style={{ color: 'var(--text-secondary)' }}>
          {blueprint.status || 'draft'}
        </span>
      </div>

      <div className="grid gap-1 text-xs sm:grid-cols-2"
        style={{ color: 'var(--text-secondary)' }}>
        <span>Provider: {blueprint.provider || 'unknown'}</span>
        <span>Terraform: {blueprint.terraform_template_id || 'none'}</span>
        <span>Region: {spec.region || 'unset'}</span>
        <span>Budget: {formatBudget(spec.budget_usd)}</span>
        <span>Storage: {formatValue(spec.storage_gb, 'GB')}</span>
        <span>Backups: {formatValue(spec.backup_retention_days, 'days')}</span>
        <span>Multi-AZ: {formatBool(spec.multi_az)}</span>
        <span>PITR: {formatBool(spec.pitr)}</span>
        <span>Private network: {formatBool(spec.private_network)}</span>
        <span>Public IP: {formatBool(spec.public_ip)}</span>
      </div>

      {!!extensions.length && (
        <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
          Extensions: {extensions.join(', ')}
        </p>
      )}
      {hint && (
        <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
          Generated: {hint}
        </p>
      )}
      <PolicyFindings findings={blueprint.policy_findings || []} />
      <div className="flex flex-wrap gap-2">
        {blueprint.status === 'generated' && (
          <button type="button" disabled={busy}
            data-testid="agent-db-blueprint-approve"
            className="rounded border px-2 py-1 text-xs"
            style={{ borderColor: 'var(--border)', color: 'var(--green)' }}
            onClick={() => onApprove?.(blueprint.blueprint_id)}>
            Approve
          </button>
        )}
        {blueprint.status === 'approved' && (
          <button type="button" disabled={busy}
            data-testid="agent-db-blueprint-provision"
            className="rounded border px-2 py-1 text-xs"
            style={{ borderColor: 'var(--border)', color: 'var(--accent)' }}
            onClick={() => onProvision?.(blueprint)}>
            Provision
          </button>
        )}
      </div>
    </article>
  )
}

function PolicyFindings({ findings }) {
  if (!findings.length) {
    return (
      <p className="text-xs" style={{ color: 'var(--green)' }}>
        No policy findings
      </p>
    )
  }

  return (
    <ul className="space-y-1">
      {findings.map((finding, index) => (
        <li key={`${finding.message || finding}-${index}`}
          className="text-xs"
          style={{ color: 'var(--text-secondary)' }}>
          {finding.severity ? `${finding.severity}: ` : ''}
          {finding.message || finding.detail || finding}
        </li>
      ))}
    </ul>
  )
}

function TextField({ label, value, onChange, tipKey }) {
  return (
    <label className="block text-xs" style={{ color: 'var(--text-secondary)' }}>
      <FieldTip tipKey={tipKey}>
        <span>{label}</span>
      </FieldTip>
      <input value={value} onChange={e => onChange(e.target.value)}
        className="mt-1 w-full rounded border px-2 py-1.5 text-sm"
        style={inputStyle()} />
    </label>
  )
}

function generatedHint(blueprint) {
  return blueprint.generated_file ||
    blueprint.generated_template ||
    blueprint.template_hint ||
    blueprint.file_hint ||
    blueprint.terraform_file ||
    blueprint.terraform_template_path
}

function formatBudget(value) {
  if (value === undefined || value === null || value === '') return 'unset'
  return `$${value}`
}

function formatValue(value, unit) {
  if (value === undefined || value === null || value === '') return 'unset'
  return `${value} ${unit}`
}

function formatBool(value) {
  if (value === undefined || value === null) return 'unset'
  return value ? 'yes' : 'no'
}

function inputStyle() {
  return {
    background: 'var(--bg-primary)',
    borderColor: 'var(--border)',
    color: 'var(--text-primary)',
  }
}
