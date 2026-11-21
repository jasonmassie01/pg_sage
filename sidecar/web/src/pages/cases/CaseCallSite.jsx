import { useState } from 'react'
import { QueryCallSite } from '../QueryCallSite'

// CaseCallSite (pr-01a): the call site of a case's query in the
// application code. A case list renders many cards, so the code host is
// asked only when the operator asks, never once per card.
export function CaseCallSite({ queryid, database }) {
  const [asked, setAsked] = useState(false)
  if (!queryid || !/^-?\d+$/.test(String(queryid))) return null
  if (!database || database === 'all') return null
  if (asked) return <QueryCallSite queryid={String(queryid)} database={database} />
  return (
    <button type="button" onClick={() => setAsked(true)}
      className="block mt-2 text-xs underline" style={{ color: 'var(--accent)' }}>
      Find the call site in the code
    </button>
  )
}
