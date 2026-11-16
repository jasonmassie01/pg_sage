import {
  dependants, expectedText, gateText, measuredText, stageStatus, stagesText, tierText,
  verdictText,
} from './plans'

// PlanStageRow is one stage of a plan (ux-03): status, evidence, the exact
// SQL, expected and measured effect, the ladder tier, the gate to the next
// stage and its rollback with what reverting it also reverts. An operator
// stage (plan-02d) reads "You run this" with the exact command and its
// observed landing, and has no rollback: pg_sage never reverts it. Every
// value renders as React text, SQL inside <pre>.

const muted = { color: 'var(--text-secondary)' }
const preStyle = { background: 'var(--bg-primary)', border: '1px solid var(--border)' }

function Code({ id, text }) {
  return <pre data-testid={id} className="rounded p-2 text-xs font-mono whitespace-pre-wrap
    break-all" style={preStyle}>{text}</pre>
}

function EvidenceLinks({ ids }) {
  if (!Array.isArray(ids) || ids.length === 0) return null
  return <p className="text-xs" style={muted}>Evidence:{' '}
    {ids.map((id, i) => <span key={id}>{i > 0 && ', '}
      <a className="underline" style={{ color: 'var(--accent)' }}
        href={`#/ledger?${new URLSearchParams({ evidence: id })}`}>{id}</a></span>)}
  </p>
}

function landingText(step) {
  const l = step.landing
  if (!l?.setting) return step.runtime?.landed_at ? 'landed' : 'waiting for the change'
  return step.runtime?.landed_at ? `${l.setting} reads ${l.value}`
    : `waiting for ${l.setting} to read ${l.value}`
}

function OperatorBody({ step }) {
  const n = step.ordinal
  return <>
    <p className="text-sm font-medium" style={{ color: 'var(--text-primary)' }}>You run this</p>
    <Code id={`plan-stage-command-${n}`} text={step.operator_command || ''} />
    <p className="text-xs" style={muted}>Observed landing: {landingText(step)}</p>
    <p className="text-xs" style={muted}>pg_sage cannot revert this stage
      {step.operator_revert ? `; to undo it, run: ${step.operator_revert}` : ''}</p>
  </>
}

function Rollback({ step, edges }) {
  const n = step.ordinal
  const also = dependants(edges, n)
  if (!step.rollback_sql) {
    return <p className="text-xs" style={muted}>Rollback: none recorded</p>
  }
  return <div className="text-xs space-y-1" style={muted}>
    <div>Rollback:</div>
    <Code id={`plan-stage-rollback-${n}`} text={step.rollback_sql} />
    {also.length > 0 && <p>Reverting it also reverts {stagesText(also)}, which depend on it</p>}
  </div>
}

function Effects({ step, outcome, next, ledgerError }) {
  const verdict = outcome?.verdict && outcome.verdict !== 'pending' ? outcome.verdict
    : step.runtime?.verdict
  return <div className="text-xs space-y-0.5" style={muted}>
    <p>Expected: {expectedText(step)} · {tierText(step, outcome)}</p>
    <p>Measured: {measuredText(outcome, ledgerError)}</p>
    {verdict && <p>Verdict: {verdictText(verdict)}</p>}
    <p>Gate: {gateText(step, next)}</p>
  </div>
}

export function PlanStageRow({ step, edges, next, outcome, ledgerError = '', onApproveStep,
  busy }) {
  const n = step.ordinal
  const operator = step.kind === 'operator'
  return <li data-testid={`plan-stage-${n}`} className="rounded border p-3 space-y-2"
    style={{ borderColor: 'var(--border)', background: 'var(--bg-card)' }}>
    <div className="flex flex-wrap items-center justify-between gap-2">
      <span className="font-medium" style={{ color: 'var(--text-primary)' }}>
        Stage {n}{step.action_type ? ` · ${step.action_type}` : ''}</span>
      <span data-testid={`plan-stage-status-${n}`} className="text-xs" style={muted}>
        {stageStatus(step.runtime)}</span>
    </div>
    {step.runtime?.state_reason && <p className="text-xs" style={muted}>
      {step.runtime.state_reason}</p>}
    <EvidenceLinks ids={step.evidence_ids} />
    {operator ? <OperatorBody step={step} /> : step.proposed_sql
      && <Code id={`plan-stage-sql-${n}`} text={step.proposed_sql} />}
    <Effects step={step} outcome={outcome} next={next} ledgerError={ledgerError} />
    {!operator && <Rollback step={step} edges={edges} />}
    {onApproveStep && !operator && <button type="button" disabled={busy} aria-busy={busy}
      onClick={() => onApproveStep(step)} className="px-3 py-1.5 rounded text-sm"
      style={{ border: '1px solid var(--accent)', color: 'var(--accent)' }}>
      Approve stage {n}</button>}
  </li>
}
