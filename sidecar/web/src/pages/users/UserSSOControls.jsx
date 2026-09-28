// D7: per-user SSO status and admin actions for the Users table, plus the
// one-time link notice shown after a grant is issued.

export function UserSSOCell({ user, onUnlink, onGrant }) {
  if (user.sso_linked) {
    return (
      <div className="flex items-center gap-2">
        <span data-testid="user-sso-badge"
          title={`Linked to ${user.sso_issuer}`}
          className="px-1.5 py-0.5 rounded text-xs"
          style={{ background: 'var(--bg-main)', color: 'var(--accent)',
            border: '1px solid var(--border)' }}>
          SSO
        </span>
        <button data-testid="user-sso-unlink"
          onClick={() => onUnlink(user)}
          className="px-2 py-1 rounded text-xs"
          style={{ color: 'var(--text-secondary)' }}>
          Unlink SSO
        </button>
      </div>
    )
  }
  return (
    <div className="flex items-center gap-2">
      {!user.password_login && (
        <span className="text-xs" style={{ color: 'var(--text-secondary)' }}>
          SSO only
        </span>
      )}
      <button data-testid="user-sso-grant"
        onClick={() => onGrant(user)}
        title="Issue a one-time link (valid 15 minutes) the user opens to link SSO"
        className="px-2 py-1 rounded text-xs"
        style={{ color: 'var(--accent)' }}>
        Issue SSO link
      </button>
    </div>
  )
}

export function SSOGrantNotice({ grant, onDismiss }) {
  if (!grant) return null
  return (
    <div data-testid="sso-grant-link" className="text-sm p-3 rounded mb-4"
      style={{ background: 'var(--bg-card)', border: '1px solid var(--border)',
        color: 'var(--text-primary)' }}>
      <div className="mb-1">
        One-time SSO link for <strong>{grant.email}</strong>. It works once and
        expires at {new Date(grant.expiresAt).toLocaleString()}. Send it over a
        trusted channel; it is not shown again.
      </div>
      <code className="block break-all text-xs p-2 rounded"
        style={{ background: 'var(--bg-main)' }}>
        {grant.url}
      </code>
      <button onClick={onDismiss} className="mt-2 text-xs underline"
        style={{ color: 'var(--text-secondary)' }}>
        Dismiss
      </button>
    </div>
  )
}
