import { useAPI } from '../../hooks/useAPI'

// The investigation's event timeline (its append-only hash chain): every
// step and transition, with the model turn's events called out:
// model_reviewed (an accepted review), model_rejected (a reply that fell
// back to the deterministic result, with the reason) and model_disagreed
// (the model ranked another root first; the graph's root stands).

const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }

const MODEL_EVENTS = new Set(['model_reviewed', 'model_rejected', 'model_disagreed'])

function describe(e) {
  const p = e.payload || {}
  switch (e.type) {
    case 'model_rejected':
      return `model reply not used (${p.reason || 'unknown reason'}, stage ` +
        `${p.stage || 'unknown'}): ${p.detail || ''}. The deterministic result stands.`
    case 'model_disagreed':
      return `the model ranked ${p.model_root} first; the graph's root cause ` +
        `${p.graph_root} stands and nothing the model said is kept.`
    case 'model_reviewed':
      return `model review accepted (turn ${p.turns ?? '?'}` +
        `${p.repaired ? ', after one repair' : ''}).`
    default:
      return e.type.replaceAll('_', ' ')
  }
}

function Event({ event }) {
  const model = MODEL_EVENTS.has(event.type)
  const color = event.type === 'model_reviewed' || !model
    ? muted : { color: 'var(--yellow, #ca8a04)' }
  return (
    <li data-testid={`timeline-${event.type}`} data-kind={model ? 'model' : 'graph'}
      style={color}>
      {event.observed_at ? new Date(event.observed_at).toLocaleTimeString() : ''}{' '}
      <span style={model ? color : strong}>{event.type}</span>: {describe(event)}
      {event.actor ? ` (${event.actor})` : ''}
    </li>
  )
}

export function InvestigationTimeline({ path }) {
  const { data, error } = useAPI(`${path}/events`, 0)
  const events = [...(data?.events || [])].sort((a, b) => a.sequence - b.sequence)
  return (
    <div data-testid="investigation-timeline">
      <div className="font-medium" style={strong}>Timeline</div>
      {error && <div style={muted}>Timeline unavailable: {error}</div>}
      {!error && events.length === 0 && <div style={muted}>No events yet.</div>}
      {data && data.chain_verified === false && (
        <div style={strong}>The event chain does not verify.</div>
      )}
      <ol className="pl-4" style={muted}>
        {events.map(e => <Event key={e.sequence} event={e} />)}
      </ol>
    </div>
  )
}
