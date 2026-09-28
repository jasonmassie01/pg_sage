import { useEffect, useMemo, useRef, useState } from 'react'
import { Archive, RefreshCw } from 'lucide-react'
import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { SummaryRow } from './agentdb/AgentDBSections'
import { AgentDBWorkspaceTabs } from './agentdb/AgentDBWorkspaceTabs'
import { AgentDBWorkspace } from './agentdb/AgentDBWorkspace'
import { useAgentDBDetail } from './agentdb/useAgentDBDetail'
import { hostedSecretReference } from './agentdb/hostedMetadata'
import {
  backupActionStatus,
  deploymentID,
  provisionActionLabel,
  provisionMetadata,
  uniqueDerivedID,
} from './agentdb/agentDBFormHelpers'
import {
  attestRestoreDrill,
  fetchDeploymentPage,
  mergeDeployments,
  runLiveProvisionAction,
} from './agentdb/agentDBLiveActions'

// How many deployment refetches to wait for an optimistic selection to appear
// before falling back to the first available deployment.
const PENDING_SELECTION_MAX_REFRESHES = 2

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
  cloud_region: '',
  cloud_account: '',
  cloud_project: '',
  cloud_workspace: '',
  lakebase_mode: 'autoscaling_branch',
  lakebase_project: '',
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

