import { useEffect, useMemo, useState } from 'react'
import { Archive, RefreshCw } from 'lucide-react'
import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { SummaryRow } from './agentdb/AgentDBSections'
import { AgentDBWorkspaceTabs } from './agentdb/AgentDBWorkspaceTabs'
import { AgentDBWorkspace } from './agentdb/AgentDBWorkspace'
import { useAgentDBDetail } from './agentdb/useAgentDBDetail'

const initialForm = {
  tenant_id: 'tenant_agent',
  agent_id: 'agent_runner',
  run_id: '',
  purpose: '',
  provider: 'local_postgres',
  provisioning_level: 'schema',
  schema_name: '',
  database_name: '',
  size_profile_id: 'local_schema_xs',
  budget_usd: '10',
  lease_seconds: '3600',
  workload_types: ['vector', 'jsonb'],
  extensions: ['pgvector', 'pg_stat_statements'],
  lakebase_mode: 'autoscaling_branch',
  lakebase_source_instance: '',
}

const initialProfile = {
  profile_id: '',
  provider: 'local_postgres',
  provisioning_level: 'schema',
  name: '',
  description: '',
  cpu: '1',
  memory_gb: '1',
  storage_gb: '5',
  max_connections: '20',
  monthly_budget_usd: '0',
  provider_params_text: '{}',
}

async function postJSON(url, body, headers = {}) {
  const res = await fetch(url, {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json', ...headers },
    body: JSON.stringify(body),
  })
  const data = await res.json().catch(() => ({}))
  if (!res.ok) {
    throw new Error(data.error || `Request failed: ${res.status}`)
  }
  return data
}

function deploymentID(form) {
  const stamp = Date.now().toString(36)
  return `${form.tenant_id}-${form.agent_id}-${stamp}`
    .toLowerCase()
    .replace(/[^a-z0-9_]+/g, '_')
    .replace(/^_+|_+$/g, '')
}

function provisionActionLabel(action) {
  if (action === 'destroy-dry-run') return 'destroy dry-run'
  return action.replaceAll('-', ' ')
}

function backupActionStatus(result) {
  return result.backup_status || result.attempt?.status || result.status || 'recorded'
}

function provisionMetadata(form) {
  const metadata = {
    purpose: form.purpose,
    workload_types: form.workload_types,
    extensions: form.extensions,
    lakebase_mode: form.lakebase_mode,
  }
  if (form.provider === 'databricks_lakebase' &&
    form.lakebase_mode !== 'provisioned_instance' &&
    form.lakebase_source_instance) {
    metadata.provider_params = {
      source_instance: form.lakebase_source_instance,
      source_branch: form.lakebase_source_instance,
    }
  }
  return metadata
}

