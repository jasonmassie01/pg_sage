// Game days (AI-SRE-SPEC §4 R3). benches-08: each game day says what its
// report scored and why runs were skipped. A run that scored nothing
// (completed_unscored: every fault program skipped on the clone, for
// example without superuser or with max_prepared_transactions = 0) is a
// warning to fix the clone, distinct from game days being off.

import { scoredText, skipReasons } from './gameDayText'

const card = { background: 'var(--bg-card)', borderColor: 'var(--border)' }
const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }
const warning = { color: 'var(--yellow)' }

// safePassAnchor is the Path to next level pair that shows the family's
// game_day_safe_pass check, or null when no climbable pair has one.
function safePassAnchor(families, family) {
  const f = (families || []).find(x => x.family === family)
  const row = (f?.classes || []).find(c => c.next && !c.pending &&
    (c.next.checks || []).some(check => check.name === 'game_day_safe_pass'))
  return row ? `#path-${family}-${row.class}` : null
}

export function GameDays({ data, families }) {
  const items = data?.items || []
  return (
    <div className="rounded border p-3 text-xs" style={card}>
      <h3 className="text-sm font-semibold" style={strong}>Game days</h3>
      {!data?.enabled && <div style={muted}>Off: needs sre.autonomy.game_days and a
        clone provider.</div>}
      <ul className="space-y-1" style={muted}>
        {items.map(g => <GameDayRow key={g.id} g={g} families={families} />)}
      </ul>
    </div>
  )
}

function GameDayRow({ g, families }) {
  const unscored = g.status === 'completed_unscored'
  const s = g.summary
  const skips = skipReasons(s)
  return (
    <li data-testid={`game-day-${g.id}`}>
      <span>{g.started_at}</span>{' '}
      <span data-testid="game-day-status" data-tone={unscored ? 'warning' : undefined}
        style={unscored ? warning : strong}>
        {unscored ? 'completed_unscored: scored nothing, so it is no safety signal'
          : g.status}
      </span>
      {s && <span>, {scoredText(s)}</span>}
      {skips && <span> (skipped: {skips})</span>}
      {g.eval_run_id && <span>, eval run {g.eval_run_id}</span>}
      <GameDayFamilies names={g.families} families={families} />
      {g.error ? <span>: {g.error}</span> : null}
    </li>
  )
}

// GameDayFamilies links each family to its game-day check on the path.
function GameDayFamilies({ names, families }) {
  if (!names?.length) return null
  return (
    <span>
      {', families '}
      {names.map((name, i) => {
        const href = safePassAnchor(families, name)
        return (
          <span key={name}>
            {i > 0 ? ', ' : ''}
            {href ? <a href={href} style={strong} className="underline">{name}</a>
              : <span>{name}</span>}
          </span>
        )
      })}
    </span>
  )
}
