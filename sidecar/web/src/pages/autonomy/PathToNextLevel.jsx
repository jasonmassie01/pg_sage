// Phase 1.1 (2026-10-02) promotion coach: for every family x class that
// can still rise, each unmet check of its next level as one instruction
// with its counts (from the server's "how") and, where the rule implies
// one, an ETA. It explains the bar; it never lowers it.

import { checkText } from './checkText'

const card = { background: 'var(--bg-card)', borderColor: 'var(--border)' }
const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }

function climbable(families) {
  return families.flatMap(f => (f.classes || [])
    .filter(row => row.next && !row.pending)
    .map(row => ({ family: f.family, row })))
}

export function PathToNextLevel({ families }) {
  const pairs = climbable(families)
  return (
    <div className="rounded border p-3 space-y-2" style={card}
      data-testid="path-to-next-level">
      <h3 className="text-sm font-semibold" style={strong}>Path to next level</h3>
      {pairs.length === 0 && (
        <div className="text-xs" style={muted}>
          Nothing left to earn here: every pair is at its cap or already has a
          promotion waiting for an admin.
        </div>
      )}
      {pairs.map(({ family, row }) => (
        <PairPath key={`${family}-${row.class}`} family={family} row={row} />
      ))}
    </div>
  )
}

function PairPath({ family, row }) {
  const unmet = (row.next.checks || []).filter(c => !c.met)
  return (
    <div data-testid={`path-${family}-${row.class}`} className="text-xs">
      <div style={strong}>
        {family} / {row.class}: {row.granted} to {row.next.target}
      </div>
      {row.next.met ? (
        <div style={muted}>
          Evidence met: press Evaluate now to propose {row.next.target}; an admin
          approves it.
        </div>
      ) : (
        <ul className="list-disc pl-5" style={muted}>
          {unmet.map(c => (
            <li key={c.name}>
              {checkText(c)}
              {c.eta && (
                <span data-testid={`eta-${c.name}`} className="ml-1" style={strong}>
                  (ETA {new Date(c.eta).toLocaleString()})
                </span>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
