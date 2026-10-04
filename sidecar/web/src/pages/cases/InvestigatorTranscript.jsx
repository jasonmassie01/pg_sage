import { EvidenceRef } from './InvestigationModel'

// The tool-calling investigator's transcript (roadmap 2.1): the plan and
// its budget, every tool call with its result, digest and evidence link,
// which results the conclusion cites, the model's outcome with the
// authority it got (advisory unless the family earned root authority),
// refused calls, dropped claims and why the loop stopped. Model text is
// untrusted and rendered as plain text.

const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }
const modelBox = { borderColor: 'var(--border)', borderStyle: 'dashed' }

function plural(n, word) {
  return `${n} ${word}${n === 1 ? '' : 's'}`
}

function counts(m) {
  return Object.entries(m || {}).filter(([, n]) => n > 0)
}

function Plan({ run }) {
  const b = run.budget || {}
  return (
    <div data-testid="investigator-plan" style={muted}>
      <span className="font-medium" style={strong}>
        {run.label || 'model investigator transcript'}
      </span>
      {': '}{run.plan} plan, budget {plural(b.max_steps || 0, 'model step')},{' '}
      {plural(b.max_probes || 0, 'probe')}, {Math.round((b.wall_ms || 0) / 1000)} s,{' '}
      {b.max_tokens || 0} tokens. Used {plural(run.model_calls || 0, 'model call')},{' '}
      {plural(run.tool_calls || 0, 'tool call')}, {plural(run.probes || 0, 'probe')}
      {run.protocol ? ` (${run.protocol} tool calls)` : ''}.
    </div>
  )
}

function target(args) {
  if (!args || typeof args !== 'object') return ''
  return args.probe || args.view || (args.queryid != null ? `queryid ${args.queryid}` : '')
}

function result(step, ev) {
  switch (step.status) {
    case 'rejected':
      return `refused: ${step.note || 'rejected'}`
    case 'tool_error':
      return `failed: ${step.note || 'error'}`
    case 'final':
      return 'final answer'
    case 'protocol':
      return step.note || 'switched protocol'
    default: {
      const rows = ev?.payload?.rows
      return Array.isArray(rows) ? `${step.status}, ${plural(rows.length, 'row')}`
        : step.status
    }
  }
}

function Step({ step, ev, known, cited, onCite }) {
  const seq = step.seq
  const what = target(step.args)
  return (
    <li data-testid={`investigator-step-${seq}`}>
      <span style={strong}>{step.tool || 'reply'}</span>
      {what && ` ${what}`}: {result(step, ev)}
      {step.digest && (
        <span data-testid={`investigator-digest-${seq}`} className="font-mono"
          title={step.digest}>
          {' '}sha256 {step.digest.slice(0, 12)}
        </span>
      )}
      {step.evidence_id && (
        <>
          {' '}(<EvidenceRef id={step.evidence_id} known={known} onCite={onCite} />)
        </>
      )}
      {step.evidence_id && cited.has(step.evidence_id) && (
        <span data-testid={`investigator-cited-${seq}`} style={strong}> cited</span>
      )}
    </li>
  )
}

function outcomeText(mc) {
  switch (mc.outcome) {
    case 'agreed':
      return `The model agrees with the root cause ${mc.root || mc.graph_root || ''}.`
    case 'concluded':
      return `The model concludes ${mc.root} on an inconclusive graph.`
    case 'contested':
      return `The model contests the graph's root ${mc.graph_root} with ${mc.root}.`
    case 'unmodeled':
      return 'The model names a cause the graph has no node for: '
    default:
      return 'The model found the evidence inconclusive.'
  }
}

function Outcome({ mc }) {
  if (!mc) return null
  const adopted = mc.authority === 'adopted'
  return (
    <div data-testid="investigator-outcome" data-authority={mc.authority} style={muted}>
      <div className="font-medium" style={strong}>{mc.label || 'model conclusion'}</div>
      {outcomeText(mc)}
      {mc.cause && <>{mc.cause.label}: {mc.cause.mechanism}</>}
      {' '}
      {adopted
        ? 'Adopted: pg_sage measured that the model may decide this family.'
        : 'Advisory: the causal graph decides the root cause.'}
      {mc.reason && <> ({mc.reason})</>}
    </div>
  )
}

function Tallies({ run, concluded }) {
  const dropped = counts(run.dropped_claims)
  const rejected = counts(run.rejected)
  const total = dropped.reduce((s, [, n]) => s + n, 0)
  return (
    <>
      <div data-testid="investigator-stop" style={muted}>
        Stopped: {run.stop}{run.stop_detail ? ` (${run.stop_detail})` : ''}.
        {!concluded && ' No usable conclusion: the causal graph\'s diagnosis stands.'}
      </div>
      {rejected.length > 0 && (
        <div data-testid="investigator-rejected" style={muted}>
          Refused calls: {rejected.map(([k, n]) => `${k} ${n}`).join(', ')}
        </div>
      )}
      {total > 0 && (
        <div data-testid="investigator-dropped" style={muted}>
          {plural(total, 'claim')} dropped (
          {dropped.map(([k, n]) => `${k} ${n}`).join(', ')})
        </div>
      )}
    </>
  )
}

// InvestigatorTranscript renders an investigation's investigator run, or
// nothing when the investigator did not run.
export function InvestigatorTranscript({ summary, evidence, onCite }) {
  const run = summary?.investigator
  if (!run) return null
  const byID = new Map((evidence || []).map(e => [e.id, e]))
  const known = new Set(byID.keys())
  const cited = new Set((summary.narrative?.claims || [])
    .flatMap(c => c.evidence_ids || []))
  const mc = summary.model_conclusion
  return (
    <div data-testid="investigator" className="space-y-1 rounded border p-1"
      style={modelBox} aria-label="Model investigator">
      <Plan run={run} />
      {run.model_plan && (
        <div data-testid="investigator-model-plan" style={muted}>
          Model plan: {run.model_plan}
        </div>
      )}
      <ol className="list-decimal pl-4" style={muted}>
        {(run.steps || []).map(s => (
          <Step key={s.seq} step={s} ev={byID.get(s.evidence_id)} known={known}
            cited={cited} onCite={onCite} />
        ))}
      </ol>
      <Outcome mc={mc} />
      <Tallies run={run} concluded={Boolean(mc)} />
    </div>
  )
}
