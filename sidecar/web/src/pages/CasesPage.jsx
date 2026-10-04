import { useState } from 'react'
import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { SQLBlock } from '../components/SQLBlock'
import { CaseControls, SuppressedFindings } from './cases/CaseControls'
import { InvestigationPanel } from './cases/InvestigationPanel'
import { FactBadges } from '../components/FactBadges'
import { FindingFactSections } from './facts/FindingFactSections'
import { caseFactTarget } from '../lib/caseFacts'
import { canDecideFacts } from '../lib/facts'

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

// investigationsByCase keeps each case's newest investigation (the API
// lists newest first), keyed by database and case id ("|" never appears
// in a database name), so a same-named case of another database never
// borrows it.
function investigationsByCase(items) {
  const out = new Map()
  for (const inv of items || []) {
    const key = `${inv.database}|${inv.case_id}`
    if (!out.has(key)) out.set(key, inv)
  }
  return out
}

function nextStep(caseRow) {
  const candidate = caseRow.action_candidates?.[0]
  if (!candidate) return 'Needs investigation'
  if (candidate.blocked_reason) {
    return `${candidate.action_type}: ${candidate.blocked_reason}`
  }
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
  // Go zero times (0001-01-01) mean "unset" (G9-B06).
  if (Number.isNaN(date.getTime()) || date.getUTCFullYear() < 1971) {
    return null
  }
  return date.toLocaleString()
}

export function CasesPage({ database, initialSource = 'all', user }) {
  const [sourceFilter, setSourceFilter] = useState(initialSource)
  const [showSuppressed, setShowSuppressed] = useState(false)
  const { data, loading, error, refetch } = useAPI(
    `/api/v1/cases${dbParam(database)}`,
    30000,
  )
  const investigations = useAPI(
    `/api/v1/investigations?database=${encodeURIComponent(database || 'all')}`,
    30000,
  )
  const byCase = investigationsByCase(investigations.data?.items)

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
        <p className="text-sm" data-testid="cases-page-description"
          style={{ color: 'var(--text-secondary)' }}>
          Cases group findings into ranked work items (by severity, then most
          recent observation) across findings, incidents and query hints.
          pg_sage attaches
          evidence and action candidates here, then approved or executable
          work flows to Actions for review, execution, and audit history.
          {' '}{filteredCases.length} of {cases.length} cases are visible.
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

      <div>
        <button type="button" data-testid="cases-show-suppressed"
          aria-pressed={showSuppressed}
          onClick={() => setShowSuppressed(v => !v)}
          className="rounded px-2.5 py-1 text-xs"
          style={{ color: 'var(--text-secondary)',
            border: '1px solid var(--border)' }}>
          {showSuppressed ? 'Hide suppressed findings'
            : 'Show suppressed findings'}
        </button>
        {showSuppressed && (
          <div className="mt-2">
            <SuppressedFindings database={database} user={user}
              onDone={refetch} />
          </div>
        )}
      </div>

      <div className="space-y-2">
        {filteredCases.map(c => (
          <CaseCard key={caseID(c)} caseRow={c} user={user}
            investigation={byCase.get(`${c.database_name}|${caseID(c)}`)}
            onDone={refetch} />
        ))}
      </div>
    </div>
  )
}

function CaseCard({ caseRow, user, investigation, onDone }) {
  const candidate = caseRow.action_candidates?.[0]
  const candidateGuardrails = guardrails(candidate)
  const factTarget = caseFactTarget(caseRow)

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
        {caseRow.database_name && (
          <span data-testid="case-database">DB: {caseRow.database_name}</span>
        )}
        <span>Next: <span>{nextStep(caseRow)}</span></span>
        {candidate && <span>Policy: {policyLabel(candidate)}</span>}
      </div>
      {caseRow.why && (
        <div className="mt-3 text-sm"
          style={{ color: 'var(--text-secondary)' }}>
          {caseRow.why}
        </div>
      )}
      <CaseFacts target={factTarget} user={user} onDone={onDone} />
      <EvidenceList evidence={caseRow.evidence || []} category={factTarget?.category} />
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
      {(caseRow.action_candidates || []).map(actionCandidate => (
        <CandidateArtifacts
          key={actionCandidate.action_type}
          candidate={actionCandidate}
        />
      ))}
      {(caseRow.actions || []).length > 0 && (
        <div className="mt-3 space-y-2" aria-label="Action timeline">
          {caseRow.actions.map(action => (
            <ActionTimelineItem key={action.id || action.type}
              action={action} />
          ))}
        </div>
      )}
      {investigation && (
        <InvestigationPanel database={investigation.database}
          investigation={investigation} user={user} />
      )}
      <CaseControls caseRow={caseRow} user={user} onDone={onDone} />
    </article>
  )
}

// CaseFacts shows the binding facts about a finding case's object.
function CaseFacts({ target, user, onDone }) {
  if (!target) return null
  return (
    <div className="mt-3">
      <FactBadges database={target.database} objects={[target.object]}
        canDecide={canDecideFacts(user)} onChanged={onDone} />
    </div>
  )
}

