import { citationHref, shortDigest } from '../../lib/ask'

// Citation markers and the citations list. Only evidence kinds with a page
// in this UI become links (lib/ask.js citationHref); everything else is
// plain text. All text is rendered through React, never as HTML.

const muted = { color: 'var(--text-secondary)' }
const accent = { color: 'var(--accent)' }

export function CitationMarkers({ ids, numbers, byId }) {
  const seen = new Set()
  const markers = []
  for (const id of ids || []) {
    const n = numbers[id]
    if (n === undefined || seen.has(n)) continue
    seen.add(n)
    const href = citationHref(byId.get(id)?.kind)
    markers.push(href
      ? <a key={n} href={href} className="ml-0.5 text-xs" style={accent}>[{n}]</a>
      : <span key={n} className="ml-0.5 text-xs" style={muted}>[{n}]</span>)
  }
  return markers
}

export function CitationList({ citations }) {
  if (!citations.length) return null
  return (
    <div data-testid="ask-citations">
      <h3 className="text-xs font-semibold uppercase" style={muted}>Evidence</h3>
      <ol className="space-y-1 mt-1">
        {citations.map((c, i) => <CitationItem key={`${c.id}-${i}`} c={c} n={i + 1} />)}
      </ol>
    </div>
  )
}

function CitationItem({ c, n }) {
  const href = citationHref(c.kind)
  return (
    <li data-testid="ask-citation" className="text-xs flex flex-wrap gap-2">
      <span style={muted}>[{n}]</span>
      {href
        ? <a href={href} style={accent}>{c.label || c.id}</a>
        : <span style={{ color: 'var(--text-primary)' }}>{c.label || c.id}</span>}
      <span className="rounded px-1" style={{ ...muted, border: '1px solid var(--border)' }}>
        {c.kind}
      </span>
      {c.digest && <code style={muted} title="evidence digest">{shortDigest(c.digest)}</code>}
    </li>
  )
}
