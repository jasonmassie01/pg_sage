import { useEffect, useState } from 'react'
import { fetchSSOStatus, hashParam, redirectTo, startSSOLink } from './users/ssoApi'

// D7: the signed-in user's account page. "Link SSO" binds an identity from
// the configured provider to this account after a provider sign-in; the
// provider's verified email must match the account email.

const cardStyle = {
  background: 'var(--bg-card)',
  border: '1px solid var(--border)',
}

function SSOSection({ status, busy, onLink }) {
  if (!status) return null
  if (status.linked) {
    return (
      <p data-testid="sso-linked-status" className="text-sm"
        style={{ color: 'var(--text-primary)' }}>
        Single sign-on is linked to <code>{status.issuer}</code>. Ask an
        administrator to unlink it.
      </p>
    )
  }
  if (!status.oauth_enabled) {
    return (
      <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
        SSO is not configured for this pg_sage install.
      </p>
    )
  }
  return (
    <div>
      <p className="text-sm mb-3" style={{ color: 'var(--text-secondary)' }}>
        Link your identity-provider account so you can sign in with SSO.
        Its verified email must match this account.
      </p>
      <button data-testid="link-sso-button" onClick={onLink} disabled={busy}
        className="px-4 py-1.5 rounded text-sm font-medium"
        style={{ background: 'var(--accent)', color: '#fff',
          opacity: busy ? 0.6 : 1 }}>
        {busy ? 'Redirecting...' : 'Link SSO'}
      </button>
    </div>
  )
}

export function ProfilePage({ user, navigate = redirectTo }) {
  const [status, setStatus] = useState(null)
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)
  const justLinked = hashParam('linked') === '1'

  useEffect(() => {
    fetchSSOStatus().then(setStatus).catch(err => setError(err.message))
  }, [])

  async function handleLink() {
    setError(null)
    setBusy(true)
    try {
      navigate(await startSSOLink())
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  return (
    <div className="rounded-lg p-4" style={cardStyle}>
      <h2 className="text-sm font-semibold mb-1"
        style={{ color: 'var(--text-primary)' }}>
        Account
      </h2>
      <p className="text-sm mb-4" style={{ color: 'var(--text-secondary)' }}>
        <span>{user?.email}</span> ({user?.role})
      </p>
      {justLinked && status?.linked && (
        <div data-testid="sso-link-success" className="text-sm p-2 rounded mb-3"
          style={{ color: '#16a34a', border: '1px solid rgba(22,163,74,0.3)' }}>
          SSO linked.
        </div>
      )}
      {error && (
        <div className="text-sm p-2 rounded mb-3"
          style={{ color: '#ef4444', border: '1px solid rgba(239,68,68,0.3)' }}>
          {error}
        </div>
      )}
      <SSOSection status={status} busy={busy} onLink={handleLink} />
    </div>
  )
}