function EvidenceList({ evidence, category }) {
  const visibleEvidence = evidence.filter(item =>
    item?.summary || Object.keys(item?.detail || {}).length > 0,
  )
  if (visibleEvidence.length === 0) return null
  return (
    <div className="mt-3 space-y-2" aria-label="Case evidence">
      {visibleEvidence.map((item, index) => (
        <EvidenceItem
          key={`${item.type || 'evidence'}-${index}`}
          evidence={item}
          category={category}
        />
      ))}
    </div>
  )
}

// Keys rendered by their own section, not as key/value chips.
const SECTION_KEYS = new Set(['query', 'normalized_query', 'sample_query', 'cleanup_sql'])

function EvidenceItem({ evidence, category }) {
  const detail = evidence.detail || {}
  const query = detail.query || detail.normalized_query || detail.sample_query
  const scalarEntries = Object.entries(detail)
    .filter(([key, value]) =>
      !SECTION_KEYS.has(key) &&
      value !== null &&
      value !== undefined &&
      typeof value !== 'object',
    )
    .slice(0, 8)
  return (
    <section className="rounded border p-2 text-xs"
      style={{ borderColor: 'var(--border)' }}>
      <div className="flex flex-wrap gap-2"
        style={{ color: 'var(--text-secondary)' }}>
        {evidence.type && <span>{evidence.type}</span>}
        {evidence.summary && <span>{evidence.summary}</span>}
      </div>
      {scalarEntries.length > 0 && (
        <div className="mt-2 flex flex-wrap gap-2"
          style={{ color: 'var(--text-secondary)' }}>
          {scalarEntries.map(([key, value]) => (
            <span key={key}>{key}: {String(value)}</span>
          ))}
        </div>
      )}
      {query && (
        <div className="mt-2">
          <div className="font-medium mb-1"
            style={{ color: 'var(--text-primary)' }}>
            Query
          </div>
          <SQLBlock sql={String(query)} />
        </div>
      )}
      <FindingFactSections row={{ category, detail }} />
    </section>
  )
}

function CandidateArtifacts({ candidate }) {
  const preflight = candidate.ddl_preflight
  const script = candidate.script_output
  if (!preflight && !script) return null
  return (
    <div className="mt-3 space-y-2">
      {preflight && <DDLPreflight preflight={preflight} />}
      {script && <ScriptOutput script={script} />}
    </div>
  )
}

function DDLPreflight({ preflight }) {
  return (
    <section className="rounded border p-2 text-xs"
      style={{ borderColor: 'var(--border)' }}>
      <div className="font-medium mb-1"
        style={{ color: 'var(--text-primary)' }}>
        DDL preflight
      </div>
      <div className="mb-2" style={{ color: 'var(--text-secondary)' }}>
        {preflight.summary}
      </div>
      <div className="flex flex-wrap gap-2 mb-2"
        style={{ color: 'var(--text-secondary)' }}>
        {preflight.lock_level && (
          <span>Lock: {preflight.lock_level}</span>
        )}
        <span>Rewrite: {preflight.requires_rewrite ? 'yes' : 'no'}</span>
        {preflight.risk_score > 0 && (
          <span>Risk: {preflight.risk_score}</span>
        )}
      </div>
      {(preflight.checks || []).length > 0 && (
        <div className="flex flex-wrap gap-1.5">
          {preflight.checks.map(check => (
            <span key={check.name}
              className="rounded px-1.5 py-0.5"
              style={{
                color: 'var(--text-secondary)',
                border: '1px solid var(--border)',
              }}>
              {check.name}: {check.status}
              {check.detail ? ` (${check.detail})` : ''}
            </span>
          ))}
        </div>
      )}
    </section>
  )
}

function ScriptOutput({ script }) {
  return (
    <section className="rounded border p-2 text-xs"
      style={{ borderColor: 'var(--border)' }}>
      <div className="flex flex-wrap items-center gap-2 mb-2">
        <span className="font-medium"
          style={{ color: 'var(--text-primary)' }}>
          Migration script
        </span>
        <span style={{ color: 'var(--text-secondary)' }}>
          {script.filename}
        </span>
      </div>
      {script.migration_sql && <SQLBlock sql={script.migration_sql} />}
      {script.rollback_sql && (
        <div className="mt-2">
          <div className="font-medium mb-1"
            style={{ color: 'var(--text-primary)' }}>
            Rollback script
          </div>
          <SQLBlock sql={script.rollback_sql} />
        </div>
      )}
      {(script.verification_sql || []).length > 0 && (
        <div className="mt-2">
          <div className="font-medium mb-1"
            style={{ color: 'var(--text-primary)' }}>
            Verification SQL
          </div>
          {script.verification_sql.map(sql => (
            <SQLBlock key={sql} sql={sql} />
          ))}
        </div>
      )}
      {(script.pr_title || script.pr_body) && (
        <div className="mt-2" style={{ color: 'var(--text-secondary)' }}>
          <div className="font-medium"
            style={{ color: 'var(--text-primary)' }}>
            PR / CI output
          </div>
          {script.pr_title && <div>{script.pr_title}</div>}
          {script.pr_body && <div>{script.pr_body}</div>}
        </div>
      )}
    </section>
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
