// StackedAreaChart: an SVG stacked area chart of per-sample counts by
// class (market-01, the wait-class load), with no chart dependency.
// series is [{ t, by_class: { cls: n } }] oldest first; classes orders the
// stack bottom up. Each class's band is drawn between the classes below it
// and itself.

import { stackSeries } from '../lib/workload'

const WIDTH = 640
const HEIGHT = 160
const PALETTE = ['var(--accent)', 'var(--red)', 'var(--yellow)', 'var(--green)',
  '#8b5cf6', '#06b6d4', '#f97316', '#64748b']


function bandPath(points, x, y) {
  const top = points.map(([, upper], i) => `${x(i)},${y(upper)}`)
  const bottom = points.map(([lower], i) => `${x(i)},${y(lower)}`).reverse()
  return `M${top.join(' L')} L${bottom.join(' L')} Z`
}

export function StackedAreaChart({ series, classes, unit = 'sessions' }) {
  if (!series?.length || !classes?.length) {
    return (
      <p data-testid="stacked-empty" className="text-sm"
        style={{ color: 'var(--text-secondary)' }}>
        No samples in this window.
      </p>
    )
  }
  const bands = stackSeries(series, classes)
  const max = Math.max(1, ...bands[bands.length - 1].points.map(([, upper]) => upper))
  const x = i => (series.length === 1 ? WIDTH / 2 : (i / (series.length - 1)) * WIDTH)
  const y = v => HEIGHT - (v / max) * HEIGHT
  return (
    <div>
      <svg viewBox={`0 0 ${WIDTH} ${HEIGHT}`} className="w-full h-40" role="img"
        aria-label={`Stacked ${unit} by class, peak ${max}`}>
        {bands.map((band, i) => (
          <path key={band.cls} data-testid={`stack-${band.cls}`}
            d={bandPath(band.points, x, y)} fill={PALETTE[i % PALETTE.length]}
            fillOpacity="0.7" stroke="none" />
        ))}
      </svg>
      <div className="mt-2 flex flex-wrap gap-3 text-xs"
        style={{ color: 'var(--text-secondary)' }}>
        {bands.map((band, i) => (
          <span key={band.cls} className="flex items-center gap-1">
            <span className="inline-block w-3 h-3 rounded-sm"
              style={{ background: PALETTE[i % PALETTE.length] }} />
            {band.cls}
          </span>
        ))}
        <span>peak {max} {unit}</span>
      </div>
    </div>
  )
}
