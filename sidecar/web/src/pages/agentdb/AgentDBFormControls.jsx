import * as Tooltip from '@radix-ui/react-tooltip'
import { Info } from 'lucide-react'

const FIELD_HELP = {
  tenant_id: 'Logical tenant or owner for the agent-created database.',
  agent_id: 'Stable agent identity that owns this deployment request.',
  run_id: 'Optional run or workflow identifier for traceability.',
  provider: 'Where pg_sage should provision the database. Cloud providers are instance-level.',
  provisioning_level: 'Isolation boundary. Local supports schema or database; cloud uses instance.',
  size_profile_id: 'Reusable t-shirt size that supplies provider capacity and cloud parameters.',
  cloud_region: 'Cloud region where the provider should create the live instance or branch.',
  cloud_account: 'Optional AWS account guardrail used by policy and audit evidence.',
  cloud_project: 'GCP project ID used for Cloud SQL provisioning and policy checks.',
  cloud_workspace: 'Optional Databricks workspace or account reference used for policy checks.',
  schema_name: 'Optional local schema name. Empty lets pg_sage generate a safe name.',
  database_name: 'Optional database name. For cloud providers this becomes the application database.',
  budget_usd: 'Monthly spend guardrail used for tracking and cleanup decisions.',
  lease_seconds: 'Required heartbeat window. Expired leases are archived and cleaned up safely.',
  workload_types: 'Expected workload shapes used to seed tuning hints.',
  extensions: 'Extensions the agent expects so pg_sage can warn about support and tuning gaps.',
  lakebase_mode: 'Choose an autoscaling Lakebase branch for lightweight agent work or a full instance when isolation demands it.',
  lakebase_project: 'Lakebase project or database group that contains the source branch or instance.',
  lakebase_source_instance: 'The existing Lakebase instance or branch path used as the source for a new branch.',
  provider_settings_provider: 'Cloud provider whose live-provisioning settings will be edited.',
  provider_enabled: 'Allows this provider to pass policy checks when global live provisioning is enabled.',
  provider_settings_json: 'JSON policy and credential-reference settings. Secrets are stripped before saving.',
  terraform_template_id: 'Stable template key used by generated blueprints, agents, and approval flows.',
  terraform_provider: 'Cloud provider this Terraform template targets.',
  terraform_path: 'Terraform file path inside the uploaded template bundle.',
  terraform_content: 'Terraform source body. It is validated and stored as a draft, not applied directly.',
  blueprint_intent: 'Plain-English deployment request that the LLM translates into a typed blueprint.',
  blueprint_provider: 'Provider constraint for the blueprint generator.',
  blueprint_id: 'Stable blueprint key. Leave empty to let pg_sage derive one from the intent.',
  blueprint_name: 'Human readable blueprint name shown in review lists.',
  blueprint_created_by: 'Operator, agent, or workflow that requested the blueprint.',
}

export function FieldTip({ tipKey, children }) {
  const help = FIELD_HELP[tipKey]
  if (!help) return children
  return (
    <Tooltip.Provider delayDuration={200}>
      <Tooltip.Root>
        <Tooltip.Trigger asChild>
          <span className="inline-flex items-center gap-1 cursor-help"
            data-testid={`agent-db-provision-tip-${tipKey}`}
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

export function SelectField({ label, value, options, onChange, tipKey }) {
  return (
    <label className="block text-xs" style={{ color: 'var(--text-secondary)' }}>
      <FieldTip tipKey={tipKey}>
        <span>{label}</span>
      </FieldTip>
      <select value={value} onChange={e => onChange(e.target.value)}
        className="mt-1 w-full rounded border px-2 py-1.5 text-sm"
        style={{
          background: 'var(--bg-primary)',
          borderColor: 'var(--border)',
          color: 'var(--text-primary)',
        }}>
        {options.map(option => (
          <option key={option.key} value={option.key}>
            {option.label}
          </option>
        ))}
      </select>
    </label>
  )
}

export function TextField({ label, value, onChange, tipKey }) {
  return (
    <label className="block text-xs" style={{ color: 'var(--text-secondary)' }}>
      <FieldTip tipKey={tipKey}>
        <span>{label}</span>
      </FieldTip>
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

export function OptionGroup({ label, options, values, onToggle, tipKey }) {
  return (
    <fieldset>
      <legend className="mb-1 text-xs" style={{ color: 'var(--text-secondary)' }}>
        <FieldTip tipKey={tipKey}>
          <span>{label}</span>
        </FieldTip>
      </legend>
      <div className="flex flex-wrap gap-2">
        {options.map(option => (
          <label key={option.key}
            className="inline-flex items-center gap-1.5 rounded border px-2 py-1 text-xs"
            style={{ borderColor: 'var(--border)', color: 'var(--text-primary)' }}>
            <input type="checkbox" checked={values.includes(option.key)}
              onChange={() => onToggle(option.key)} />
            {option.label}
          </label>
        ))}
      </div>
    </fieldset>
  )
}
