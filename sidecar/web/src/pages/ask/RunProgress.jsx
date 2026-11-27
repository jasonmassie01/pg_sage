// RunProgress: the active Ask Sage run (mcp-07b): its state, the tools it
// has called so far (never their output) and a Cancel button that posts
// once. A run that already acted keeps its partial answer when cancelled.

const muted = { color: 'var(--text-secondary)' }
const card = { background: 'var(--bg-card)', border: '1px solid var(--border)' }

const STATE_LABELS = { queued: 'Waiting to start…', running: 'Sage is working…' }

export function RunProgress({ run, onCancel }) {
  if (!run) return null
  const steps = Array.isArray(run.progress) ? run.progress : []
  return (
    <section data-testid="ask-run" aria-live="polite" className="rounded p-3 text-sm space-y-2"
      style={card}>
      <p data-testid="ask-run-state" style={muted}>
        {STATE_LABELS[run.state] || 'Sage is working…'} You can close this page; the
        answer will be stored in this conversation.
      </p>
      {steps.length > 0 && (
        <ol className="list-decimal pl-5 space-y-1">
          {steps.map((step, i) => (
            <li key={`${i}-${step.tool}`} data-testid={`ask-run-step-${i}`}>
              <code>{String(step.tool)}</code> <span style={muted}>{String(step.status)}</span>
              {step.action_id ? <span> (action {String(step.action_id)})</span> : null}
            </li>
          ))}
        </ol>
      )}
      <button type="button" data-testid="ask-run-cancel" disabled={Boolean(run.cancelling)}
        onClick={onCancel} className="rounded px-3 py-1"
        style={{ border: '1px solid var(--border)', color: 'var(--text-primary)' }}>
        {run.cancelling ? 'Cancelling…' : 'Cancel'}
      </button>
    </section>
  )
}
