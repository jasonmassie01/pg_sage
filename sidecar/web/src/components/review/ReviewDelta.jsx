import { statusLabel } from '../../lib/review'
import { formatTime } from '../../lib/time'

const card = { background: 'var(--bg-card)', border: '1px solid var(--border)' }
const muted = { color: 'var(--text-secondary)' }

// ReviewDelta is "What changed since <date>": the checks that moved since
// the previous grade, or that nothing did.
export function ReviewDelta({ delta, previous }) {
  if (!delta) return null
  const moves = (delta.dimensions || []).flatMap(d => (d.checks || []).map(c => ({
    ...c, dimension: d.dimension })))
  const since = formatTime(previous?.finished_at)
  let heading = `What changed since ${since}`
  if (delta.first_run || !previous) heading = 'What changed'
  else if (moves.length === 0) heading = `No change since ${since}`
  return (
    <section data-testid="review-delta" className="rounded p-4 space-y-2" style={card}
      aria-labelledby="delta-heading">
      <h2 id="delta-heading" className="text-sm font-medium" style={muted}>{heading}</h2>
      {delta.first_run || !previous ? (
        <p className="text-sm">First grade: there is no earlier grade to compare with.</p>
      ) : (
        <DeltaBody delta={delta} moves={moves} />
      )}
    </section>
  )
}

function DeltaBody({ delta, moves }) {
  const overall = `Overall ${delta.from_overall || 'unknown'} → ${delta.to_overall || 'unknown'}`
  return (
    <>
      {delta.from_overall !== delta.to_overall && <p className="text-sm">{overall}</p>}
      {delta.rubric_changed && (
        <p className="text-xs" style={muted}>
          The rubric changed between these grades, so some moves come from the rubric,
          not the database.
        </p>
      )}
      {moves.length > 0 && (
        <ul className="space-y-1 text-sm">
          {moves.map(m => (
            <li key={`${m.dimension}-${m.check}`} data-testid="delta-check"
              style={{ overflowWrap: 'anywhere' }}>
              {`${m.check}: ${m.from ? statusLabel(m.from) : 'new'} → ${statusLabel(m.to)}`}
            </li>
          ))}
        </ul>
      )}
    </>
  )
}
