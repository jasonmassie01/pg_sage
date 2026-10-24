import {
  DASH, cardStyle, formatMs, formatNumber, formatTime, humanize, receiptState,
} from '../../lib/reviews'

// ReceiptPanel follows a review after it merges: awaiting landing (what
// the receipt will watch), landed (the bracketed landing time while the
// window runs), and decided (before/after numbers per watched queryid,
// and the incidents, lock findings and queryids that contradicted it).

function plural(n, word) {
  return `${n} ${word}${n === 1 ? '' : 's'}`
}

function pct(value) {
  if (value === null || value === undefined) return DASH
  const rounded = Math.round(value * 10) / 10
  return `${rounded > 0 ? '+' : ''}${rounded}%`
}

function Awaiting({ review }) {
  const exp = review.expectations || {}
  return (
    <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
      Watching for the change to land: {plural((exp.post_conditions || []).length,
        'catalog post-condition')}, {plural((exp.watch_queryids || []).length,
        'watched statement')}, or a signed deploy event linked to the PR.
    </p>
  )
}

function Landing({ review }) {
  if (!review.landed_at) return null
  return (
    <p data-testid="receipt-landing" className="text-sm"
      style={{ color: 'var(--text-secondary)' }}>
      Landed between {formatTime(review.landed_lower)} and {formatTime(review.landed_at)}
      {' '}(detected by {humanize(review.landing_source)}).
    </p>
  )
}

function Targets({ targets }) {
  if (!targets?.length) return null
  return (
    <table className="w-full text-left text-xs mt-2" aria-label="Watched statements">
      <thead>
        <tr style={{ color: 'var(--text-secondary)' }}>
          <th className="p-1">queryid</th><th className="p-1">before</th>
          <th className="p-1">after</th><th className="p-1">change</th>
          <th className="p-1">calls before / after</th><th className="p-1">verdict</th>
        </tr>
      </thead>
      <tbody>
        {targets.map(t => (
          <tr key={t.queryid} data-testid="receipt-target"
            style={{ borderTop: '1px solid var(--border)' }}>
            <td className="p-1 font-mono">{t.queryid}</td>
            <td className="p-1">{formatMs(t.before_mean_ms)}</td>
            <td className="p-1">{formatMs(t.after_mean_ms)}</td>
            <td className="p-1">{pct(t.change_pct)}</td>
            <td className="p-1">
              {formatNumber(t.before_calls)} / {formatNumber(t.after_calls)}
            </td>
            <td className="p-1">{humanize(t.verdict)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

function Line({ testId, label, children }) {
  return (
    <div data-testid={testId} className="text-xs mt-1"
      style={{ color: 'var(--text-secondary)' }}>
      {label}: {children}
    </div>
  )
}

function Harm({ receipt }) {
  const incidents = receipt.incidents || []
  const findings = receipt.lock_findings || []
  if (incidents.length === 0 && findings.length === 0) return null
  return (
    <div className="text-xs mt-1 flex flex-wrap gap-2">
      {incidents.map(id => (
        <a key={id} href="#/incidents" data-testid="receipt-incident"
          className="underline" style={{ color: 'var(--accent)' }}>
          incident {id}
        </a>
      ))}
      {findings.map(id => (
        <a key={id} href="#/cases" data-testid="receipt-lock-finding"
          className="underline" style={{ color: 'var(--accent)' }}>
          finding {id}
        </a>
      ))}
    </div>
  )
}

function Lists({ receipt }) {
  const born = receipt.born_queryids || []
  const contradicted = receipt.contradicted_by || []
  const confounders = receipt.confounders || []
  return (
    <>
      {contradicted.length > 0 && (
        <Line testId="receipt-contradicted" label="Contradicted by">
          {contradicted.join(', ')}
        </Line>
      )}
      {confounders.length > 0 && (
        <Line testId="receipt-confounders" label="Confounders">
          {confounders.join(', ')}
        </Line>
      )}
      {born.length > 0 && (
        <Line testId="receipt-born" label="New statements after landing (not judged)">
          {born.map(b => `${b.queryid} (${formatMs(b.mean_ms)}, ` +
            `${formatNumber(b.calls)} calls)`).join(', ')}
        </Line>
      )}
    </>
  )
}

function Decided({ review }) {
  const receipt = review.receipt
  return (
    <div>
      {receipt.reason && (
        <Line testId="receipt-reason" label="Reason">{humanize(receipt.reason)}</Line>
      )}
      <Line testId="receipt-window" label="Window">
        {receipt.window_hours}h before and after landing
      </Line>
      {receipt.tolerance && (
        <Line testId="receipt-tolerance" label="Against the prediction">
          {humanize(receipt.tolerance)}
        </Line>
      )}
      <Targets targets={receipt.targets} />
      <Harm receipt={receipt} />
      <Lists receipt={receipt} />
      {review.receipt_hash && (
        <Line testId="receipt-hash" label="Receipt hash">
          <span title={review.receipt_hash}>{review.receipt_hash.slice(0, 12)}</span>
        </Line>
      )}
    </div>
  )
}

function Body({ review }) {
  if (review.state !== 'complete') {
    return null
  }
  if (review.receipt) return <Decided review={review} />
  if (review.landing_state === 'awaiting') return <Awaiting review={review} />
  return null
}

export function ReceiptPanel({ review }) {
  const pending = review.state !== 'complete'
  return (
    <section data-testid="receipt-panel" className="rounded p-3" style={cardStyle}
      aria-label="Receipt">
      <div className="flex items-center gap-2">
        <h3 className="text-sm font-semibold" style={{ color: 'var(--text-primary)' }}>
          Receipt
        </h3>
        <span data-testid="receipt-state" className="text-xs"
          style={{ color: 'var(--text-secondary)' }}>
          {pending ? 'starts after the review completes' : receiptState(review)}
        </span>
      </div>
      {!pending && <Landing review={review} />}
      <Body review={review} />
    </section>
  )
}
