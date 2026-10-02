// What a signed runbook did in an investigation (AI-SRE-SPEC §7.1): the
// version that ran and its signer, the path through its DAG, and its
// proposal, which the runbook never executes. An abstention shows why.

const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }

export function RunbookResult({ run }) {
  if (!run) return null
  const proposal = run.proposal
  return (
    <div data-testid="runbook-result">
      <div className="font-medium" style={strong}>Runbook</div>
      <div style={muted}>
        {run.name} v{run.version} ({run.label}, signed by {run.signed_by}):{' '}
        {run.outcome}, {run.probes} probe{run.probes === 1 ? '' : 's'}
      </div>
      <div style={muted}>Path: {(run.path || []).join(' → ')}</div>
      {run.reason && <div style={muted}>Reason: {run.reason}</div>}
      {proposal && (
        <div style={muted}>
          <span style={strong}>{proposal.label}:</span> {proposal.text}
        </div>
      )}
    </div>
  )
}
