import { hostedMetadata } from './hostedMetadata'

export function deploymentID(form) {
  const stamp = Date.now().toString(36)
  return `${form.tenant_id}-${form.agent_id}-${stamp}`
    .toLowerCase()
    .replace(/[^a-z0-9_]+/g, '_')
    .replace(/^_+|_+$/g, '')
}

export function provisionActionLabel(action) {
  if (action === 'destroy-dry-run') return 'destroy dry-run'
  return action.replaceAll('-', ' ')
}

export function backupActionStatus(result) {
  return result.backup_status || result.attempt?.status || result.status || 'recorded'
}

export function provisionMetadata(form) {
  const metadata = {
    purpose: form.purpose,
    workload_types: form.workload_types,
    extensions: form.extensions,
    lakebase_mode: form.lakebase_mode,
  }
  const providerParams = {}
  if (form.provider === 'neon' || form.provider === 'supabase') {
    Object.assign(providerParams, hostedMetadata(form))
  }
  if (form.provider === 'aws_rds') {
    if (form.cloud_region) providerParams.region = form.cloud_region
    if (form.cloud_account) providerParams.account = form.cloud_account
  }
  if (form.provider === 'gcp_cloudsql') {
    if (form.cloud_project) providerParams.project = form.cloud_project
    if (form.cloud_region) providerParams.region = form.cloud_region
  }
  if (form.provider === 'databricks_lakebase' &&
    form.lakebase_mode !== 'provisioned_instance') {
    if (form.lakebase_project) providerParams.project = form.lakebase_project
    if (form.cloud_workspace) providerParams.workspace = form.cloud_workspace
    if (form.lakebase_source_instance) {
      providerParams.source_instance = form.lakebase_source_instance
    }
  }
  if (Object.keys(providerParams).length > 0) {
    metadata.provider_params = providerParams
  }
  return metadata
}

export function uniqueDerivedID(prefix) {
  return `${prefix}_${Date.now().toString(36)}_deployment`
    .toLowerCase()
    .replace(/[^a-z0-9_]+/g, '_')
    .replace(/^_+|_+$/g, '')
}
