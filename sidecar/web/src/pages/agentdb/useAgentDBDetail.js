import { useAPI } from '../../hooks/useAPI'

export function useAgentDBDetail(id) {
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
