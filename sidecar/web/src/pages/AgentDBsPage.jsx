import { useEffect, useMemo, useState } from 'react'
import { Archive, RefreshCw } from 'lucide-react'
import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import {
  DeploymentDetail, DeploymentList, ProvisionForm, RequestQueue, SummaryRow,
} from './agentdb/AgentDBSections'
import {
  ProfilePanel, ProviderReadinessPanel,
} from './agentdb/AgentDBProvisioningPanels'

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

export function AgentDBsPage() {
  const deploymentsAPI = useAPI('/api/v1/agent-dbs', 15000)
  const requestsAPI = useAPI('/api/v1/agent-dbs/requests', 15000)
  const profilesAPI = useAPI('/api/v1/agent-dbs/size-profiles', 30000)
  const providersAPI = useAPI('/api/v1/agent-dbs/providers', 30000)
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
        metadata: {
          purpose: form.purpose,
          workload_types: form.workload_types,
          extensions: form.extensions,
        },
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

  async function lifecycle(id, action) {
    setError(null)
    try {
      if (action === 'delete') {
        const res = await fetch(`/api/v1/agent-dbs/${id}`, {
          method: 'DELETE',
          credentials: 'include',
        })
        if (!res.ok) throw new Error('Delete blocked')
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
      const attempt = await postJSON(
        `/api/v1/agent-dbs/${id}/provision/${action}`,
        {},
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

      <div className="grid gap-4 xl:grid-cols-[360px_minmax(0,1fr)]">
        <ProvisionForm
          form={form}
          busy={busy}
          profiles={profiles}
          onChange={setForm}
          onSubmit={submitProvision}
        />
        <DeploymentList
          deployments={deployments}
          selectedID={selectedID}
          onSelect={setSelectedID}
          onLifecycle={lifecycle}
        />
      </div>

      <div className="grid gap-4 xl:grid-cols-[minmax(0,1fr)_360px]">
        <ProfilePanel
          profiles={profiles}
          form={profileForm}
          busy={busy}
          onChange={setProfileForm}
          onSubmit={submitProfile}
        />
        <ProviderReadinessPanel providers={providers} />
      </div>

      <div className="grid gap-4 xl:grid-cols-[minmax(0,1fr)_360px]">
        <DeploymentDetail
          deployment={selected}
          detail={detail}
          busy={busy}
          onProvisionPreflight={id => runProvisionAction(id, 'preflight')}
          onProvisionExecute={id => runProvisionAction(id, 'execute')}
          onProvisionStatus={id => runProvisionAction(id, 'status')}
          onProvisionDestroyDryRun={
            id => runProvisionAction(id, 'destroy-dry-run')
          }
          onBackupCheck={runBackupCheck}
          onRestoreDrillDryRun={planRestoreDrill}
          onCreateDeployRequest={createDeployRequest}
          onApproveDeployRequest={(id, requestID) =>
            reviewDeployRequest(id, requestID, 'approve')}
          onDenyDeployRequest={(id, requestID) =>
            reviewDeployRequest(id, requestID, 'deny')}
        />
        <RequestQueue requests={requests} />
      </div>
    </div>
  )
}

function useAgentDBDetail(id) {
  const cost = useAPI(id ? `/api/v1/agent-dbs/${id}/cost` : null, 15000)
  const hints = useAPI(id ? `/api/v1/agent-dbs/${id}/tuning-hints` : null, 15000)
  const backups = useAPI(id ? `/api/v1/agent-dbs/${id}/backups` : null, 15000)
  const recs = useAPI(id ? `/api/v1/agent-dbs/${id}/recommendations` : null, 15000)
  const audit = useAPI(id ? `/api/v1/agent-dbs/${id}/audit` : null, 15000)
  const deployRequests = useAPI(
    id ? `/api/v1/agent-dbs/${id}/deploy-requests` : null,
    15000,
  )
  const attempts = useAPI(
    id ? `/api/v1/agent-dbs/${id}/provision/attempts` : null,
    15000,
  )
  return {
    cost: cost.data?.cost,
    hints: hints.data?.tuning_hints || [],
    backups: backups.data?.backups || [],
    recommendations: recs.data?.recommendations || [],
    auditEvents: audit.data?.audit_events || [],
    deployRequests: deployRequests.data?.deploy_requests || [],
    attempts: attempts.data?.attempts || [],
    refetch: () => Promise.all([
      cost.refetch(),
      hints.refetch(),
      backups.refetch(),
      recs.refetch(),
      audit.refetch(),
      deployRequests.refetch(),
      attempts.refetch(),
    ]),
  }
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
