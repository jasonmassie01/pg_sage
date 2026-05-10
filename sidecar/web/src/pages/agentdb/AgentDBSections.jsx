import {
  Archive, Bot, Database, DollarSign, RefreshCw,
  Send, ShieldCheck, Trash2, Wrench,
} from 'lucide-react'
import { EmptyState } from '../../components/EmptyState'
import { BackupAssurancePanel } from './BackupAssurancePanel'
import { CloudProvisioningPanel } from './CloudProvisioningPanel'
import { AuditEventsPanel } from './AuditEventsPanel'
import { PromotionPanel } from './PromotionPanel'
import { RecommendationList } from './RecommendationList'
import { OptionGroup, SelectField, TextField } from './AgentDBFormControls'

const WORKLOADS = [
  { key: 'vector', label: 'Vector' },
  { key: 'postgis', label: 'PostGIS' },
  { key: 'jsonb', label: 'JSONB' },
]

const EXTENSIONS = [
  { key: 'pgvector', label: 'pgvector' },
  { key: 'postgis', label: 'postgis' },
  { key: 'pg_stat_statements', label: 'pg_stat_statements' },
]

const PROVIDERS = [
  { key: 'local_postgres', label: 'Local Postgres' },
  { key: 'aws_rds', label: 'AWS RDS' },
  { key: 'gcp_cloudsql', label: 'Cloud SQL' },
  { key: 'databricks_lakebase', label: 'Lakebase' },
]

function toggleValue(values, value) {
  return values.includes(value)
    ? values.filter(v => v !== value)
    : [...values, value]
}

function formatDate(value) {
  if (!value) return 'n/a'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return 'n/a'
  return date.toLocaleString()
}

function statusColor(status) {
  switch (status) {
    case 'active':
      return 'var(--green)'
    case 'archived':
      return 'var(--yellow)'
    case 'budget_exceeded':
      return 'var(--red)'
    default:
      return 'var(--text-secondary)'
  }
}

export function SummaryRow({ summary }) {
  const items = [
    { label: 'Deployments', value: summary.total, icon: Database },
    { label: 'Active', value: summary.active, icon: Bot },
    { label: 'Archived', value: summary.archived, icon: Archive },
    { label: 'Budget holds', value: summary.budget, icon: DollarSign },
  ]
  return (
    <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
      {items.map(item => (
        <div key={item.label} className="rounded border p-3"
          style={{
            background: 'var(--bg-card)',
            borderColor: 'var(--border)',
          }}>
          <div className="flex items-center justify-between">
            <span className="text-xs" style={{ color: 'var(--text-secondary)' }}>
              {item.label}
            </span>
            <item.icon size={16} style={{ color: 'var(--text-secondary)' }} />
          </div>
          <div className="mt-2 text-2xl font-semibold"
            style={{ color: 'var(--text-primary)' }}>
            {item.value}
          </div>
        </div>
      ))}
    </div>
  )
}

