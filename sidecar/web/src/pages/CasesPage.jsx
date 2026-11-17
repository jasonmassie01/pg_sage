import { useEffect, useState } from 'react'
import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { QueryState } from '../components/QueryState'
import { queryBlocked } from '../lib/queryState'
import { CaseControls, SuppressedFindings } from './cases/CaseControls'
import { InvestigationPanel } from './cases/InvestigationPanel'
import { WhyNotRun } from './cases/WhyNotRun'
import {
  caseChunks, investigationsByCase, investigationsURL,
} from './cases/caseInvestigations'
import { CasesHealthNotes } from './cases/CasesHealthNotes'
import { CandidateArtifacts } from './cases/CandidateArtifacts'
import { FactBadges } from '../components/FactBadges'
import { SourceFixReport } from './facts/FindingFactSections'
import { caseFactTarget } from '../lib/caseFacts'
import { canDecideFacts } from '../lib/facts'
import {
  FINDING_SOURCES, actionAvailability, caseID, caseLink, casePresentation,
} from './cases/casePresentation'
import {
  focusFromHash, focusedInvestigation, investigationCase, investigationDetailURL,
  resolveFocus,
} from './cases/caseFocus'
import { FocusMissing } from './cases/FocusMissing'
import { CaseTicket } from './cases/CaseTicket'
import { EvidenceList } from './cases/CaseEvidence'
import { FindingSourceSelect } from './cases/FindingSourceSelect'
import { useFindingSource } from './cases/findingSource'
import { CasePlans } from './cases/CasePlans'
import { formatTime } from '../lib/time'

const SOURCE_FILTERS = [
  { value: 'all', label: 'All' },
  { value: 'finding', label: 'Findings' },
  { value: 'schema_health', label: 'Schema' },
  { value: 'query_hint', label: 'Query Hints' },
  { value: 'forecast', label: 'Forecasts' },
  { value: 'incident', label: 'Incidents' },
]

// casesQuery selects the database and, for a focused case, asks the
// server to load that case even when the capped list does not carry it.
// A finding source (rules-12b) filters the list, never a focused case.
function casesQuery(database, focusedID, findingSource) {
  const params = new URLSearchParams()
  if (database && database !== 'all') params.set('database', database)
  if (focusedID) params.set('case', focusedID)
  if (findingSource) params.set('finding_source', findingSource)
  const query = params.toString()
  return query ? `?${query}` : ''
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
  return formatTime(date)
}

export function CasesPage(props) {
  const [focus, setFocus] = useState(() => focusFromHash())
  useEffect(() => {
    const onHashChange = () => setFocus(focusFromHash())
    window.addEventListener('hashchange', onHashChange)
    return () => window.removeEventListener('hashchange', onHashChange)
  }, [])
  const view = { ...props, focus, setFocus }
  return focus?.kind === 'investigation'
    ? <InvestigationFocusCases {...view} /> : <CasesView {...view} />
}

// A cited investigation resolves to its case through its own record, read
// once by id (cases-12), never the database's newest 100. A 404 is the
// not-found notice; any other failure an error with Retry.
function InvestigationFocusCases(props) {
  const url = investigationDetailURL(props.focus, props.database)
  const detail = useAPI(url, 0)
  if (url && detail.errorStatus !== 404 && queryBlocked(detail)) {
    return <QueryState query={detail} skeleton={<LoadingSpinner />} />
  }
  const inv = detail.data?.investigation
  const record = url && inv ? { ...inv, database: detail.data.database } : null
  return <CasesView {...props} investigation={record} />
}

