// ModelTierView renders a workload pack's model tier (evid-02c): exactly
// what would leave the network if a model read the pack. Each section is
// shown as the model would receive it; charts are the PNG sparklines of the
// top families. The pseudonym map, when present, is local only and is shown
// apart from the tier.

function formatBytes(n) {
  if (!Number.isFinite(n)) return 'unknown size'
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / (1024 * 1024)).toFixed(1)} MB`
}

function chartLabel(name) {
  const file = name.replace(/^charts\//, '').replace(/\.png$/, '')
  return file.endsWith('-growth')
    ? `family ${file.replace(/-growth$/, '')}: weekly exec time`
    : `family ${file}: hour-of-week exec time`
}

function Section({ name, value }) {
  let text
  try {
    text = JSON.stringify(value, null, 2)
  } catch (err) {
    if (!(err instanceof TypeError)) throw err
    text = `(${name} cannot be shown: ${err.message})`
  }
  return (
    <details className="rounded p-2" data-testid={`model-section-${name}`}
      style={{ border: '1px solid var(--border)' }}>
      <summary className="text-xs font-mono cursor-pointer"
        style={{ color: 'var(--text-primary)' }}>
        {name}
      </summary>
      <pre className="text-xs overflow-auto mt-2 max-h-80"
        style={{ color: 'var(--text-secondary)' }}>
        {text}
      </pre>
    </details>
  )
}

export function ModelTierView({ view }) {
  const sections = view?.sections && typeof view.sections === 'object' ? view.sections : {}
  const charts = view?.charts && typeof view.charts === 'object' ? view.charts : {}
  const names = Object.keys(sections).sort()
  const chartNames = Object.keys(charts).sort()
  const pseudonyms = view?.pseudonyms && typeof view.pseudonyms === 'object'
    ? Object.entries(view.pseudonyms).sort(([a], [b]) => a.localeCompare(b)) : []
  return (
    <div className="space-y-3 mt-3" data-testid="model-tier">
      <p className="text-xs" data-testid="model-tier-size"
        style={{ color: 'var(--text-secondary)' }}>
        Model tier {String(view?.content_sha256 || '').slice(0, 12)}:{' '}
        {formatBytes(view?.bytes)}, about {view?.tokens_estimate ?? '?'} tokens,{' '}
        {names.length} sections, {chartNames.length} charts.
        {view?.hash_identifiers ? ' Names are pseudonyms.' : ' Names are shown as they are.'}
      </p>
      {chartNames.length > 0 && (
        <div className="grid gap-2" data-testid="model-tier-charts"
          style={{ gridTemplateColumns: 'repeat(auto-fill, minmax(336px, 1fr))' }}>
          {chartNames.map(name => (
            <figure key={name} className="m-0">
              <img src={charts[name]} alt={chartLabel(name)} width={336} height={64}
                style={{ background: '#fff', borderRadius: 4 }} />
              <figcaption className="text-xs" style={{ color: 'var(--text-secondary)' }}>
                {chartLabel(name)}
              </figcaption>
            </figure>
          ))}
        </div>
      )}
      <div className="space-y-1">
        {names.map(name => <Section key={name} name={name} value={sections[name]} />)}
      </div>
      {pseudonyms.length > 0 && (
        <details className="rounded p-2" data-testid="model-tier-pseudonyms"
          style={{ border: '1px dashed var(--border)' }}>
          <summary className="text-xs cursor-pointer"
            style={{ color: 'var(--text-secondary)' }}>
            Pseudonym map ({pseudonyms.length}, local only: never part of the tier)
          </summary>
          <ul className="text-xs font-mono mt-2" style={{ color: 'var(--text-secondary)' }}>
            {pseudonyms.map(([p, n]) => <li key={p}>{p} = {n}</li>)}
          </ul>
        </details>
      )}
    </div>
  )
}