export function ProvisionForm({ form, busy, profiles, onChange, onSubmit }) {
  function update(key, value) {
    const next = { ...form, [key]: value }
    if (key === 'provider' && value !== 'local_postgres') {
      next.provisioning_level = 'instance'
    }
    if (key === 'provider' && value === 'local_postgres' &&
      form.provisioning_level === 'instance') {
      next.provisioning_level = 'schema'
    }
    if (key === 'provider' && value === 'databricks_lakebase' &&
      !next.lakebase_mode) {
      next.lakebase_mode = 'autoscaling_branch'
    }
    onChange(next)
  }
  const levels = form.provider === 'local_postgres'
    ? [
      { key: 'schema', label: 'Schema' },
      { key: 'database', label: 'Database' },
    ]
    : [{ key: 'instance', label: 'Instance' }]
  const profileOptions = profiles.filter(profile =>
    profile.provider === form.provider &&
    profile.provisioning_level === form.provisioning_level)

  return (
    <form onSubmit={onSubmit} data-testid="agent-db-form"
      className="rounded border p-4 space-y-3"
      style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
      <div className="flex items-center gap-2">
        <Bot size={18} style={{ color: 'var(--accent)' }} />
        <h2 className="text-sm font-semibold"
          style={{ color: 'var(--text-primary)' }}>
          Provision
        </h2>
      </div>
      <TextField label="Tenant" value={form.tenant_id} tipKey="tenant_id"
        onChange={value => update('tenant_id', value)} />
      <TextField label="Agent" value={form.agent_id} tipKey="agent_id"
        onChange={value => update('agent_id', value)} />
      <TextField label="Run" value={form.run_id} tipKey="run_id"
        onChange={value => update('run_id', value)} />
      <SelectField label="Provider" value={form.provider} options={PROVIDERS}
        tipKey="provider"
        onChange={value => update('provider', value)} />
      <SelectField label="Level" value={form.provisioning_level}
        options={levels}
        tipKey="provisioning_level"
        onChange={value => update('provisioning_level', value)} />
      {form.provider === 'aws_rds' && (
        <div className="grid grid-cols-2 gap-3">
          <TextField label="AWS region" value={form.cloud_region}
            tipKey="cloud_region"
            onChange={value => update('cloud_region', value)} />
          <TextField label="AWS account" value={form.cloud_account}
            tipKey="cloud_account"
            onChange={value => update('cloud_account', value)} />
        </div>
      )}
      {form.provider === 'gcp_cloudsql' && (
        <div className="grid grid-cols-2 gap-3">
          <TextField label="GCP project" value={form.cloud_project}
            tipKey="cloud_project"
            onChange={value => update('cloud_project', value)} />
          <TextField label="Cloud SQL region" value={form.cloud_region}
            tipKey="cloud_region"
            onChange={value => update('cloud_region', value)} />
        </div>
      )}
      <SelectField label="Size" value={form.size_profile_id}
        options={profileOptions.map(profile => ({
          key: profile.profile_id,
          label: profile.name,
        }))}
        tipKey="size_profile_id"
        onChange={value => update('size_profile_id', value)} />
      {form.provider === 'databricks_lakebase' && (
        <>
          <TextField label="Lakebase project" value={form.lakebase_project || ''}
            tipKey="lakebase_project"
            onChange={value => update('lakebase_project', value)} />
          <TextField label="Databricks workspace" value={form.cloud_workspace}
            tipKey="cloud_workspace"
            onChange={value => update('cloud_workspace', value)} />
          <SelectField label="Lakebase shape"
            value={form.lakebase_mode || 'autoscaling_branch'}
            tipKey="lakebase_mode"
            options={[
              { key: 'autoscaling_branch', label: 'Branch' },
              { key: 'provisioned_instance', label: 'Full instance' },
            ]}
            onChange={value => update('lakebase_mode', value)} />
          {(form.lakebase_mode || 'autoscaling_branch') ===
            'autoscaling_branch' && (
            <TextField label="Lakebase source instance"
              value={form.lakebase_source_instance || ''}
              tipKey="lakebase_source_instance"
              onChange={value => update('lakebase_source_instance', value)} />
          )}
        </>
      )}
      <TextField label="Schema" value={form.schema_name} tipKey="schema_name"
        onChange={value => update('schema_name', value)} />
      <TextField label="Database" value={form.database_name} tipKey="database_name"
        onChange={value => update('database_name', value)} />
      <div className="grid grid-cols-2 gap-3">
        <TextField label="Budget USD" value={form.budget_usd} tipKey="budget_usd"
          onChange={value => update('budget_usd', value)} />
        <TextField label="Lease seconds" value={form.lease_seconds}
          tipKey="lease_seconds"
          onChange={value => update('lease_seconds', value)} />
      </div>
      <OptionGroup label="Workloads" options={WORKLOADS}
        values={form.workload_types}
        tipKey="workload_types"
        onToggle={value => update(
          'workload_types', toggleValue(form.workload_types, value),
        )} />
      <OptionGroup label="Extensions" options={EXTENSIONS}
        values={form.extensions}
        tipKey="extensions"
        onToggle={value => update(
          'extensions', toggleValue(form.extensions, value),
        )} />
      <button type="submit" disabled={busy}
        data-testid="agent-db-submit"
        className="inline-flex items-center gap-2 rounded px-3 py-2 text-sm"
        style={{ background: 'var(--accent)', color: '#fff' }}>
        <Send size={15} />
        {busy ? 'Provisioning' : 'Provision'}
      </button>
    </form>
  )
}

