import { ChangePasswordForm } from './ChangePasswordForm'

// apiauth-03: the whole app while the session's password must change (an
// admin reset or a generated bootstrap password). The server refuses every
// other API call with password_change_required until the change succeeds.

export function ForcedPasswordChange({ user, onChanged, onLogout }) {
  return (
    <div data-testid="forced-password-change" className="min-h-screen flex items-center
      justify-center p-4" style={{ background: 'var(--bg-main)' }}>
      <div className="rounded-lg p-6 w-full max-w-md"
        style={{ background: 'var(--bg-card)', border: '1px solid var(--border)' }}>
        <h2 className="text-sm font-semibold mb-1" style={{ color: 'var(--text-primary)' }}>
          Change your password to continue
        </h2>
        <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
          {user.email} signed in with a temporary password. Choose your own before
          using pg_sage.
        </p>
        <ChangePasswordForm user={user} forced onChanged={onChanged} />
        <button data-testid="forced-password-logout" onClick={onLogout}
          className="mt-4 text-xs underline" style={{ color: 'var(--text-secondary)' }}>
          Sign out
        </button>
      </div>
    </div>
  )
}
