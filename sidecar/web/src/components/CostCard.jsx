// market-04: the Value page's Cost card. Dollars always show with their
// rate source and date; without operator rates the card says so and shows
// no figure. pg_sage ships no price tables.

import { formatMoney } from '../lib/money'

const cardStyle = { background: 'var(--bg-card)', borderColor: 'var(--border)' }
const mutedStyle = { color: 'var(--text-secondary)' }

function formatShare(share) {
  if (share === null || share === undefined) return 'unknown'
  return `${(Number(share) * 100).toFixed(1)}%`
}

function casesLink(database) {
  return `#/cases?${new URLSearchParams({ database })}`
}

function Figure({ label, amount, currency }) {
  return (
    <div>
      <div className="text-xs" style={mutedStyle}>{label}</div>
      <div className="text-xl font-semibold">{formatMoney(amount, currency)}</div>
    </div>
  )
}

function Unavailable({ reason }) {
  return (
    <section aria-label="Cost" className="rounded border p-4" style={cardStyle}>
      <h2 className="text-lg font-semibold">Cost</h2>
      <p className="mt-1 text-sm" style={mutedStyle}>
        {reason === 'rates not configured'
          ? 'Rates not configured: set cost.rates to see what each instance and ' +
            'query costs.'
          : `Cost unavailable: ${reason || 'unknown reason'}.`}
      </p>
    </section>
  )
}

function TopQueries({ rows, currency }) {
  if (!rows.length) {
    return (
      <p className="mt-3 text-sm" style={mutedStyle}>
        No query time recorded in this window.
      </p>
    )
  }
  return (
    <table aria-label="Top queries by cost" className="mt-3 w-full text-sm">
      <thead>
        <tr style={mutedStyle}>
          <th scope="col" className="py-2 text-left">Query</th>
          <th scope="col" className="py-2 text-left">Database</th>
          <th scope="col" className="py-2 text-right">Share</th>
          <th scope="col" className="py-2 text-right">Cost</th>
        </tr>
      </thead>
      <tbody>
        {rows.map(row => (
          <tr key={`${row.database}:${row.queryid}`}
            style={{ borderTop: '1px solid var(--border)' }}>
            <th scope="row" className="py-2 text-left font-mono font-normal">
              {row.queryid}
            </th>
            <td className="py-2">
              <a className="underline" href={casesLink(row.database)}
                title={`Issues on ${row.database}`}>{row.database}</a>
            </td>
            <td className="py-2 text-right">{formatShare(row.share)}</td>
            <td className="py-2 text-right">
              {formatMoney(row.cost, row.currency || currency)}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

export function CostCard({ cost }) {
  if (!cost || !cost.available) return <Unavailable reason={cost?.reason} />
  const currency = cost.currency
  const hours = Number(cost.window_hours || 0).toFixed(0)
  return (
    <section aria-label="Cost" className="rounded border p-4" style={cardStyle}>
      <h2 className="text-lg font-semibold">Cost</h2>
      <p className="mt-1 text-sm" data-testid="cost-source" style={mutedStyle}>
        Rates from {cost.rate_source}, as of {cost.as_of}. Last {hours} h.
        {currency === 'mixed' &&
          ' Instances are priced in different currencies, so there is no total.'}
      </p>
      <div className="mt-3 flex flex-wrap gap-8">
        <Figure label="Total" amount={cost.total} currency={currency} />
        <Figure label="Compute" amount={cost.compute} currency={currency} />
        {cost.storage !== null && cost.storage !== undefined &&
          <Figure label="Storage" amount={cost.storage} currency={currency} />}
        <div>
          <div className="text-xs" style={mutedStyle}>Idle capacity</div>
          <div className="text-xl font-semibold">{formatShare(cost.idle_share)}</div>
        </div>
      </div>
      {cost.unpriced?.length > 0 && (
        <p className="mt-2 text-sm" style={mutedStyle}>
          Not priced (no rate): {cost.unpriced.join(', ')}.
        </p>
      )}
      {cost.partial && (
        <p role="alert" className="mt-2 text-sm" style={{ color: 'var(--yellow)' }}>
          Partial: {(cost.unavailable || []).join('; ')}
        </p>
      )}
      <TopQueries rows={cost.top_queries || []} currency={currency} />
    </section>
  )
}
