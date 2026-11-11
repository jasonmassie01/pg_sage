// FocusMissing: the Cases notice for an Ask Sage citation of a finding
// or investigation that no loaded case holds (see caseFocus.js).

// FocusMissing says which cited finding or investigation has no case here.
export function FocusMissing({ focus, investigation }) {
  if (focus.kind === 'investigation' && investigation) {
    return <CaselessInvestigation inv={investigation} />
  }
  const db = focus.database ? `?database=${encodeURIComponent(focus.database)}` : ''
  return (
    <div data-testid="cases-focus-missing" className="rounded border p-4 text-sm"
      style={{ borderColor: 'var(--border)', color: 'var(--text-secondary)' }}>
      {focus.kind === 'finding'
        ? <>No open case holds finding {focus.id}; it may have been resolved,
          suppressed, or not loaded in this view.{' '}
          <a href={`/api/v1/findings/${encodeURIComponent(focus.id)}${db}`}
            className="underline" style={{ color: 'var(--accent)' }}>
            View the finding record</a></>
        : <>No case is loaded for investigation {focus.id}; it may have expired
          or belong to another database.</>}
    </div>
  )
}

// CaselessInvestigation is an investigation that belongs to no case (Ask
// Sage opens its own from a question); it links to the investigation
// record instead.
function CaselessInvestigation({ inv }) {
  const href = `/api/v1/databases/${encodeURIComponent(inv.database || '')}` +
    `/investigations/${encodeURIComponent(inv.id)}`
  return (
    <div data-testid="cases-focus-missing" className="rounded border p-4 text-sm"
      style={{ borderColor: 'var(--border)', color: 'var(--text-secondary)' }}>
      Investigation {inv.id} has no case here; it was not opened from a case (Ask Sage
      opens its own from a question).{' '}
      <a href={href} className="underline" style={{ color: 'var(--accent)' }}>
        View the investigation record</a>
    </div>
  )
}
