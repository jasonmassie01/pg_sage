import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'

function dbParam(database) {
  return database && database !== 'all'
    ? `?database=${encodeURIComponent(database)}`
    : ''
}

function caseID(caseRow) {
  return caseRow.case_id || caseRow.id || caseRow.identity_key
}

function scoreValue(caseRow, key) {
  return caseRow[key] ?? 'n/a'
}

function nextStep(caseRow) {
  const candidate = caseRow.action_candidates?.[0]
  if (!candidate) return 'No action proposed'
  if (candidate.blocked_reason) return candidate.blocked_reason
  return candidate.action_type
}

function policyLabel(candidate) {
  return candidate?.policy_decision?.decision || 'policy pending'
}

function guardrails(candidate) {
  return candidate?.guardrails ||
    candidate?.policy_decision?.guardrails ||
    []
}

export function CasesPage({ database }) {
  const { data, loading, error, refetch } = useAPI(
    `/api/v1/cases${dbParam(database)}`,
    30000,
  )

  if (loading) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error} onRetry={refetch} />

  const cases = data?.cases || []

  return (
    <div className="space-y-4" data-testid="cases-page">
      <div>
        <h2 className="text-sm font-semibold"
          style={{ color: 'var(--text-primary)' }}>
          Cases
        </h2>
        <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
          {cases.length} active cases ranked by urgency and actionability.
        </p>
      </div>

      <div className="space-y-2">
        {cases.map(c => (
          <CaseCard key={caseID(c)} caseRow={c} />
        ))}
      </div>
    </div>
  )
}

function CaseCard({ caseRow }) {
  const candidate = caseRow.action_candidates?.[0]
  const candidateGuardrails = guardrails(candidate)

  return (
    <article className="rounded border p-3"
      style={{
        background: 'var(--bg-card)',
        borderColor: 'var(--border)',
      }}>
      <div className="flex items-start justify-between gap-3">
        <div>
          <h3 className="font-medium"
            style={{ color: 'var(--text-primary)' }}>
            {caseRow.title}
          </h3>
          <p className="text-sm mt-1"
            style={{ color: 'var(--text-secondary)' }}>
            {caseRow.why_now || 'not urgent'}
          </p>
        </div>
        <span className="text-xs uppercase"
          style={{ color: 'var(--text-secondary)' }}>
          {caseRow.severity}
        </span>
      </div>
      <div className="mt-3 flex flex-wrap gap-2 text-xs"
        style={{ color: 'var(--text-secondary)' }}>
        <span>State: {caseRow.state}</span>
        <span>Impact: {scoreValue(caseRow, 'impact_score')}</span>
        <span>Urgency: {scoreValue(caseRow, 'urgency_score')}</span>
        <span>Next: <span>{nextStep(caseRow)}</span></span>
        {candidate && <span>Policy: {policyLabel(candidate)}</span>}
      </div>
      {candidateGuardrails.length > 0 && (
        <div className="mt-3 flex flex-wrap gap-1.5"
          aria-label="Action guardrails">
          {candidateGuardrails.map(g => (
            <span key={g}
              className="rounded px-1.5 py-0.5 text-xs"
              style={{
                color: 'var(--text-secondary)',
                border: '1px solid var(--border)',
              }}>
              {g}
            </span>
          ))}
        </div>
      )}
    </article>
  )
}