export function DeploymentList({
  deployments, selectedID, onSelect, onLifecycle,
}) {
  if (deployments.length === 0) {
    return (
      <div className="rounded border" style={{ borderColor: 'var(--border)' }}>
        <EmptyState message="No agent databases" icon={Bot} />
      </div>
    )
  }
  return (
    <section className="space-y-2" data-testid="agent-db-list">
      {deployments.map(dep => (
        <article key={dep.deployment_id}
          data-testid="agent-db-row"
          className="rounded border p-3"
          style={{
            background: selectedID === dep.deployment_id
              ? 'var(--bg-hover)' : 'var(--bg-card)',
            borderColor: selectedID === dep.deployment_id
              ? 'var(--accent)' : 'var(--border)',
          }}>
          <button type="button" onClick={() => onSelect(dep.deployment_id)}
            className="w-full text-left">
            <div className="flex items-start justify-between gap-3">
              <div className="min-w-0">
                <h3 className="truncate text-sm font-medium"
                  style={{ color: 'var(--text-primary)' }}>
                  {dep.deployment_id}
                </h3>
                <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
                  {dep.tenant_id} / {dep.agent_id}
                </p>
                <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
                  {dep.provider || 'local_postgres'} /{' '}
                  {dep.provisioning_level || dep.isolation_type}
                </p>
              </div>
              <span className="text-xs font-medium"
                style={{ color: statusColor(dep.status) }}>
                {dep.status}
              </span>
            </div>
          </button>
          <div className="mt-3 flex flex-wrap gap-2">
            <IconButton label="Ping" icon={RefreshCw}
              onClick={() => onLifecycle(dep.deployment_id, 'ping')} />
            <IconButton label="Archive" icon={Archive}
              onClick={() => onLifecycle(dep.deployment_id, 'archive')} />
            <IconButton label="Restore" icon={ShieldCheck}
              onClick={() => onLifecycle(dep.deployment_id, 'restore')} />
            <IconButton label="Delete" icon={Trash2}
              onClick={() => onLifecycle(dep.deployment_id, 'delete')} />
          </div>
        </article>
      ))}
    </section>
  )
}

function IconButton({ label, icon, onClick, disabled = false }) {
  const ButtonIcon = icon
  return (
    <button type="button" onClick={onClick} title={label} disabled={disabled}
      className="inline-flex items-center gap-1.5 rounded border px-2 py-1 text-xs"
      style={{ borderColor: 'var(--border)', color: 'var(--text-secondary)' }}>
      <ButtonIcon size={13} />
      {label}
    </button>
  )
}

export function DeploymentDetail({
  deployment,
  detail,
  busy = false,
  onProvisionPreflight,
  onProvisionExecute,
  onProvisionExecuteLive,
  onProvisionStatus,
  onProvisionDestroyDryRun,
  onProvisionDestroyLive,
  onBackupCheck,
  onRestoreDrillDryRun,
  onMarkRestoreVerified,
  onCreateDeployRequest,
  onApproveDeployRequest,
  onDenyDeployRequest,
  onRequestDeployReview,
}) {
  if (!deployment) {
    return (
      <section className="rounded border" style={{ borderColor: 'var(--border)' }}>
        <EmptyState message="No deployment selected" icon={Database} />
      </section>
    )
  }
  const provider = deployment.provider || 'local_postgres'
  const level = deployment.provisioning_level || deployment.isolation_type
  const canProvisionCloudInstance = provider !== 'local_postgres' &&
    level === 'instance'
  return (
    <section className="rounded border p-4 space-y-4"
      data-testid="agent-db-detail"
      style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
      <div>
        <h2 className="text-sm font-semibold"
          style={{ color: 'var(--text-primary)' }}>
          {deployment.deployment_id}
        </h2>
        <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
          Lease expires {formatDate(deployment.lease_expires_at)}
        </p>
      </div>
      <div className="grid gap-3 md:grid-cols-3">
        <Metric label="Cost" value={`$${(detail.cost?.total_usd || 0).toFixed(2)}`} />
        <Metric label="Budget" value={detail.cost?.budget_state || 'n/a'} />
        <Metric label="Backups" value={detail.backups.length} />
      </div>
      <div className="grid gap-3 md:grid-cols-3">
        <Metric label="Provider" value={provider} />
        <Metric label="Level" value={level} />
        <Metric label="Provisioning" value={deployment.provisioning_status || 'registered'} />
      </div>
      {canProvisionCloudInstance && (
        <CloudProvisioningPanel
          attempts={detail.attempts}
          busy={busy}
          onPreflight={() => onProvisionPreflight?.(deployment.deployment_id)}
          onExecute={() => onProvisionExecute?.(deployment.deployment_id)}
          onExecuteLive={() =>
            onProvisionExecuteLive?.(deployment.deployment_id)}
          onStatus={() => onProvisionStatus?.(deployment.deployment_id)}
          onDestroyDryRun={
            () => onProvisionDestroyDryRun?.(deployment.deployment_id)
          }
          onDestroyLive={
            () => onProvisionDestroyLive?.(deployment.deployment_id)
          }
        />
      )}
      <BackupAssurancePanel
        backups={detail.backups}
        busy={busy}
        onCheckBackups={() => onBackupCheck?.(deployment.deployment_id)}
        onPlanRestoreDrill={
          () => onRestoreDrillDryRun?.(deployment.deployment_id)
        }
        onMarkRestoreVerified={
          () => onMarkRestoreVerified?.(deployment.deployment_id)
        }
      />
      <HintList hints={detail.hints} />
      <RecommendationList recommendations={detail.recommendations} />
      <PromotionPanel
        deployRequests={detail.deployRequests}
        busy={busy}
        onCreate={payload => onCreateDeployRequest?.(
          deployment.deployment_id,
          payload,
        )}
        onApprove={requestID => onApproveDeployRequest?.(
          deployment.deployment_id,
          requestID,
        )}
        onDeny={requestID => onDenyDeployRequest?.(
          deployment.deployment_id,
          requestID,
        )}
        onRequestReview={requestID => onRequestDeployReview?.(
          deployment.deployment_id,
          requestID,
        )}
      />
      <AuditEventsPanel events={detail.auditEvents} />
    </section>
  )
}

