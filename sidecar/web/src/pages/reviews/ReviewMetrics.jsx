import {
  DASH, cardStyle, concurrenceTotal, formatDuration, formatNumber, formatPct, humanize,
} from '../../lib/reviews'

// ReviewMetrics shows the measured Change Review figures of
// GET /api/v1/change-reviews/summary: concurrence, time to a DBA verdict,
// receipts and false-safe count, override rate per rule, hours credited
// and pending, and the shadow auto-clear readiness bar. Auto-clear is
// shadow only in this build: the bar is the evidence a promotion would
// need, never a switch.

const VERDICT_ORDER = ['risky', 'review', 'safe']

function Metric({ testId, label, children }) {
  return (
    <div data-testid={testId} className="rounded p-3" style={cardStyle}>
      <div className="text-xs" style={{ color: 'var(--text-secondary)' }}>{label}</div>
      <div className="text-sm mt-1" style={{ color: 'var(--text-primary)' }}>
        {children}
      </div>
    </div>
  )
}

function Concurrence({ concurrence }) {
  const total = concurrenceTotal(concurrence)
  const classes = VERDICT_ORDER.filter(v => concurrence?.[v]?.of > 0)
  return (
    <Metric testId="metric-concurrence" label="DBA concurrence">
      {formatPct(total.rate)}
      {total.of > 0 ? ` (${total.n} of ${total.of} decided)` : ''}
      <div data-testid="metric-concurrence-by-verdict" className="text-xs mt-1"
        style={{ color: 'var(--text-secondary)' }}>
        {classes.map(v => `${v} ${concurrence[v].n} of ${concurrence[v].of}`).join(' · ')}
      </div>
    </Metric>
  )
}

function Overrides({ byRule }) {
  const rules = Object.entries(byRule || {}).sort(([a], [b]) => a.localeCompare(b))
  return (
    <Metric testId="metric-overrides" label="Override rate per rule">
      {rules.length === 0 ? DASH : (
        <ul className="text-xs space-y-1">
          {rules.map(([rule, o]) => (
            <li key={rule}>
              <span className="font-mono">{rule}</span>: {o.overrides} of {o.reviews}
              {' '}({formatPct(o.rate)})
              {o.reasons?.length > 0 && (
                <span style={{ color: 'var(--text-secondary)' }}>
                  {' '}· {o.reasons.join('; ')}
                </span>
              )}
            </li>
          ))}
        </ul>
      )}
    </Metric>
  )
}

function Receipts({ receipts }) {
  const r = receipts || {}
  return (
    <Metric testId="metric-receipts" label="Receipts">
      {r.clean || 0} clean · {r.clean_low_traffic || 0} clean, low traffic ·
      {' '}{r.regressed || 0} regressed · {r.unverifiable || 0} unverifiable
    </Metric>
  )
}

export function AutoClearBar({ autoClear }) {
  const a = autoClear || {}
  const max = a.min_scored || 0
  const value = Math.min(a.scored || 0, max)
  return (
    <section data-testid="autoclear-bar" className="rounded p-3" style={cardStyle}
      aria-label="Auto-clear readiness">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h3 className="text-sm font-semibold" style={{ color: 'var(--text-primary)' }}>
          Auto-clear readiness
        </h3>
        <span className="text-xs px-2 py-0.5 rounded"
          style={{ color: 'var(--yellow)', border: '1px solid var(--yellow)' }}>
          {a.label || 'shadow only — not enabled'}
        </span>
      </div>
      <div role="progressbar" aria-valuemin={0} aria-valuemax={max} aria-valuenow={value}
        aria-label="Scored reviews toward the bar" className="mt-2 h-2 rounded"
        style={{ background: 'var(--bg-hover)' }}>
        <div className="h-2 rounded" style={{ background: 'var(--accent)',
          width: max > 0 ? `${(value / max) * 100}%` : '0%' }} />
      </div>
      <div className="text-xs mt-2" style={{ color: 'var(--text-secondary)' }}>
        {a.scored || 0} of {max} scored · {formatPct(a.correct_rate)} correct (needs
        {' '}{formatPct(a.min_correct_rate)}) · {a.regressions_90d || 0} regressions in 90
        days · {a.ready ? 'bar met; enabling needs a human promotion' : 'not ready'}
      </div>
    </section>
  )
}

export function ReviewMetrics({ summary }) {
  if (!summary) return null
  return (
    <div className="space-y-3" aria-label="Change Review metrics">
      {summary.partial && (
        <p data-testid="metrics-partial" className="text-xs"
          style={{ color: 'var(--yellow)' }}>
          Partial: {(summary.unavailable || []).join(', ')} could not be read.
        </p>
      )}
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Concurrence concurrence={summary.concurrence} />
        <Metric testId="metric-dba-wait" label="Time to DBA verdict (median)">
          {formatDuration(summary.dba_wait_p50_seconds)}
          {summary.dba_wait_p90_seconds != null &&
            ` · p90 ${formatDuration(summary.dba_wait_p90_seconds)}`}
        </Metric>
        <Metric testId="metric-false-safe" label="False safe (safe, then regressed)">
          {summary.false_safe ?? 0}
        </Metric>
        <Metric testId="metric-hours" label="DBA hours">
          {(summary.hours_credited || 0).toFixed(1)} h credited ·
          {' '}{(summary.hours_pending || 0).toFixed(1)} h pending
        </Metric>
        <Metric testId="metric-reviews" label="Reviews">
          {summary.reviews || 0} ({(summary.reviews_per_week || 0).toFixed(1)} / week)
        </Metric>
        <Receipts receipts={summary.receipts} />
        <Metric testId="metric-caught" label="Caught before merge">
          {summary.caught_before_merge || 0}
        </Metric>
        <Metric testId="metric-tokens" label="Tokens per review">
          {formatNumber(Math.round(summary.tokens_per_review || 0))}
        </Metric>
        <Metric testId="metric-first-comment" label="PR opened to first comment (median)">
          {formatDuration(summary.first_comment_p50_seconds)}
        </Metric>
      </div>
      <Overrides byRule={summary.override_by_rule} />
      <AutoClearBar autoClear={summary.auto_clear} />
      <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
        Verdict mix: {Object.entries(summary.verdict_mix || {})
          .map(([v, n]) => `${humanize(v)} ${n}`).join(' · ') || DASH}
      </p>
    </div>
  )
}
