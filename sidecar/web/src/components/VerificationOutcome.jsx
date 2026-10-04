// Phase 1.3: what pg_sage predicted an action would do, what it observed,
// the verdict and the evidence behind it. Only "Improved" earns trust;
// neutral, insufficient evidence and unverifiable are neither credit nor
// harm; a regression is rolled back.

import { VERDICTS } from '../lib/verificationOutcome'

const METHODS = {
  hypopg: 'HypoPG', model: 'model estimate', rule: 'rule', none: 'none',
}

const TOLERANCES = {
  met: 'Met prediction', partial: 'Partly met prediction',
  missed: 'Missed prediction', no_prediction: 'No prediction',
  unmeasured: 'Not measured', pending: 'Pending',
}

const METRICS = {
  mean_exec_time: 'mean exec time', temp_spills: 'temp files',
  dead_tuples: 'dead tuples', hot_updates: 'HOT update share',
  n_mod_since_analyze: 'rows modified since analyze', rows_deleted: 'rows deleted',
}

const TRIGGERS = {
  query_regression: 'query regression',
  hint_reference: 'an active hint names the index',
}

function formatPct(value) {
  if (value === null || value === undefined || Number.isNaN(Number(value))) {
    return null
  }
  const n = Number(value)
  const digits = Number.isInteger(n) ? 0 : 1
  return `${n > 0 ? '+' : ''}${n.toFixed(digits)}%`
}

function creditNote(verdict) {
  if (verdict === 'improved') return 'Counts toward earned trust.'
  if (verdict === 'regressed') return 'Counts against earned trust; rolled back.'
  if (verdict === 'pending') return 'Verification in progress.'
  return 'Neutral for trust: not credited.'
}

function predictedText(predicted) {
  const change = formatPct(predicted?.expected_change_pct)
  if (!predicted || predicted.method === 'none' || change === null) {
    return `No prediction${predicted?.note ? ` (${predicted.note})` : ''}`
  }
  const metric = METRICS[predicted.metric] || predicted.metric || ''
  return `${change} ${metric} (${METHODS[predicted.method] || predicted.method})`
}

function observedText(observed) {
  const change = formatPct(observed?.change_pct)
  if (change === null) return 'Not measured'
  const metric = METRICS[observed.metric] || observed.metric || ''
  return `${change} ${metric}`
}

function Row({ label, testId, children }) {
  return (
    <div className="flex gap-2">
      <span className="w-24 shrink-0" style={{ color: 'var(--text-secondary)' }}>
        {label}
      </span>
      <span data-testid={testId}>{children}</span>
    </div>
  )
}

function ComparisonEvidence({ comparison }) {
  if (!comparison?.before || !comparison?.after) return null
  const mean = m => (m.mean_ms === undefined ? '' : ` (mean ${Number(m.mean_ms).toFixed(1)} ms)`)
  const ci = comparison.ci_low_pct !== null && comparison.ci_low_pct !== undefined
    ? `; 95% CI ${Number(comparison.ci_low_pct).toFixed(1)}% .. ` +
      `${Number(comparison.ci_high_pct).toFixed(1)}%`
    : ''
  return (
    <Row label="Evidence" testId="outcome-evidence">
      Calls before {comparison.before.calls}{mean(comparison.before)}, after{' '}
      {comparison.after.calls}{mean(comparison.after)}{ci}
    </Row>
  )
}

function SoftDrop({ softDrop }) {
  if (!softDrop) return null
  const trigger = TRIGGERS[softDrop.trigger] || softDrop.trigger
  return (
    <Row label="Soft drop" testId="outcome-soft-drop">
      {softDrop.recreated
        ? `Index re-created on the first miss (${trigger}).`
        : `First miss (${trigger}); the re-create did not complete.`}
    </Row>
  )
}

export function VerificationOutcome({ outcome }) {
  if (!outcome) return null
  const verdict = VERDICTS[outcome.verdict] || { label: outcome.verdict, color: 'inherit' }
  const targets = outcome.predicted?.target_queryids || []
  return (
    <div data-testid="verification-outcome" className="p-3 rounded text-xs space-y-1"
      style={{ border: '1px solid var(--border)' }}>
      <div className="flex items-center gap-2">
        <span className="font-medium" style={{ color: 'var(--text-secondary)' }}>
          Verification
        </span>
        <span data-testid="outcome-verdict" className="font-medium"
          style={{ color: verdict.color }}>{verdict.label}</span>
        <span style={{ color: 'var(--text-secondary)' }}>{creditNote(outcome.verdict)}</span>
      </div>
      <Row label="Predicted" testId="outcome-predicted">
        {predictedText(outcome.predicted)}
      </Row>
      <Row label="Observed" testId="outcome-observed">{observedText(outcome.observed)}</Row>
      <Row label="Tolerance" testId="outcome-tolerance">
        {TOLERANCES[outcome.tolerance] || outcome.tolerance}
      </Row>
      {targets.length > 0 && (
        <Row label="Targets" testId="outcome-targets">
          queryid {targets.join(', ')}
        </Row>
      )}
      <ComparisonEvidence comparison={outcome.evidence?.comparison} />
      <SoftDrop softDrop={outcome.evidence?.soft_drop} />
      {outcome.reason && <p style={{ color: 'var(--text-primary)' }}>{outcome.reason}</p>}
    </div>
  )
}
