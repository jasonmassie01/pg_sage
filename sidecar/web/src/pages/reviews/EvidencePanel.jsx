// EvidencePanel shows one cited evidence item: its id, kind, label, the
// first 12 characters of its sha256 digest and the data it rests on.

export function EvidencePanel({ id, evidence }) {
  const item = (evidence || []).find(e => e.id === id)
  const muted = { color: 'var(--text-secondary)' }
  if (!item) {
    return (
      <div data-testid="evidence-panel" className="mt-2 rounded border p-2 text-xs"
        style={{ borderColor: 'var(--border)', ...muted }}>
        Evidence {id} is not in this review.
      </div>
    )
  }
  return (
    <div data-testid="evidence-panel" className="mt-2 rounded border p-2 text-xs"
      style={{ borderColor: 'var(--border)' }}>
      <div className="flex flex-wrap gap-2" style={muted}>
        <span className="font-mono">{item.id}</span>
        <span>{item.kind}</span>
        {item.digest && (
          <span title={item.digest}>sha256 {item.digest.slice(0, 12)}</span>
        )}
      </div>
      <div className="mt-1" style={{ color: 'var(--text-primary)' }}>{item.label}</div>
      {item.data !== undefined && item.data !== null && (
        <pre className="mt-1 overflow-x-auto whitespace-pre-wrap"
          style={{ color: 'var(--text-secondary)' }}>
          {JSON.stringify(item.data, null, 2)}
        </pre>
      )}
    </div>
  )
}