function designRequestBody(form, provider, budget) {
  return {
    tenant_id: form.tenant_id || 'tenant_agent',
    agent_id: form.agent_id || 'agent_runner',
    run_id: form.run_id,
    purpose: form.purpose,
    provider,
    requested_isolation_type: 'instance',
    database_name: form.database_name,
    budget_usd: Number(form.budget_usd || budget || 0),
    backup_required: true,
  }
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
  const [pendingSelectionID, setPendingSelectionID] = useState(null)
  const [detailFocusKey, setDetailFocusKey] = useState(0)
  const [form, setForm] = useState(initialForm)
  const [profileForm, setProfileForm] = useState(initialProfile)
  const [busy, setBusy] = useState(false)
  const [message, setMessage] = useState(null)
  const [messageDeploymentID, setMessageDeploymentID] = useState(null)
  const [error, setError] = useState(null)
  const [extraDeployments, setExtraDeployments] = useState([])
  const [extraCursor, setExtraCursor] = useState(null)

  const deployments = useMemo(
    () => mergeDeployments(deploymentsAPI.data?.deployments || [], extraDeployments),
    [deploymentsAPI.data, extraDeployments],
  )
  const nextCursor = extraCursor ?? deploymentsAPI.data?.next_cursor ?? ''
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

  const pendingWaitRef = useRef(0)

  useEffect(() => {
    if (deployments.length === 0) {
      if (selectedID) setSelectedID(null)
      if (pendingSelectionID) setPendingSelectionID(null)
      return
    }
    const selectedExists = deployments.some(d => d.deployment_id === selectedID)
    if (selectedExists) {
      if (pendingSelectionID === selectedID) setPendingSelectionID(null)
      return
    }
    // The selected deployment is not in the list. If it is an optimistic
    // pending selection, wait a bounded number of refetches for it to land so
    // a backend that never returns the new deployment can't pin us to a
    // non-existent row indefinitely.
    if (selectedID && selectedID === pendingSelectionID) {
      pendingWaitRef.current += 1
      if (pendingWaitRef.current <= PENDING_SELECTION_MAX_REFRESHES) return
      setPendingSelectionID(null)
    }
    setSelectedID(deployments[0].deployment_id)
  }, [deployments, pendingSelectionID, selectedID])

  const selected = deployments.find(d => d.deployment_id === selectedID)
  const detail = useAgentDBDetail(selectedID)

  const summary = useMemo(() => summarize(deployments), [deployments])

  // Only surface the "View details" jump while its deployment is reachable —
  // either already in the list or still pending its first appearance.
  const messageTarget = messageDeploymentID && (
    deployments.some(d => d.deployment_id === messageDeploymentID) ||
    messageDeploymentID === pendingSelectionID
  ) ? messageDeploymentID : null

  function clearStatus() {
    setError(null)
    setMessage(null)
    setMessageDeploymentID(null)
  }

  function showMessage(text, deploymentID = null) {
    setMessage(text)
    setMessageDeploymentID(deploymentID)
  }

  function focusDeployment(id) {
    pendingWaitRef.current = 0
    setPendingSelectionID(id)
    setSelectedID(id)
    setActiveTab('deployments')
    setDetailFocusKey(key => key + 1)
  }

  function selectDeployment(id) {
    setPendingSelectionID(null)
    setSelectedID(id)
    setDetailFocusKey(key => key + 1)
  }

  function viewMessageDeployment() {
    if (messageTarget) focusDeployment(messageTarget)
  }

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

  // obtainApprovedRequest asks request policy for an approval. It returns
  // the approved request, or null after reporting that a human must decide.
  async function obtainApprovedRequest(requestBody, idempotencyKey) {
    const request = await postJSON('/api/v1/agent-dbs/requests',
      requestBody, { 'Idempotency-Key': idempotencyKey })
    if (request.status !== 'approved') {
      showMessage(`Request ${request.status}: ${request.policy_decision}`)
      await refreshAll()
      return null
    }
    if (!request.request_id) throw new Error('Request response has no request_id')
    return request
  }

  // D4: an approved blueprint or template is a reviewed design, not
  // permission to spend; each cloud plan consumes its own approved request.
  async function designRequest(designID, provider, budget) {
    const tenant = form.tenant_id || 'tenant_agent'
    const agent = form.agent_id || 'agent_runner'
    return obtainApprovedRequest(designRequestBody(form, provider, budget),
      `ui-${tenant}-${agent}-${designID}-${Date.now().toString(36)}`)
  }

  async function submitProvision(event) {
    event.preventDefault()
    setBusy(true)
    clearStatus()
    try {
      const secretReference = hostedSecretReference(form)
      const requestBody = {
        tenant_id: form.tenant_id,
        agent_id: form.agent_id,
        run_id: form.run_id,
        purpose: form.purpose,
        provider: form.provider,
        requested_isolation_type: form.provisioning_level,
        database_name: form.database_name,
        budget_usd: Number(form.budget_usd || 0),
        backup_required: true,
      }
      const requestRun = form.run_id || Date.now().toString(36)
      const request = await obtainApprovedRequest(requestBody,
        `ui-${form.tenant_id}-${form.agent_id}-${requestRun}`)
      if (!request) return
      // D4: provisioning consumes the approved request exactly once; the
      // server checks that tenant, agent and provider match the approval.
      const generatedID = deploymentID(form)
      const provisionURL = '/api/v1/agent-dbs/requests/' +
        `${encodeURIComponent(request.request_id)}/provision`
      const created = await postJSON(provisionURL, {
        ...secretReference,
        deployment_id: generatedID,
        tenant_id: form.tenant_id,
        agent_id: form.agent_id,
        provider: form.provider,
        provisioning_level: form.provisioning_level,
        schema_name: form.schema_name,
        size_profile_id: form.size_profile_id,
        lease_seconds: Number(form.lease_seconds || 3600),
        metadata: provisionMetadata(form),
      })
      const id = created.deployment_id || generatedID
      focusDeployment(id)
      showMessage(`Provisioned ${id}`, id)
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
    clearStatus()
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
      showMessage('Size profile saved')
      await profilesAPI.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function saveProviderSettings(provider, payload) {
    setBusy(true)
    clearStatus()
    try {
      await postJSON(`/api/v1/agent-dbs/provider-configs/${provider}`, payload)
      showMessage('Provider settings saved')
      await providerConfigsAPI.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function uploadTerraformTemplate(payload) {
    setBusy(true)
    clearStatus()
    try {
      await postJSON('/api/v1/agent-dbs/terraform-templates', payload)
      showMessage('Terraform template uploaded')
      await terraformTemplatesAPI.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function generateBlueprint(payload) {
    setBusy(true)
    clearStatus()
    try {
      await postJSON('/api/v1/agent-dbs/blueprints', payload)
      showMessage('Blueprint generated')
      await blueprintsAPI.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function approveBlueprint(blueprintID) {
    setBusy(true)
    clearStatus()
    try {
      await postJSON(`/api/v1/agent-dbs/blueprints/${blueprintID}/approve`, {
        approved_by: 'operator',
      })
      showMessage('Blueprint approved')
      await blueprintsAPI.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function provisionBlueprint(blueprint) {
    setBusy(true)
    clearStatus()
    try {
      const request = await designRequest(blueprint.blueprint_id,
        blueprint.provider || form.provider, blueprint.blueprint?.budget_usd)
      if (!request) return
      const generatedID = uniqueDerivedID(blueprint.blueprint_id)
      const created = await postJSON(
        `/api/v1/agent-dbs/blueprints/${blueprint.blueprint_id}/provision`,
        {
          request_id: request.request_id,
          deployment_id: generatedID,
          tenant_id: form.tenant_id || 'tenant_agent',
          agent_id: form.agent_id || 'agent_runner',
          run_id: form.run_id,
          database_name: form.database_name,
          lease_seconds: Number(form.lease_seconds || 3600),
        },
      )
      const id = created.deployment_id || generatedID
      focusDeployment(id)
      showMessage(`Blueprint provisioned as ${id}`, id)
      await refreshAll()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function approveTerraformTemplate(templateID) {
    setBusy(true)
    clearStatus()
    try {
      await postJSON(
        `/api/v1/agent-dbs/terraform-templates/${templateID}/approve`,
        { approved_by: 'operator' },
      )
      showMessage('Terraform template approved')
      await terraformTemplatesAPI.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function provisionTerraformTemplate(template) {
    setBusy(true)
    clearStatus()
    try {
      const provider = template.provider || form.provider
      if (provider === 'local_postgres') {
        throw new Error('Select a cloud provider before provisioning Terraform')
      }
      const request = await designRequest(template.template_id, provider)
      if (!request) return
      const generatedID = uniqueDerivedID(template.template_id)
      const created = await postJSON(
        `/api/v1/agent-dbs/terraform-templates/${template.template_id}/provision`,
        {
          request_id: request.request_id,
          deployment_id: generatedID,
          tenant_id: form.tenant_id || 'tenant_agent',
          agent_id: form.agent_id || 'agent_runner',
          provider,
          provisioning_level: 'instance',
          database_name: form.database_name,
          lease_seconds: Number(form.lease_seconds || 3600),
        },
      )
      const id = created.deployment_id || generatedID
      focusDeployment(id)
      showMessage(`Terraform template provisioned as ${id}`, id)
      await refreshAll()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function lifecycle(id, action) {
    clearStatus()
    try {
      if (['archive', 'delete'].includes(action) &&
        window.confirm &&
        !window.confirm(`${action} ${id}?`)) {
        return
      }
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
    clearStatus()
    try {
      const result = await postJSON('/api/v1/agent-dbs/cleanup', {})
      showMessage(`Archived ${result.archived?.length || 0}`)
      await refreshAll()
    } catch (err) {
      setError(err.message)
    }
  }

  async function reconcileAbandoned() {
    setBusy(true)
    clearStatus()
    try {
      if (window.confirm && !window.confirm(
        'Reconcile abandoned deployments? Archived live cloud databases may be destroyed.',
      )) {
        return
      }
      const result = await postJSON('/api/v1/agent-dbs/reconcile', {})
      const archived = result.archived?.length || 0
      const destroyDryRun = result.destroy_dry_run?.length || 0
      const destroyLive = result.destroy_live?.length || 0
      showMessage([
        `Reconciled ${archived} archived`,
        `${destroyDryRun} destroy dry-run`,
        `${destroyLive} live destroy`,
      ].join(', '))
      await refreshAll()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function loadMoreDeployments() {
    clearStatus()
    try {
      const page = await fetchDeploymentPage(nextCursor)
      setExtraDeployments(prev => [...prev, ...(page.deployments || [])])
      setExtraCursor(page.next_cursor || '')
    } catch (err) {
      setError(err.message)
    }
  }

  async function runProvisionAction(id, action) {
    setBusy(true)
    clearStatus()
    try {
      const live = action === 'live-execute' || action === 'destroy-live'
      const confirmFn = window.confirm ? msg => window.confirm(msg) : null
      const attempt = live
        ? await runLiveProvisionAction(postJSON, id, action, confirmFn)
        : await postJSON(`/api/v1/agent-dbs/${id}/provision/${action}`, {})
      if (!attempt) return
      const status = attempt.status || 'recorded'
      showMessage(`Provision ${provisionActionLabel(action)} ${status}`)
      await detail.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function runBackupCheck(id) {
    setBusy(true)
    clearStatus()
    try {
      const result = await postJSON(`/api/v1/agent-dbs/${id}/backups/check`, {})
      showMessage(`Backup check ${backupActionStatus(result)}`)
      await detail.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function planRestoreDrill(id) {
    setBusy(true)
    clearStatus()
    try {
      const attempt = await postJSON(
        `/api/v1/agent-dbs/${id}/backups/restore-drill-dry-run`,
        {},
      )
      showMessage(`Restore drill dry-run ${attempt.status || 'planned'}`)
      await detail.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function markRestoreVerified(id) {
    const evidence = window.prompt
      ? window.prompt('Restore drill evidence URI (admin attestation)')
      : ''
    if (!evidence) return
    setBusy(true)
    clearStatus()
    try {
      const backup = await attestRestoreDrill(postJSON, id, evidence)
      showMessage(`Restore verification ${backup.status || 'recorded'}`)
      await detail.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function createDeployRequest(id, payload) {
    setBusy(true)
    clearStatus()
    try {
      await postJSON(`/api/v1/agent-dbs/${id}/deploy-requests`, payload)
      showMessage('Promotion draft recorded')
      await detail.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function reviewDeployRequest(id, requestID, decision) {
    setBusy(true)
    clearStatus()
    try {
      await postJSON(
        `/api/v1/agent-dbs/${id}/deploy-requests/${requestID}/${decision}`,
        {
          reviewed_by: 'operator',
          review_reason: `${decision} in pg_sage UI`,
        },
      )
      showMessage(decision === 'approve'
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
    clearStatus()
    try {
      await postJSON(
        `/api/v1/agent-dbs/${id}/deploy-requests/${requestID}/request-review`,
        {},
      )
      showMessage('Promotion submitted for review')
      await detail.refetch()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function provisionApprovedAgentRequest(requestID) {
    setBusy(true)
    clearStatus()
    try {
      const generatedID = uniqueDerivedID(requestID)
      const created = await postJSON(
        `/api/v1/agent-dbs/requests/${requestID}/provision`,
        {
          deployment_id: generatedID,
          lease_seconds: Number(form.lease_seconds || 3600),
        },
      )
      const id = created.deployment_id || generatedID
      focusDeployment(id)
      showMessage(`Approved agent request provisioned as ${id}`, id)
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
        <div
          className={[
            'flex flex-wrap items-center justify-between gap-2 rounded border',
            'px-3 py-2 text-sm',
          ].join(' ')}
          data-testid="agent-db-message"
          style={{
            color: 'var(--green)',
            borderColor: 'rgba(52,211,153,0.35)',
            background: 'rgba(52,211,153,0.08)',
          }}>
          <span>{message}</span>
          {messageTarget && (
            <button type="button"
              data-testid="agent-db-view-deployment"
              onClick={viewMessageDeployment}
              className="rounded border px-2 py-1 text-xs"
              style={{
                borderColor: 'rgba(52,211,153,0.45)',
                color: 'var(--green)',
              }}>
              View details
            </button>
          )}
        </div>
      )}

      {nextCursor && (
        <button type="button" onClick={loadMoreDeployments}
          data-testid="agent-db-load-more"
          className="rounded border px-3 py-2 text-sm"
          style={{ borderColor: 'var(--border)', color: 'var(--text-primary)' }}>
          Load more deployments
        </button>
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
        detailFocusKey={detailFocusKey}
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
        onMarkRestoreVerified={markRestoreVerified}
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
        onSelectDeployment={selectDeployment}
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
