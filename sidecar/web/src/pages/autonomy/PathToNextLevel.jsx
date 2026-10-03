// Phase 1.1 (2026-10-02) promotion coach: for every family x class that
// can still rise, each unmet check of its next level as one instruction
// with its counts (from the server's "how") and, where the rule implies
// one, an ETA. It explains the bar; it never lowers it. Roadmap 1.1
// (2026-10-03): it shows each family's bench provenance and offers "Run
// bench locally" where bench evidence is missing.

import { checkText } from './checkText'
import { BenchEvidence, RunBenchLocally } from './BenchEvidence'

const card = { background: 'var(--bg-card)', borderColor: 'var(--border)' }
const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }

function climbable(families) {
  return families.flatMap(f => (f.classes || [])
    .filter(row => row.next && !row.pending)
    .map(row => ({ family: f.family, row })))
}

export function PathToNextLevel({ families, benchRuns, canRunBench, onRunBench }) {
  const pairs = climbable(families)
  const climbing = families.filter(f => pairs.some(p => p.family === f.family))
  return (
    <div className="rounded border p-3 space-y-2" style={card}
      data-testid="path-to-next-level">
      <h3 className="text-sm font-semibold" style={strong}>Path to next level</h3>
      <div data-testid="path-reviews-by-a-person" className="text-xs" style={muted}>
        Only reviews by a person count: accept or reject finished investigations in Cases
        (or the REST API). Reviews an agent records through MCP are kept but never count
        toward promotion.
      </div>
      {pairs.length === 0 && (
        <div className="text-xs" style={muted}>
          Nothing left to earn here: every pair is at its cap or already has a
          promotion waiting for an admin.
        </div>
      )}
      {climbing.length > 0 && (
        <BenchEvidence families={climbing} benchRuns={benchRuns} />
      )}
      {pairs.map(({ family, row }) => (
        <PairPath key={`${family}-${row.class}`} family={family} row={row}
          bench={{ benchRuns, canRun: canRunBench, onRun: onRunBench }} />
      ))}
    </div>
  )
}

function PairPath({ family, row, bench }) {
  const unmet = (row.next.checks || []).filter(c => !c.met)
  const needsBench = unmet.some(c => c.name.startsWith('bench_'))
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
      {!row.next.met && needsBench && (
        <RunBenchLocally family={family} benchRuns={bench.benchRuns}
          canRun={bench.canRun} onRun={bench.onRun} />
      )}
    </div>
  )
}
