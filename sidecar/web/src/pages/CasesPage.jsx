import { useState } from 'react'
import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'

const SOURCE_FILTERS = [
  { value: 'all', label: 'All' },
  { value: 'finding', label: 'Findings' },
  { value: 'schema_health', label: 'Schema' },
  { value: 'query_hint', label: 'Query Hints' },
  { value: 'forecast', label: 'Forecasts' },
  { value: 'incident', label: 'Incidents' },
]

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

function formatDate(value) {
  if (!value) return null
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return null
  return date.toLocaleString()
}

export function CasesPage({ database, initialSource = 'all' }) {
  const [sourceFilter, setSourceFilter] = useState(initialSource)
  const { data, loading, error, refetch } = useAPI(
    `/api/v1/cases${dbParam(database)}`,
    30000,
  )

  if (loading) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error} onRetry={refetch} />

  const cases = data?.cases || []
  const filteredCases = sourceFilter === 'all'
    ? cases
    : cases.filter(c => c.source_type === sourceFilter)

  return (
    <div className="space-y-4" data-testid="cases-page">
      <div>
        <h2 className="text-sm font-semibold"
          style={{ color: 'var(--text-primary)' }}>
          Cases
        </h2>
        <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
          {filteredCases.length} of {cases.length} cases ranked by urgency
          and actionability.
        </p>
      </div>

      <div className="flex flex-wrap gap-2" aria-label="Case source filters">
        {SOURCE_FILTERS.map(source => (
          <button
            key={source.value}
            type="button"
            aria-pressed={sourceFilter === source.value}
            onClick={() => setSourceFilter(source.value)}
            className="rounded px-2.5 py-1 text-xs"
            style={{
              color: sourceFilter === source.value
                ? 'var(--accent)' : 'var(--text-secondary)',
              border: '1px solid var(--border)',
              background: sourceFilter === source.value
                ? 'var(--bg-hover)' : 'var(--bg-card)',
            }}>
            {source.label}
          </button>
        ))}
      </div>

      <div className="space-y-2">
        {filteredCases.map(c => (
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
      {(caseRow.actions || []).length > 0 && (
        <div className="mt-3 space-y-2" aria-label="Action timeline">
          {caseRow.actions.map(action => (
            <ActionTimelineItem key={action.id || action.type}
              action={action} />
          ))}
        </div>
      )}
    </article>
  )
}

function ActionTimelineItem({ action }) {
  const expiresAt = formatDate(action.expires_at)
  const cooldownUntil = formatDate(action.cooldown_until)
  return (
    <div className="rounded border p-2 text-xs"
      style={{ borderColor: 'var(--border)' }}>
      <div className="flex flex-wrap gap-2"
        style={{ color: 'var(--text-secondary)' }}>
        <span>{action.type}</span>
        <span>Status: {action.status}</span>
        {action.lifecycle_state && (
          <span>Lifecycle: {action.lifecycle_state}</span>
        )}
        {action.verification_status && (
          <span>Verification: {action.verification_status}</span>
        )}
        {action.attempt_count > 0 && (
          <span>Attempts: {action.attempt_count}</span>
        )}
        {expiresAt && <span>Expires: {expiresAt}</span>}
        {cooldownUntil && <span>Cooldown until: {cooldownUntil}</span>}
      </div>
      {action.blocked_reason && (
        <div className="mt-1" style={{ color: 'var(--text-primary)' }}>
          {action.blocked_reason}
        </div>
      )}
    </div>
  )
}
