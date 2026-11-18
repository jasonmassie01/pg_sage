import { modelPassText, planHref, statusLabel } from '../../lib/review'

const card = { background: 'var(--bg-card)', border: '1px solid var(--border)' }
const muted = { color: 'var(--text-secondary)' }

// ReviewReport is the review's report: the ranked findings, then, when a
// model pass is done, its cross-dimension links, its hypotheses (advice
// only: pg_sage never acts on one) and its text, and the plan link. Every
// string is React text: model output is never parsed as HTML or markdown.
export function ReviewReport({ report, database }) {
  if (!report) return null
  const model = report.model_pass?.status === 'done'
  return (
    <section data-testid="review-report" data-run={report.run_id}
      className="rounded p-4 space-y-3" style={card} aria-labelledby="report-heading">
      <h2 id="report-heading" className="text-sm font-medium" style={muted}>Report</h2>
      <p data-testid="model-pass" className="text-xs" style={muted}>
        {modelPassText(report.model_pass)}
      </p>
      <Findings findings={report.findings || []} />
      {model && <ModelSection report={report} />}
      {report.plan_id && (
        <a href={planHref(report.plan_id, database)} className="inline-block text-sm underline"
          style={{ color: 'var(--accent)' }}>Review the plan</a>
      )}
    </section>
  )
}

function Findings({ findings }) {
  if (findings.length === 0) {
    return <p className="text-sm">No check failed or warned in this grade.</p>
  }
  return (
    <ol className="space-y-2">
      {findings.map(f => (
        <li key={`${f.rank}-${f.check}`} data-testid="report-finding" className="text-sm"
          style={{ overflowWrap: 'anywhere' }}>
          <span className="font-semibold">{f.rank}.</span>{' '}
          <span className="font-semibold">{statusLabel(f.status)}</span>
          {f.critical ? ' (critical)' : ''}{' · '}{f.dimension_title}{' · '}
          <span className="font-mono">{f.check}</span>
          {f.reason ? <span>{` — ${f.reason}`}</span> : null}
          {f.evidence?.length > 0 && (
            <span className="block text-xs font-mono" style={muted}>
              Evidence: {f.evidence.join(', ')}
            </span>
          )}
        </li>
      ))}
    </ol>
  )
}

function ModelSection({ report }) {
  return (
    <div data-testid="model-section" className="space-y-2">
      {(report.sections || []).map((s, i) => (
        <p key={`${s.dimension}-${i}`} className="text-sm" style={{ overflowWrap: 'anywhere' }}>
          <span className="font-semibold">{s.dimension}: </span>{s.text}
        </p>
      ))}
      {(report.links || []).map((l, i) => (
        <p key={`link-${i}`} data-testid="report-link" className="text-sm"
          style={{ overflowWrap: 'anywhere' }}>
          Linked: <span className="font-mono">{l.a}</span> and{' '}
          <span className="font-mono">{l.b}</span>{l.relation ? `: ${l.relation}` : ''}
        </p>
      ))}
      {(report.hypotheses || []).map((h, i) => (
        <div key={`hyp-${i}`} data-testid="report-hypothesis" className="text-sm"
          style={{ overflowWrap: 'anywhere' }}>
          <p><span className="font-semibold">Advice: </span>{h.title}</p>
          {h.rationale && <p style={muted}>{h.rationale}</p>}
          <p className="text-xs" style={muted}>
            A hypothesis from the model: pg_sage does not act on advice.
          </p>
        </div>
      ))}
    </div>
  )
}
