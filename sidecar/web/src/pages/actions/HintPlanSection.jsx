/* eslint-disable react-refresh/only-export-components */

// HintPlanSection shows a query-hint card's plan comparison (tuning-05b):
// the verdict, both costs and the signed delta, the reason when the
// verdict is not "changed", and both EXPLAIN plans in collapsed blocks.
// Plans render as React text in a <pre>, never as HTML. A plan the server
// left out (over 64 KB) shows its *_omitted note instead.

function isNumber(v) {
  return typeof v === 'number' && Number.isFinite(v)
}

function signedPct(v) {
  const sign = v > 0 ? '+' : ''
  return `${sign}${v.toFixed(1)}%`
}

export function hintPlanSummary(ev) {
  let text = `Plan comparison: ${ev.verdict}`
  if (isNumber(ev.plain_cost) && isNumber(ev.hinted_cost)) {
    text += `, cost ${ev.plain_cost.toFixed(1)} to ${ev.hinted_cost.toFixed(1)}`
  }
  if (isNumber(ev.cost_delta_pct)) text += ` (${signedPct(ev.cost_delta_pct)})`
  return text
}

function PlanBlock({ testId, label, plan, omitted }) {
  if (plan == null && !omitted) return null
  return (
    <details data-testid={testId} className="text-sm">
      <summary className="cursor-pointer" style={{ color: 'var(--text-primary)' }}>
        {label}
      </summary>
      {plan != null ? (
        <pre className="text-xs overflow-x-auto p-2 rounded mt-1"
          style={{ background: 'var(--bg-primary)', color: 'var(--text-primary)' }}>
          {JSON.stringify(plan, null, 2)}
        </pre>
      ) : (
        <p className="text-xs mt-1" style={{ color: 'var(--text-secondary)' }}>{omitted}</p>
      )}
    </details>
  )
}

export function HintPlanSection({ evidence: ev }) {
  if (!ev || typeof ev !== 'object' || Array.isArray(ev)) return null
  if (typeof ev.verdict !== 'string' || ev.verdict === '') return null
  return (
    <div data-testid="approval-hint-plans" className="space-y-1">
      <div className="text-xs font-medium mb-1" style={{ color: 'var(--text-secondary)' }}>
        Plan comparison
      </div>
      <p data-testid="hint-plan-summary" className="text-sm"
        style={{ color: 'var(--text-primary)' }}>
        {hintPlanSummary(ev)}
      </p>
      {ev.verdict !== 'changed' && ev.reason && (
        <p data-testid="hint-plan-reason" className="text-sm"
          style={{ color: 'var(--text-secondary)' }}>
          Reason: {String(ev.reason)}
        </p>
      )}
      <PlanBlock testId="hint-plan-plain" label="Plan without hint" plan={ev.plain_plan}
        omitted={ev.plain_plan_omitted} />
      <PlanBlock testId="hint-plan-hinted" label="Plan with hint" plan={ev.hinted_plan}
        omitted={ev.hinted_plan_omitted} />
    </div>
  )
}
