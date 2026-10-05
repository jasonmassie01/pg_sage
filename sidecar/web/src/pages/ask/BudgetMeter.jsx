// BudgetMeter: today's Ask Sage token use against the per-user and
// per-database limits. Hidden when the budget cannot be loaded.

const muted = { color: 'var(--text-secondary)' }

export function BudgetMeter({ budget }) {
  if (!budget) return null
  return (
    <div data-testid="ask-budget" className="flex flex-wrap gap-4 text-xs" style={muted}>
      <Meter testid="ask-budget-user" label="Your tokens today"
        used={budget.user_used} limit={budget.user_limit} />
      <Meter testid="ask-budget-database" label="Database tokens today"
        used={budget.database_used} limit={budget.database_limit} />
    </div>
  )
}

function Meter({ testid, label, used, limit }) {
  const u = Number(used) || 0
  const l = Number(limit) || 0
  const pct = l > 0 ? Math.min(100, Math.round((u / l) * 100)) : 0
  return (
    <div data-testid={testid} className="min-w-40">
      <div>{label}: {u} / {l}</div>
      <div className="h-1 rounded mt-1" style={{ background: 'var(--border)' }}>
        <div className="h-1 rounded" style={{ width: `${pct}%`,
          background: pct >= 100 ? 'var(--red)' : 'var(--accent)' }} />
      </div>
    </div>
  )
}
