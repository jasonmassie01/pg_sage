import { formatTime } from '../../lib/time'
// apiauth-01: deactivation is the default offboarding step. A deactivated
// user cannot sign in, their sessions end and their operator MCP tokens are
// revoked; their history stays attributable. Delete stays available.

export function DisabledBadge({ user }) {
  if (!user.disabled) return null
  return (
    <span data-testid="user-disabled-badge"
      title={user.disabled_at
        ? `Deactivated ${formatTime(user.disabled_at)}`
        : 'Deactivated'}
      className="ml-2 px-1.5 py-0.5 rounded text-xs"
      style={{ background: 'var(--bg-main)', color: 'var(--text-secondary)',
        border: '1px solid var(--border)' }}>
      Deactivated
    </span>
  )
}

export function UserStatusButton({ user, locked, lockTitle, onToggle }) {
  const label = user.disabled ? 'Reactivate' : 'Deactivate'
  return (
    <button
      onClick={() => onToggle(user)}
      disabled={locked}
      title={locked ? lockTitle : undefined}
      data-testid="user-status-button"
      className="px-2 py-1 rounded text-xs"
      style={{ color: 'var(--accent)', opacity: locked ? 0.6 : 1 }}>
      {label}
    </button>
  )
}
