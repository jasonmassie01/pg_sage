import { SQLBlock } from '../../components/SQLBlock'
import { ExplainQueryButton } from '../../components/ExplainPanel'
import { FindingFactSections } from '../facts/FindingFactSections'
import { QueryPageLink } from './QueryPageLink'
import { CaseCallSite } from './CaseCallSite'

// A case's evidence list (split out of CasesPage.jsx): each item's
// summary, scalar details, query with Explain and its full payload.
export function EvidenceList({ evidence, category, database, canExplain }) {
  const visibleEvidence = evidence.filter(item =>
    item?.summary || Object.keys(item?.detail || {}).length > 0,
  )
  if (visibleEvidence.length === 0) return null
  return (
    <div className="mt-3 space-y-2" aria-label="Issue evidence">
      {visibleEvidence.map((item, index) => (
        <EvidenceItem
          key={`${item.type || 'evidence'}-${index}`}
          evidence={item}
          category={category}
          database={database}
          canExplain={canExplain}
        />
      ))}
    </div>
  )
}

// Keys rendered by their own section, not as key/value chips.
const SECTION_KEYS = new Set(['query', 'normalized_query', 'sample_query', 'cleanup_sql'])

function EvidenceItem({ evidence, category, database, canExplain }) {
  const detail = evidence.detail || {}
  const query = detail.query || detail.normalized_query || detail.sample_query
  const scalarEntries = Object.entries(detail)
    .filter(([key, value]) =>
      !SECTION_KEYS.has(key) &&
      value !== null &&
      value !== undefined &&
      typeof value !== 'object',
    )
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
      <QueryPageLink queryid={evidence.queryid} database={database} />
      <CaseCallSite queryid={evidence.queryid} database={database} />
      {canExplain && <ExplainQueryButton database={database} queryid={evidence.queryid} />}
      <FindingFactSections row={{ category, detail }} />
      <details className="mt-2">
        <summary className="cursor-pointer">Complete evidence payload</summary>
        <pre className="mt-2 whitespace-pre-wrap break-all">{JSON.stringify(detail, null, 2)}</pre>
      </details>
    </section>
  )
}
