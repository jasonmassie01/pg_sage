import { caseHref, relatedCase, rowDatabase } from './workflow'

// CaseRelation links an action row to the case that explains it, or to the
// database's cases when no case is linked.
export function CaseRelation({ row, cases, database }) {
  const linked = relatedCase(row, cases, database)
  return <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
    {linked ? <a href={caseHref(linked, database)} className="underline"
      style={{ color: 'var(--accent)' }}>Review case: {linked.title}</a>
      : <>Case relation unavailable. <a href={caseHref(null, rowDatabase(row, database))}
        className="underline" style={{ color: 'var(--accent)' }}>
        View Cases for this database</a></>}
  </p>
}