function CasesView({ database, initialSource = 'all', user, focus, setFocus, investigation }) {
  const [sourceFilter, setSourceFilter] = useState(initialSource)
  const [showSuppressed, setShowSuppressed] = useState(false)
  const [findingSource, setFindingSource] = useFindingSource()
  const serverFocus = focus?.kind === 'case' ? focus.id : investigationCase(focus, investigation)
  const casesResult = useAPI(
    `/api/v1/cases${casesQuery(database, serverFocus, focus ? '' : findingSource)}`, 30000)
  const { data, refetch } = casesResult

  if (queryBlocked(casesResult)) {
    return <QueryState query={casesResult} skeleton={<LoadingSpinner />} />
  }

  const cases = data?.cases || []
  const filteredCases = sourceFilter === 'all'
    ? cases
    : cases.filter(c => c.source_type === sourceFilter)
  const focusedID = resolveFocus(focus, cases, investigation)
  const focusedCase = focusedID ? cases.find(c => caseID(c) === focusedID) : null
  const visibleCases = focus ? (focusedCase ? [focusedCase] : []) : filteredCases
  const criticalCount = cases.filter(c => c.severity === 'critical').length
  const investigationCount = cases.filter(c => !c.action_candidates?.length).length
  const listQuery = database && database !== 'all'
    ? `?database=${encodeURIComponent(database)}` : ''

  return (
    <QueryState query={casesResult}><div className="space-y-4" data-testid="cases-page">
      <div>
        <h2 className="text-sm font-semibold"
          style={{ color: 'var(--text-primary)' }}>
          Cases · Issues &amp; investigations
        </h2>
        <p className="text-sm" data-testid="cases-page-description"
          style={{ color: 'var(--text-secondary)' }}>
          Review what needs attention and what we know. Cases hold observations
          and evidence; Actions holds proposed changes, decisions and results.
          Ask Sage can help explain a case.
          {' '}{visibleCases.length} of {cases.length} cases are visible.
        </p>
        <p className="mt-2 text-xs" style={{ color: 'var(--text-secondary)' }}>
          {criticalCount} reported critical · {investigationCount} without a proposed change.
          {' '}Ordered by reported severity, then latest observation. These counts
          describe returned cases; they do not establish collection health or user impact.
        </p>
      </div>

      <CasesHealthNotes unavailable={data?.unavailable}
        truncated={data?.truncated} />

      {focus ? (
        <a href={`#/cases${listQuery}`} className="inline-block text-sm underline"
          style={{ color: 'var(--accent)' }} onClick={() => setFocus(null)}>
          Back to all cases
        </a>
      ) : <div className="flex flex-wrap gap-2" aria-label="Case source filters">
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
        <FindingSourceSelect value={findingSource} sources={data?.finding_sources}
          onChange={setFindingSource} />
      </div>}

      {!focus && <div>
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
      </div>}

      {visibleCases.length === 0 && focus && focus.kind !== 'case' && (
        <FocusMissing focus={focus} database={database}
          investigation={focusedInvestigation(focus, investigation)} />
      )}
      {visibleCases.length === 0 && (!focus || focus.kind === 'case') && (
        <div className="rounded border p-4 text-sm" style={{ borderColor: 'var(--border)', color: 'var(--text-secondary)' }}>
          {focus
            ? 'This case is unavailable in the selected database. Return to the case list or check the database selection.'
            : cases.length > 0 ? 'No cases match this filter.'
              : 'No cases were returned for this database. This alone does not confirm a healthy database; check collection status.'}
          {!focus && cases.length > 0 && <button type="button" className="ml-2 underline"
            onClick={() => setSourceFilter('all')}>Show all cases</button>}
        </div>
      )}

      <div className="space-y-2">
        {caseChunks(visibleCases).map((chunk, i) => (
          <CaseChunk key={i} chunk={chunk} database={database} user={user}
            focused={Boolean(focus)}
            onReview={c => setFocus({ kind: 'case', id: caseID(c), database: '' })}
            onDone={refetch} />
        ))}
      </div>
    </div></QueryState>
  )
}

// CaseChunk renders up to 100 cases with the newest investigation of each,
// asked for by case id (sre-09): a list of the database's newest
// investigations missed a case whose investigation was older. A changed
// case set is a new request; until it answers, the last answer stays
// (keyed by database and case), so an open panel does not unmount.
function CaseChunk({ chunk, database, user, focused, onReview, onDone }) {
  const investigations = useAPI(investigationsURL(database, chunk.ids), 30000)
  const [kept, setKept] = useState({ at: null, items: null })
  const { data, updatedAt } = investigations
  if (data?.items && updatedAt !== kept.at) setKept({ at: updatedAt, items: data.items })
  const byCase = investigationsByCase(data?.items || kept.items)
  const tickets = useAPI(ticketsURL(database), 60000)
  return chunk.cases.map(c => (
    <CaseCard key={`${c.database_name}|${caseID(c)}`} caseRow={c} user={user}
      focused={focused} onReview={() => onReview(c)}
      investigation={byCase.get(`${c.database_name}|${caseID(c)}`)}
      tickets={tickets.data} onTicket={tickets.refetch}
      onDone={onDone} />
  ))
}

// ticketsURL lists the Jira tickets of the database's cases (market-14).
function ticketsURL(database) {
  return database && database !== 'all'
    ? `/api/v1/case-tickets?database=${encodeURIComponent(database)}`
    : '/api/v1/case-tickets'
}

