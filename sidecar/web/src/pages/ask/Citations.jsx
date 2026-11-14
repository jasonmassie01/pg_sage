import { citationHref, shortDigest } from '../../lib/ask'

// Citation markers and the citations list. Only evidence kinds with a page
// in this UI become links (lib/ask.js citationHref): a finding, action or
// investigation links to that object, list kinds to their page, and
// everything else is plain text. Markers and the list are keyed by alias
// (else id), so two reads of one id are two citations with their own
// digests. All text is rendered through React, never as HTML.

const muted = { color: 'var(--text-secondary)' }
const accent = { color: 'var(--accent)' }

export function CitationMarkers({ ids, numbers, byId, database }) {
  const seen = new Set()
  const markers = []
  for (const id of ids || []) {
    const n = numbers[id]
    if (n === undefined || seen.has(n)) continue
    seen.add(n)
    const href = citationHref(byId.get(id), database)
    markers.push(href
      ? <a key={n} href={href} className="ml-0.5 text-xs" style={accent}>[{n}]</a>
      : <span key={n} className="ml-0.5 text-xs" style={muted}>[{n}]</span>)
  }
  return markers
}

export function CitationList({ citations, database }) {
  if (!citations.length) return null
  return (
    <div data-testid="ask-citations">
      <h3 className="text-xs font-semibold uppercase" style={muted}>Evidence</h3>
      <ol className="space-y-1 mt-1">
        {citations.map((c, i) => <CitationItem key={`${c.id}-${i}`} c={c} n={i + 1}
          database={database} />)}
      </ol>
    </div>
  )
}

function CitationItem({ c, n, database }) {
  const href = citationHref(c, database)
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
      {c.excerpt_truncated && <span style={muted}>excerpt truncated</span>}
      {c.excerpt && <Excerpt text={c.excerpt} />}
    </li>
  )
}

// Excerpt is "what Sage read": the redacted text the model was shown when
// it answered, stored with the answer. It is rendered as React text in a
// <pre>, never as HTML. Answers stored before excerpts have none.
function Excerpt({ text }) {
  return (
    <details className="w-full">
      <summary className="cursor-pointer" style={muted}>What Sage read</summary>
      <pre data-testid="ask-citation-excerpt"
        className="mt-1 p-2 rounded overflow-x-auto whitespace-pre-wrap break-words"
        style={{ color: 'var(--text-primary)', border: '1px solid var(--border)' }}>
        {text}
      </pre>
    </details>
  )
}
