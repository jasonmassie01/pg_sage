import { useState } from 'react'
import { hashParam, redeemSSOLinkGrant, redirectTo } from './users/ssoApi'

// D7: landing page for an admin-issued one-time SSO link. Continuing uses
// the link once, then signs in at the identity provider; the provider's
// verified email must match the account the link was issued for.

export function LinkSSOPage({ navigate = redirectTo }) {
  const [grant] = useState(() => hashParam('grant'))
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)

  async function handleContinue() {
    setError(null)
    setBusy(true)
    try {
      navigate(await redeemSSOLinkGrant(grant))
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  return (
    <div className="min-h-screen flex items-center justify-center"
      style={{ background: 'var(--bg-main)' }}>
      <div className="w-full max-w-sm rounded-lg p-6"
        style={{ background: 'var(--bg-card)', border: '1px solid var(--border)' }}>
        <h1 className="text-lg font-semibold mb-2"
          style={{ color: 'var(--text-primary)' }}>
          Link single sign-on
        </h1>
        <p className="text-sm mb-4" style={{ color: 'var(--text-secondary)' }}>
          {grant
            ? 'An administrator issued this one-time link for your account. ' +
              'Continue to sign in with your identity provider.'
            : 'This link is missing its code. Ask an administrator for a new one.'}
        </p>
        {error && (
          <div className="text-sm p-2 rounded mb-3"
            style={{ color: '#ef4444', border: '1px solid rgba(239,68,68,0.3)' }}>
            {error}
          </div>
        )}
        <button data-testid="link-sso-grant-continue"
          onClick={handleContinue} disabled={!grant || busy}
          className="w-full px-4 py-2 rounded text-sm font-medium"
          style={{ background: 'var(--accent)', color: '#fff',
            opacity: !grant || busy ? 0.6 : 1 }}>
          {busy ? 'Redirecting...' : 'Continue with SSO'}
        </button>
      </div>
    </div>
  )
}