function CaseCard({
  caseRow, user, investigation, onDone, focused, onReview, tickets, onTicket,
}) {
  const candidate = caseRow.action_candidates?.[0]
  const candidateGuardrails = guardrails(candidate)
  const factTarget = caseFactTarget(caseRow)
  const summary = casePresentation(caseRow)
  const observedAt = formatDate(caseRow.observed_at)
  const [detailsOpen, setDetailsOpen] = useState(false)
  const findingID = FINDING_SOURCES.has(caseRow.source_type) ? caseRow.source_ids?.[0] : null

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
            {summary.title}
          </h3>
          <p className="text-sm mt-1"
            style={{ color: 'var(--text-secondary)' }}>
            <strong>Observed: </strong>{summary.observed}
          </p>
        </div>
        <span className="text-xs uppercase"
          style={{ color: 'var(--text-secondary)' }}>
          Reported severity: {caseRow.severity || 'unknown'}
        </span>
      </div>
      <div className="mt-3 flex flex-wrap gap-2 text-xs"
        style={{ color: 'var(--text-secondary)' }}>
        <span>Case state: {caseRow.state || 'unknown'}</span>
        {caseRow.database_name && (
          <span data-testid="case-database">DB: {caseRow.database_name}</span>
        )}
        {factTarget?.object && <span className="font-mono break-all">Object: {factTarget.object}</span>}
        <span>Last observed: {observedAt || 'unknown'}</span>
      </div>
      <p className="mt-2 text-sm" style={{ color: 'var(--text-secondary)' }}>
        <strong>Possible consequence: </strong>{summary.consequence}
      </p>
      <p className="mt-2 text-xs" style={{ color: 'var(--text-secondary)' }}>
        <strong>Reason for attention: </strong>{summary.attention}
      </p>
      {summary.recurrence && <p className="mt-2 text-xs" data-testid="case-recurrence"
        style={{ color: 'var(--text-secondary)' }}>
        <strong>Recurrence: </strong>{summary.recurrence}
      </p>}
      <p className="mt-2 text-sm" style={{ color: 'var(--text-secondary)' }}>
        <strong>Next step: </strong>{actionAvailability(caseRow)}
      </p>
      {summary.glossary && <details className="mt-2 text-xs" data-testid="case-glossary"
        style={{ color: 'var(--text-secondary)' }}>
        <summary className="cursor-pointer">What {summary.glossary.term === 'table statistics' ? 'are' : 'is'} {summary.glossary.term}?</summary>
        <p className="mt-1">{summary.glossary.definition}</p>
      </details>}
      <CasePlans plans={caseRow.plans} database={caseRow.database_name} />
      <CaseFacts target={factTarget} user={user} onDone={onDone} />
      <div className="mt-3 flex flex-wrap items-center gap-3 text-sm">
        {!focused && <a href={caseLink('cases', caseRow)} onClick={onReview}
          className="rounded border px-3 py-1.5 font-medium"
          style={{ borderColor: 'var(--accent)', color: 'var(--accent)' }}>Review case</a>}
        {focused && candidate && <a href={caseLink('actions', caseRow)}
          className="rounded border px-3 py-1.5 font-medium"
          style={{ borderColor: 'var(--accent)', color: 'var(--accent)' }}>Review proposed changes</a>}
        <a href={caseLink('ask', caseRow)} className="underline"
          style={{ color: 'var(--accent)' }}>Explain this</a>
      </div>
      {investigation && <InvestigationPanel database={investigation.database}
        investigation={investigation} user={user} />}
      <details className="mt-3 text-xs" data-testid="case-technical-details"
        onToggle={e => setDetailsOpen(e.currentTarget.open)}>
        <summary className="cursor-pointer py-1" style={{ color: 'var(--text-secondary)' }}>
          Technical evidence and proposed changes
        </summary>
        <div className="mt-2 space-y-2" style={{ color: 'var(--text-secondary)' }}>
          <p className="font-mono break-all">Reported condition: {caseRow.title}</p>
          <p className="font-mono break-all">Case ID: {caseID(caseRow)}</p>
          <p>Source urgency note: {caseRow.why_now || 'not supplied'}</p>
          {factTarget?.object && <p className="font-mono break-all">Affected object: {factTarget.object}</p>}
          <p>Next: <span>{nextStep(caseRow)}</span></p>
          {caseRow.actions_unavailable && <p data-testid="case-actions-unavailable">
            The action history for this case could not be read; it may have
            actions that are not shown.</p>}
          {candidate && <p>Policy: {policyLabel(candidate)}</p>}
          <WhyNotRun caseRow={caseRow} user={user} />
          {caseRow.why && <div><strong>Source recommendation (review applicability before use): </strong>{caseRow.why}</div>}
        </div>
      <EvidenceList evidence={caseRow.evidence || []} category={factTarget?.category}
        database={caseRow.database_name} canExplain={canExplain(user)} />
      {findingID && <div className="mt-3"><SourceFixReport findingID={findingID}
        database={caseRow.database_name} enabled={detailsOpen} /></div>}
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
      </details>
      <CaseTicket caseRow={caseRow} user={user} tickets={tickets?.tickets}
        jiraEnabled={Boolean(tickets?.jira_enabled)} onCreated={onTicket} />
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

// canExplain: POST /api/v1/explain needs the operator or admin role, so a
// viewer is not offered a button that can only fail.
function canExplain(user) {
  return user?.role === 'admin' || user?.role === 'operator'
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
