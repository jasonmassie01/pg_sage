// Shadow mode (roadmap 1.4) on an approval card: how pg_sage's shadow
// decisions for this action's class scored, and whether this proposal was
// itself recorded as one (the operator's decision then scores it).

const muted = { color: 'var(--text-secondary)' }

export function ShadowHistory({ history }) {
  const s = history?.summary
  if (!s || !s.total) return null
  return (
    <div data-testid="approval-shadow" className="text-xs" style={muted}>
      {s.total} shadow decisions for {history.class}: {s.correct || 0} correct ·
      {' '}{s.incorrect || 0} incorrect · {s.neutral || 0} neutral ·
      {' '}{s.unscored || 0} unscored · {s.pending || 0} pending
      {history.this_proposal && (
        <span>. pg_sage recorded this proposal as a shadow decision: your decision
          scores it.</span>
      )}
    </div>
  )
}
