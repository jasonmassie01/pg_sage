import {
  DASH, budgetState, connectionLabel, findingsLabel, haLabel, healthLabel,
  healthReasons, stopLabel, trustState,
} from './runtimeStatus'

const CONNECTION_COLORS = {
  connected: 'var(--green)',
  disconnected: 'var(--red)',
  'not running': 'var(--yellow)',
}

// RuntimeCells renders one database's runtime state as table cells, the
// same on managed and read-only fleet rows (fleet-06). runtime is the
// GET /api/v1/databases row, or undefined when there is none; runtimeKnown
// says whether that inventory was read; registeredTrust, when given, is
// the registered trust level an effective level can override.
export function RuntimeCells({ runtime, runtimeKnown = true, registeredTrust, onSelectDatabase }) {
  const status = runtime?.status || {}
  const connection = connectionLabel(runtime, runtimeKnown)
  const ha = runtime ? haLabel(status) : ''
  const reasons = healthReasons(status)
  const trust = trustState(status, registeredTrust)
  const budget = budgetState(runtime)
  const muted = { color: 'var(--text-secondary)' }
  return (
    <>
      <td className="px-4 py-2"><span data-testid="runtime-connection"
        className="inline-flex items-center gap-2 whitespace-nowrap"
        style={{ color: CONNECTION_COLORS[connection] || 'var(--text-secondary)' }}>
        <span aria-hidden="true" className="w-1.5 h-1.5 rounded-full"
          style={{ background: 'currentColor' }} />{connection}
        {ha && <span className="text-xs" style={muted}>· {ha}</span>}
      </span></td>
      <td className="px-4 py-2 font-mono whitespace-nowrap" data-testid="runtime-findings"
        style={{ color: status.findings_critical > 0 ? 'var(--red)' : 'var(--text-primary)' }}>
        {runtime ? findingsLabel(status) : DASH}
      </td>
      <td className="px-4 py-2" data-testid="runtime-health">
        <span className="font-mono" style={{ color: 'var(--text-primary)' }}>
          {runtime ? healthLabel(status) : DASH}</span>
        {reasons.length > 0 && (
          <span className="block text-xs" style={muted}>{reasons.join('; ')}</span>
        )}
      </td>
      <td className="px-4 py-2 whitespace-nowrap" data-testid="runtime-trust" style={muted}>
        {runtime ? trust.effective : DASH}
        {runtime && trust.override && (
          <span data-testid="runtime-trust-override" className="ml-1 text-xs"
            style={{ color: 'var(--yellow)' }}
            title={`registered as ${registeredTrust}`}>override</span>
        )}
      </td>
      <td className="px-4 py-2" data-testid="runtime-stop"
        style={{ color: runtime?.emergency_stopped ? 'var(--red)' : 'var(--text-secondary)' }}>
        {stopLabel(runtime)}
      </td>
      <td className="px-4 py-2 font-mono whitespace-nowrap" data-testid="runtime-budget"
        style={{ color: budget.exhausted ? 'var(--red)' : 'var(--text-primary)' }}>
        {budget.label}{budget.exhausted ? ' · exhausted' : ''}
      </td>
      <td className="px-4 py-2">{runtime ? (
        <a href="#/database" data-testid="runtime-inspect"
          onClick={() => onSelectDatabase?.(runtime.name)}
          className="underline whitespace-nowrap" style={{ color: 'var(--accent)' }}>
          Inspect database</a>
      ) : DASH}</td>
    </>
  )
}