function Metric({ label, value }) {
  return (
    <div className="rounded border p-3"
      style={{ borderColor: 'var(--border)' }}>
      <div className="text-xs" style={{ color: 'var(--text-secondary)' }}>
        {label}
      </div>
      <div className="mt-1 text-sm font-semibold"
        style={{ color: 'var(--text-primary)' }}>
        {value}
      </div>
    </div>
  )
}

function HintList({ hints }) {
  return (
    <div>
      <div className="mb-2 flex items-center gap-2">
        <Wrench size={15} style={{ color: 'var(--text-secondary)' }} />
        <h3 className="text-xs font-semibold"
          style={{ color: 'var(--text-secondary)' }}>
          Tuning hints
        </h3>
      </div>
      <div className="space-y-2">
        {hints.map(hint => (
          <div key={hint.hint_id} className="rounded border p-2"
            style={{ borderColor: 'var(--border)' }}>
            <div className="text-sm" style={{ color: 'var(--text-primary)' }}>
              {hint.title}
            </div>
            <div className="text-xs" style={{ color: 'var(--text-secondary)' }}>
              {hint.kind}: {hint.detail}
            </div>
          </div>
        ))}
      </div>
    </div>
  )
}

export function RequestQueue({ requests, busy = false, onProvisionApproved }) {
  return (
    <section className="rounded border p-4"
      data-testid="agent-db-requests"
      style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
      <h2 className="mb-3 text-sm font-semibold"
        style={{ color: 'var(--text-primary)' }}>
        Requests
      </h2>
      <div className="space-y-2">
        {requests.map(req => (
          <div key={req.request_id} className="rounded border p-2"
            style={{ borderColor: 'var(--border)' }}>
            <div className="flex items-center justify-between gap-2">
              <span className="truncate text-sm"
                style={{ color: 'var(--text-primary)' }}>
                {req.request_id}
              </span>
              <span className="text-xs" style={{ color: 'var(--text-secondary)' }}>
                {req.status}
              </span>
            </div>
            <div className="text-xs" style={{ color: 'var(--text-secondary)' }}>
              {req.policy_decision} / {req.requested_isolation_type}
            </div>
            {req.status === 'approved' && (
              <button type="button" disabled={busy}
                data-testid="agent-db-request-provision"
                className="mt-2 rounded border px-2 py-1 text-xs"
                style={{
                  borderColor: 'var(--border)',
                  color: 'var(--accent)',
                }}
                onClick={() => onProvisionApproved?.(req.request_id)}>
                Provision
              </button>
            )}
          </div>
        ))}
      </div>
    </section>
  )
}
