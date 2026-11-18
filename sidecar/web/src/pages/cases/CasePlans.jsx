import { planHref } from '../actions/plans'

// CasePlans links a case to the plans its findings are steps of (ux-03):
// "Part of plan: <goal>" opens the plan's review in Actions. The case
// payload's plans are [{id, goal}]; an entry without a positive integer id
// is skipped.

export function CasePlans({ plans, database }) {
  const list = (Array.isArray(plans) ? plans : [])
    .filter(p => Number.isSafeInteger(p?.id) && p.id > 0)
  if (list.length === 0) return null
  return <ul className="mt-2 text-sm space-y-1" data-testid="case-plans">
    {list.map(p => <li key={p.id}>
      <a href={planHref(p.id, database)} className="underline"
        style={{ color: 'var(--accent)' }}>Part of plan: {p.goal || `plan ${p.id}`}</a>
    </li>)}
  </ul>
}
