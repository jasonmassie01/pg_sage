import { formatActionTime, rollbackClassLabel } from './actionFormat'

// LifecycleDetails shows a pending action's expiry, cooldown, rollback
// class, guardrails and block or defer reasons; nothing when none is set.
export function LifecycleDetails({ row }) {
  const expiresAt = formatActionTime(row.expires_at)
  const cooldownUntil = formatActionTime(row.cooldown_until)
  const guardrails = row.guardrails || []
  const rollbackClass = rollbackClassLabel(row.rollback_class)
  if (!expiresAt && !cooldownUntil && guardrails.length === 0 &&
    !row.blocked_reason && !row.attempt_count && !rollbackClass) {
    return null
  }
  return (
    <div className="space-y-2 text-xs"
      style={{ color: 'var(--text-secondary)' }}>
      <div className="flex flex-wrap gap-2">
        {row.attempt_count > 0 && <span>Attempts: {row.attempt_count}</span>}
        {expiresAt && <span>Expires: {expiresAt}</span>}
        {cooldownUntil && <span>Cooldown until: {cooldownUntil}</span>}
        {rollbackClass && <span>{rollbackClass}</span>}
        {row.verification_status && (
          <span>Verification: {row.verification_status}</span>
        )}
      </div>
      {row.blocked_reason && (
        <div style={{ color: 'var(--text-primary)' }}>
          {row.blocked_reason}
        </div>
      )}
      {row.defer_reason && row.defer_reason !== row.blocked_reason && (
        <div style={{ color: 'var(--text-primary)' }}>
          {row.defer_reason}
        </div>
      )}
      {guardrails.length > 0 && (
        <div className="flex flex-wrap gap-1.5">
          {guardrails.map(g => (
            <span key={g} className="rounded px-1.5 py-0.5"
              style={{ border: '1px solid var(--border)' }}>
              {g}
            </span>
          ))}
        </div>
      )}
    </div>
  )
}
