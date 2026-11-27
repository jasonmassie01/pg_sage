/* eslint-disable react-refresh/only-export-components */
import { RotateCcw } from 'lucide-react'

// config-09: GET /api/v1/config/global lists each saved setting that is not
// in force yet (pending) and whether POST /api/v1/restart can apply them
// (restart_supported). The page shows the desired config, so this panel is
// what says which values the running sidecar does not use yet.

export function formatPendingValue(value) {
  if (value === '' || value == null) return '(empty)'
  if (Array.isArray(value)) return value.length ? value.join(', ') : '(empty)'
  if (typeof value === 'object') return JSON.stringify(value)
  return String(value)
}

function PendingRow({ entry }) {
  return (
    <li data-testid={`pending-${entry.key}`}
      className="flex flex-wrap items-baseline gap-x-2 py-0.5">
      <code style={{ color: 'var(--text-primary)' }}>{entry.key}</code>
      {entry.hidden ? (
        <span>value hidden (secret or not shown in the UI)</span>
      ) : (
        <span>
          running <span data-testid="pending-active">
            {formatPendingValue(entry.active)}</span>
          {' '}&rarr; after restart <span data-testid="pending-desired">
            {formatPendingValue(entry.desired)}</span>
        </span>
      )}
    </li>
  )
}

export function PendingRestartPanel({ pending }) {
  const entries = Array.isArray(pending) ? pending.filter(e => e?.key) : []
  if (entries.length === 0) return null
  const count = entries.length === 1
    ? '1 setting waits for restart'
    : `${entries.length} settings wait for restart`
  return (
    <div data-testid="settings-pending-panel" data-tone="info" role="status"
      className="rounded p-3 text-xs"
      style={{
        background: 'var(--bg-card)',
        border: '1px solid var(--accent)',
        color: 'var(--text-secondary)',
      }}>
      <div className="font-medium mb-1" style={{ color: 'var(--accent)' }}>
        {count}
      </div>
      <p className="mb-1">
        Saved, but the running sidecar still uses its startup value. The
        fields below show the saved value.
      </p>
      <ul>
        {entries.map(entry => <PendingRow key={entry.key} entry={entry} />)}
      </ul>
    </div>
  )
}

export function RestartControl({ supported, restarting, onRestart }) {
  if (!supported) {
    return (
      <span data-testid="settings-restart-hint" className="max-w-xs">
        Restart the sidecar through your supervisor (systemd, Kubernetes
        rollout) or set SAGE_SUPERVISED=1 to restart it from here.
      </span>
    )
  }
  return (
    <button
      data-testid="settings-restart-btn"
      onClick={onRestart}
      disabled={restarting}
      className="text-xs px-3 py-1 rounded whitespace-nowrap inline-flex items-center gap-1"
      style={{ color: 'var(--text)', border: '1px solid var(--border)' }}
    >
      <RotateCcw size={13} />
      {restarting ? 'Restarting…' : 'Restart now'}
    </button>
  )
}
