import { LetterBadge } from './LetterBadge'
import { dimensionUnknownReason, shortReason, statusLabel } from '../../lib/review'

const card = { background: 'var(--bg-card)', border: '1px solid var(--border)' }
const muted = { color: 'var(--text-secondary)' }

// DimensionTable lists the grade's dimensions in rubric order, each with
// its letter or "Unknown: <reason>", its coverage and the checks behind
// it. Rows stack, so eight dimensions fit a 375 px screen.
export function DimensionTable({ dimensions }) {
  const dims = dimensions || []
  return (
    <section className="rounded p-4 space-y-2" style={card}
      aria-labelledby="dimensions-heading">
      <h2 id="dimensions-heading" className="text-sm font-medium" style={muted}>Dimensions</h2>
      {dims.length === 0 ? (
        <p className="text-sm">This grade has no dimensions.</p>
      ) : (
        <ul className="divide-y" style={{ borderColor: 'var(--border)' }}>
          {dims.map(d => <DimensionRow key={d.dimension} dim={d} />)}
        </ul>
      )}
    </section>
  )
}

function DimensionRow({ dim }) {
  const checks = dim.checks || []
  return (
    <li data-testid="dimension-row" className="py-2 space-y-1">
      <div className="flex flex-wrap items-center gap-2">
        <span data-testid="dimension-title" className="text-sm font-medium">{dim.title}</span>
        <LetterBadge letter={dim.letter} score={dim.score}
          reason={dimensionUnknownReason(dim)} />
        <span className="text-xs" style={muted}>coverage {Number(dim.coverage || 0)}%</span>
      </div>
      <details className="text-xs">
        <summary className="cursor-pointer" style={muted}>
          {checks.length} check{checks.length === 1 ? '' : 's'}
        </summary>
        <ul className="mt-1 space-y-1">
          {checks.map(c => <CheckLine key={c.check} check={c} />)}
        </ul>
      </details>
    </li>
  )
}

function CheckLine({ check }) {
  const reason = shortReason(check.reason)
  return (
    <li data-testid="dimension-check" style={{ overflowWrap: 'anywhere' }}>
      <span className="font-semibold">{statusLabel(check.status)}</span>{' '}
      <span className="font-mono">{check.check}</span>
      {check.critical ? ' (critical)' : ''}
      {reason.text && (
        <>
          {' — '}
          <span data-testid="check-reason" title={reason.title}>{reason.text}</span>
        </>
      )}
    </li>
  )
}