export function AgentDBsPage() {
  const deploymentsAPI = useAPI('/api/v1/agent-dbs', 15000)
  const requestsAPI = useAPI('/api/v1/agent-dbs/requests', 15000)
  const profilesAPI = useAPI('/api/v1/agent-dbs/size-profiles', 30000)
  const providersAPI = useAPI('/api/v1/agent-dbs/providers', 30000)
  const providerConfigsAPI = useAPI('/api/v1/agent-dbs/provider-configs', 30000)
  const terraformTemplatesAPI = useAPI(
    '/api/v1/agent-dbs/terraform-templates',
    30000,
  )
  const blueprintsAPI = useAPI('/api/v1/agent-dbs/blueprints', 30000)
  const [activeTab, setActiveTab] = useState('deployments')
  const [selectedID, setSelectedID] = useState(null)
  const [form, setForm] = useState(initialForm)
  const [profileForm, setProfileForm] = useState(initialProfile)
  const [busy, setBusy] = useState(false)
  const [message, setMessage] = useState(null)
  const [error, setError] = useState(null)

  const deployments = useMemo(
    () => deploymentsAPI.data?.deployments || [],
    [deploymentsAPI.data],
  )
  const requests = useMemo(
    () => requestsAPI.data?.requests || [],
    [requestsAPI.data],
  )
  const profiles = useMemo(
    () => profilesAPI.data?.profiles || [],
    [profilesAPI.data],
  )
  const providers = useMemo(
    () => providersAPI.data?.providers || [],
    [providersAPI.data],
  )
  const providerConfigs = useMemo(
    () => providerConfigsAPI.data?.provider_configs || [],
    [providerConfigsAPI.data],
  )
  const terraformTemplates = useMemo(
    () => terraformTemplatesAPI.data?.terraform_templates ||
      terraformTemplatesAPI.data?.templates || [],
    [terraformTemplatesAPI.data],
  )
  const blueprints = useMemo(
    () => blueprintsAPI.data?.blueprints || [],
    [blueprintsAPI.data],
  )

  useEffect(() => {
    if (!selectedID && deployments.length > 0) {
      setSelectedID(deployments[0].deployment_id)
    }
  }, [deployments, selectedID])

  const selected = deployments.find(d => d.deployment_id === selectedID)
  const detail = useAgentDBDetail(selectedID)

  const summary = useMemo(() => summarize(deployments), [deployments])

  async function refreshAll() {
    await Promise.all([
      deploymentsAPI.refetch(),
      requestsAPI.refetch(),
      profilesAPI.refetch(),
      providersAPI.refetch(),
      providerConfigsAPI.refetch(),
      terraformTemplatesAPI.refetch(),
      blueprintsAPI.refetch(),
      detail.refetch(),
    ])
  }

  async function submitProvision(event) {
    event.preventDefault()
    setBusy(true)
    setError(null)
    setMessage(null)
    try {
      const requestBody = {
        tenant_id: form.tenant_id,
        agent_id: form.agent_id,
        run_id: form.run_id,
        purpose: form.purpose,
        provider: form.provider,
        requested_isolation_type: form.provisioning_level,
        budget_usd: Number(form.budget_usd || 0),
        backup_required: true,
      }
      const idem = `ui-${form.tenant_id}-${form.agent_id}-${form.run_id}`
      const request = await postJSON('/api/v1/agent-dbs/requests',
        requestBody, { 'Idempotency-Key': idem })
      if (request.status !== 'approved') {
        setMessage(`Request ${request.status}: ${request.policy_decision}`)
        await refreshAll()
        return
      }
      const id = deploymentID(form)
      await postJSON('/api/v1/agent-dbs', {
        deployment_id: id,
        tenant_id: form.tenant_id,
        agent_id: form.agent_id,
        run_id: form.run_id,
        provider: form.provider,
        provisioning_level: form.provisioning_level,
        isolation_type: form.provisioning_level,
        database_name: form.database_name,
        schema_name: form.schema_name,
        size_profile_id: form.size_profile_id,
        budget_usd: Number(form.budget_usd || 0),
        backup_required: true,
        lease_seconds: Number(form.lease_seconds || 3600),
        metadata: provisionMetadata(form),
      })
      setSelectedID(id)
      setMessage('Provisioned')
      await refreshAll()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function submitProfile(event) {
    event.preventDefault()
    setBusy(true)
    setError(null)
    setMessage(null)
    try {
      const params = JSON.parse(profileForm.provider_params_text || '{}')
      await postJSON('/api/v1/agent-dbs/size-profiles', {
        profile_id: profileForm.profile_id,
        provider: profileForm.provider,
        provisioning_level: profileForm.provisioning_level,
        name: profileForm.name,
        description: profileForm.description,
        cpu: Number(profileForm.cpu || 0),
        memory_gb: Number(profileForm.memory_gb || 0),
        storage_gb: Number(profileForm.storage_gb || 0),
        max_connections: Number(profileForm.max_connections || 0),
        monthly_budget_usd: Number(profileForm.monthly_budget_usd || 0),
        provider_params: params,
      })
      setMessage('Size profile saved')
      await profilesAPI.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function saveProviderSettings(provider, payload) {
    setBusy(true)
    setError(null)
    setMessage(null)
    try {
      await postJSON(`/api/v1/agent-dbs/provider-configs/${provider}`, payload)
      setMessage('Provider settings saved')
      await providerConfigsAPI.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function uploadTerraformTemplate(payload) {
    setBusy(true)
    setError(null)
    setMessage(null)
    try {
      await postJSON('/api/v1/agent-dbs/terraform-templates', payload)
      setMessage('Terraform template uploaded')
      await terraformTemplatesAPI.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function generateBlueprint(payload) {
    setBusy(true)
    setError(null)
    setMessage(null)
    try {
      await postJSON('/api/v1/agent-dbs/blueprints', payload)
      setMessage('Blueprint generated')
      await blueprintsAPI.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function approveBlueprint(blueprintID) {
    setBusy(true)
    setError(null)
    setMessage(null)
    try {
      await postJSON(`/api/v1/agent-dbs/blueprints/${blueprintID}/approve`, {
        approved_by: 'operator',
      })
      setMessage('Blueprint approved')
      await blueprintsAPI.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function provisionBlueprint(blueprint) {
    setBusy(true)
    setError(null)
    setMessage(null)
    try {
      const id = `${blueprint.blueprint_id}_deployment`
      await postJSON(`/api/v1/agent-dbs/blueprints/${blueprint.blueprint_id}/provision`, {
        deployment_id: id,
        tenant_id: form.tenant_id || 'tenant_agent',
        agent_id: form.agent_id || 'agent_runner',
        run_id: form.run_id,
        database_name: form.database_name,
        lease_seconds: Number(form.lease_seconds || 3600),
      })
      setSelectedID(id)
      setActiveTab('deployments')
      setMessage('Blueprint provisioned as deployment')
      await refreshAll()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function approveTerraformTemplate(templateID) {
    setBusy(true)
    setError(null)
    setMessage(null)
    try {
      await postJSON(
        `/api/v1/agent-dbs/terraform-templates/${templateID}/approve`,
        { approved_by: 'operator' },
      )
      setMessage('Terraform template approved')
      await terraformTemplatesAPI.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function provisionTerraformTemplate(template) {
    setBusy(true)
    setError(null)
    setMessage(null)
    try {
      const id = `${template.template_id}_deployment`
      await postJSON(
        `/api/v1/agent-dbs/terraform-templates/${template.template_id}/provision`,
        {
          deployment_id: id,
          tenant_id: form.tenant_id || 'tenant_agent',
          agent_id: form.agent_id || 'agent_runner',
          provider: form.provider === 'local_postgres' ? 'aws_rds' : form.provider,
          provisioning_level: 'instance',
          database_name: form.database_name,
          lease_seconds: Number(form.lease_seconds || 3600),
        },
      )
      setSelectedID(id)
      setActiveTab('deployments')
      setMessage('Terraform template provisioned as deployment')
      await refreshAll()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function lifecycle(id, action) {
    setError(null)
    try {
      if (action === 'delete') {
        const res = await fetch(`/api/v1/agent-dbs/${id}`, {
          method: 'DELETE',
          credentials: 'include',
        })
        const data = await res.json().catch(() => ({}))
        if (!res.ok) throw new Error(data.error || 'Delete blocked')
      } else {
        await postJSON(`/api/v1/agent-dbs/${id}/${action}`, {})
      }
      await refreshAll()
    } catch (err) {
      setError(err.message)
    }
  }

  async function cleanupExpired() {
    setError(null)
    try {
      const result = await postJSON('/api/v1/agent-dbs/cleanup', {})
      setMessage(`Archived ${result.archived?.length || 0}`)
      await refreshAll()
    } catch (err) {
      setError(err.message)
    }
  }

  async function reconcileAbandoned() {
    setBusy(true)
    setError(null)
    setMessage(null)
    try {
      const result = await postJSON('/api/v1/agent-dbs/reconcile', {})
      const archived = result.archived?.length || 0
      const destroyDryRun = result.destroy_dry_run?.length || 0
      setMessage(
        `Reconciled ${archived} archived, ${destroyDryRun} destroy dry-run`,
      )
      await refreshAll()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function runProvisionAction(id, action) {
    setBusy(true)
    setError(null)
    setMessage(null)
    try {
      const endpointAction = action === 'live-execute' ? 'execute' : action
      const body = action === 'live-execute'
        ? {
          mode: 'live',
          live_enabled: true,
          provider_enabled: true,
          cost_estimate_id: `ui-${id}-${Date.now()}`,
        }
        : {}
      const attempt = await postJSON(
        `/api/v1/agent-dbs/${id}/provision/${endpointAction}`,
        body,
      )
      const status = attempt.status || 'recorded'
      setMessage(`Provision ${provisionActionLabel(action)} ${status}`)
      await detail.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function runBackupCheck(id) {
    setBusy(true)
    setError(null)
    setMessage(null)
    try {
      const result = await postJSON(`/api/v1/agent-dbs/${id}/backups/check`, {})
      setMessage(`Backup check ${backupActionStatus(result)}`)
      await detail.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function planRestoreDrill(id) {
    setBusy(true)
    setError(null)
    setMessage(null)
    try {
      const attempt = await postJSON(
        `/api/v1/agent-dbs/${id}/backups/restore-drill-dry-run`,
        {},
      )
      setMessage(`Restore drill dry-run ${attempt.status || 'planned'}`)
      await detail.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function createDeployRequest(id, payload) {
    setBusy(true)
    setError(null)
    setMessage(null)
    try {
      await postJSON(`/api/v1/agent-dbs/${id}/deploy-requests`, payload)
      setMessage('Promotion draft recorded')
      await detail.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function reviewDeployRequest(id, requestID, decision) {
    setBusy(true)
    setError(null)
    setMessage(null)
    try {
      await postJSON(
        `/api/v1/agent-dbs/${id}/deploy-requests/${requestID}/${decision}`,
        {
          reviewed_by: 'operator',
          review_reason: `${decision} in pg_sage UI`,
        },
      )
      setMessage(decision === 'approve'
        ? 'Promotion approved' : 'Promotion denied')
      await detail.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function requestDeployReview(id, requestID) {
    setBusy(true)
    setError(null)
    setMessage(null)
    try {
      await postJSON(
        `/api/v1/agent-dbs/${id}/deploy-requests/${requestID}/request-review`,
        {},
      )
      setMessage('Promotion submitted for review')
      await detail.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function provisionApprovedAgentRequest(requestID) {
    setBusy(true)
    setError(null)
    setMessage(null)
    try {
      const id = `${requestID}_deployment`
      await postJSON(`/api/v1/agent-dbs/requests/${requestID}/provision`, {
        deployment_id: id,
        lease_seconds: Number(form.lease_seconds || 3600),
      })
      setSelectedID(id)
      setActiveTab('deployments')
      setMessage('Approved agent request provisioned')
      await refreshAll()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  if (deploymentsAPI.loading && requestsAPI.loading) {
    return <LoadingSpinner />
  }

  if (deploymentsAPI.error) {
    return <ErrorBanner message={deploymentsAPI.error}
      onRetry={deploymentsAPI.refetch} />
  }

  return (
    <div className="space-y-4" data-testid="agent-dbs-page">
      <SummaryRow summary={summary} />

      <div className="flex justify-end gap-2">
        <button type="button" onClick={reconcileAbandoned}
          disabled={busy}
          data-testid="agent-db-reconcile"
          className="inline-flex items-center gap-2 rounded border px-3 py-2 text-sm"
          style={{ borderColor: 'var(--border)', color: 'var(--text-primary)' }}>
          <RefreshCw size={15} />
          Reconcile abandoned
        </button>
        <button type="button" onClick={cleanupExpired}
          disabled={busy}
          data-testid="agent-db-cleanup"
          className="inline-flex items-center gap-2 rounded border px-3 py-2 text-sm"
          style={{ borderColor: 'var(--border)', color: 'var(--text-primary)' }}>
          <Archive size={15} />
          Cleanup expired
        </button>
      </div>

      {error && (
        <ErrorBanner message={error} onRetry={() => setError(null)} />
      )}
      {message && (
        <div className="rounded border px-3 py-2 text-sm"
          style={{
            color: 'var(--green)',
            borderColor: 'rgba(52,211,153,0.35)',
            background: 'rgba(52,211,153,0.08)',
          }}>
          {message}
        </div>
      )}

      <AgentDBWorkspaceTabs activeTab={activeTab} onChange={setActiveTab} />

      <AgentDBWorkspace
        activeTab={activeTab}
        form={form}
        profileForm={profileForm}
        busy={busy}
        deployments={deployments}
        selected={selected}
        selectedID={selectedID}
        detail={detail}
        requests={requests}
        profiles={profiles}
        providers={providers}
        providerConfigs={providerConfigs}
        terraformTemplates={terraformTemplates}
        blueprints={blueprints}
        onFormChange={setForm}
        onProfileFormChange={setProfileForm}
        onSubmitProvision={submitProvision}
        onSubmitProfile={submitProfile}
        onLifecycle={lifecycle}
        onProvisionAction={runProvisionAction}
        onBackupCheck={runBackupCheck}
        onRestoreDrillDryRun={planRestoreDrill}
        onCreateDeployRequest={createDeployRequest}
        onReviewDeployRequest={reviewDeployRequest}
        onRequestDeployReview={requestDeployReview}
        onProvisionApprovedRequest={provisionApprovedAgentRequest}
        onSaveProviderSettings={saveProviderSettings}
        onUploadTerraformTemplate={uploadTerraformTemplate}
        onApproveTerraformTemplate={approveTerraformTemplate}
        onProvisionTerraformTemplate={provisionTerraformTemplate}
        onGenerateBlueprint={generateBlueprint}
        onApproveBlueprint={approveBlueprint}
        onProvisionBlueprint={provisionBlueprint}
        onSelectDeployment={setSelectedID}
      />
    </div>
  )
}

function summarize(deployments) {
  return deployments.reduce((acc, dep) => {
    acc.total += 1
    if (dep.status === 'active') acc.active += 1
    if (dep.status === 'archived') acc.archived += 1
    if (dep.status === 'budget_exceeded') acc.budget += 1
    return acc
  }, { total: 0, active: 0, archived: 0, budget: 0 })
}
