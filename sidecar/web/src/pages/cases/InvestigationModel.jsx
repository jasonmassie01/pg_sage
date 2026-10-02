// The model turn's output in an investigation (Sage SRE M3/M4, AI-SRE-SPEC
// §9): the model ranking, model-generated claims with the evidence they
// cite, and the one probe the model asked for. Everything here is labeled
// as model output and kept apart from the causal graph's scores: the
// ranking is an order, never a confidence. Model text is untrusted and is
// rendered as plain text.

const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }
const modelBox = { borderColor: 'var(--border)', borderStyle: 'dashed' }

function EvidenceRef({ id, known, onCite }) {
  if (!known.has(id)) {
    return (
      <span data-testid={`evidence-missing-${id}`} style={muted}>evidence unavailable</span>
    )
  }
  return (
    <a href={`#evidence-${id}`} data-testid={`evidence-link-${id}`}
      className="underline" style={{ color: 'var(--accent)' }}
      onClick={() => onCite(id)}>
      evidence
    </a>
  )
}

function Refs({ ids, known, onCite }) {
  return (
    <>
      {' '}(
      {ids.map((id, i) => (
        <span key={id}>
          {i > 0 && ', '}
          <EvidenceRef id={id} known={known} onCite={onCite} />
        </span>
      ))}
      )
    </>
  )
}

function ModelRanking({ ranking, labels }) {
  const nodes = ranking?.nodes || []
  if (nodes.length === 0) return null
  return (
    <div data-testid="investigation-model-ranking">
      <div className="font-medium" style={strong}>{ranking.label || 'model ranking'}</div>
      <div style={muted}>{ranking.basis}</div>
      <ol className="list-decimal pl-4" style={muted}>
        {nodes.map(n => <li key={n}>{labels[n] || n}</li>)}
      </ol>
    </div>
  )
}

function Narrative({ narrative, known, onCite }) {
  const claims = narrative?.claims || []
  if (claims.length === 0) return null
  return (
    <div data-testid="investigation-narrative">
      <div className="font-medium" style={strong}>
        {narrative.label || 'model-generated narrative'}
      </div>
      <ul className="list-disc pl-4" style={muted}>
        {claims.map(c => (
          <li key={`${c.text}-${(c.evidence_ids || []).join(',')}`}>
            {c.text}
            <Refs ids={c.evidence_ids || []} known={known} onCite={onCite} />
          </li>
        ))}
      </ul>
    </div>
  )
}

function ModelProbe({ probe, known, onCite }) {
  if (!probe?.probe_id) return null
  return (
    <div data-testid="investigation-model-probe">
      <div className="font-medium" style={strong}>{probe.label || 'model-proposed probe'}</div>
      <div style={muted}>
        {probe.probe_id}: {probe.rationale}
        {probe.evidence_id && (
          <Refs ids={[probe.evidence_id]} known={known} onCite={onCite} />
        )}
      </div>
    </div>
  )
}

// ModelOutput renders the model's part of an investigation, or nothing
// for a deterministic one.
export function ModelOutput({ investigation, hypotheses, evidence, onCite }) {
  const summary = investigation?.summary || {}
  const turns = investigation?.model_turns || 0
  const hasOutput = summary.model_ranking || summary.narrative || summary.model_probe
  if (!turns && !hasOutput) return null
  const labels = Object.fromEntries((hypotheses || []).map(h => [h.node, h.label]))
  const known = new Set((evidence || []).map(e => e.id))
  return (
    <div className="space-y-2 rounded border p-1" style={modelBox}
      aria-label="Model output">
      {turns > 0 && (
        <div data-testid="investigation-model-turns" style={muted}>
          Model turns: {turns}. The causal graph decides the root cause; the model may
          only reorder its hypotheses, ask for one probe and narrate cited claims.
        </div>
      )}
      <ModelRanking ranking={summary.model_ranking} labels={labels} />
      <Narrative narrative={summary.narrative} known={known} onCite={onCite} />
      <ModelProbe probe={summary.model_probe} known={known} onCite={onCite} />
    </div>
  )
}
