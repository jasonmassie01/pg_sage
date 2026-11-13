import { useState } from 'react'
import { ConfirmDialog } from '../../components/ConfirmDialog'

// apiauth-03: an admin resets a password user's password. The temporary
// password is returned once, shown here once with a copy button, and gone on
// dismiss; the user must replace it at their next sign-in.

// ResetPasswordDialog asks before the reset (dashboard-10b). reset is a
// useConfirmTarget state whose target is the user; a failure stays in the
// open dialog.
export function ResetPasswordDialog({ reset }) {
  const user = reset.target
  return (
    <ConfirmDialog open={user !== null} title={user ? `Reset password of ${user.email}` : ''}
      action="Reset password"
      consequence={'They are signed out everywhere and must choose a new password at '
        + 'their next sign-in.'}
      busy={reset.busy} error={reset.error?.message} onConfirm={reset.accept}
      onCancel={reset.cancel} />
  )
}

export function ResetPasswordButton({ user, isCurrentUser, onReset }) {
  if (!user.password_login || isCurrentUser) return null
  return (
    <button onClick={() => onReset(user)} data-testid="user-reset-password-button"
      title="Sign the user out everywhere and issue a temporary password"
      className="px-2 py-1 rounded text-xs" style={{ color: 'var(--accent)' }}>
      Reset password
    </button>
  )
}

export function TempPasswordNotice({ reset, onDismiss }) {
  const [copied, setCopied] = useState(false)
  if (!reset) return null

  async function handleCopy() {
    try {
      await navigator.clipboard.writeText(reset.password)
      setCopied(true)
    } catch (err) {
      if (!(err instanceof Error)) throw err
      setCopied(false)
    }
  }

  return (
    <div data-testid="temp-password-notice" className="text-sm p-3 rounded mb-4"
      style={{ background: 'var(--bg-card)', border: '1px solid var(--border)',
        color: 'var(--text-primary)' }}>
      <div className="mb-1">
        Temporary password for <strong>{reset.email}</strong>. They were signed out
        everywhere and must choose a new password at their next sign-in. Send it over a
        trusted channel; it is not shown again.
      </div>
      <code data-testid="temp-password-value" className="block break-all text-xs p-2 rounded"
        style={{ background: 'var(--bg-main)' }}>
        {reset.password}
      </code>
      <button onClick={handleCopy} data-testid="temp-password-copy"
        className="mt-2 mr-3 text-xs underline" style={{ color: 'var(--accent)' }}>
        {copied ? 'Copied' : 'Copy'}
      </button>
      <button onClick={onDismiss} className="mt-2 text-xs underline"
        style={{ color: 'var(--text-secondary)' }}>
        Dismiss
      </button>
    </div>
  )
}
