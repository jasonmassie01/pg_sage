import { useAPI } from '../../hooks/useAPI'

// Incident memory in the Cases panel (AI-SRE-SPEC §4 R2): similar past
// incidents of the same database with their operator-verified outcomes.
// They are context for a reader, never evidence of this incident.

const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }

function similarPath(database, id) {
  return `/api/v1/databases/${encodeURIComponent(database)}` +
    `/investigations/${encodeURIComponent(id)}/similar`
}

function outcomeText(outcome) {
  if (!outcome) return 'unverified'
  const actual = outcome.actual_node ? ` (actual: ${outcome.actual_node})` : ''
  return outcome.verdict === 'confirmed'
    ? `confirmed by an operator${actual}`
    : `refuted by an operator${actual}`
}

export function SimilarIncidents({ database, investigationId }) {
  const { data } = useAPI(similarPath(database, investigationId), 0)
  const items = Array.isArray(data?.items) ? data.items : []
  return (
    <div data-testid="similar-incidents">
      <div className="font-medium" style={strong}>Similar past incidents</div>
      <div style={muted}>Context only, not evidence of this incident.</div>
      {items.length === 0 && <div style={muted}>No similar past incidents.</div>}
      <ul className="list-disc pl-4" style={muted}>
        {items.map(s => (
          <li key={s.investigation_id} data-testid={`similar-${s.investigation_id}`}>
            {s.family || s.trigger_kind} ({s.state}
            {s.concluded_at ? `, ${new Date(s.concluded_at).toLocaleString()}` : ''}):
            {' '}root {s.root || 'none'}; open {(s.open || []).join(', ') || 'none'};
            {' '}overlap {Number(s.score).toFixed(2)}; {outcomeText(s.outcome)}
          </li>
        ))}
      </ul>
    </div>
  )
}
